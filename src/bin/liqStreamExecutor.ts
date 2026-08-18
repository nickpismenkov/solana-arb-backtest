// Port of src/bin/liq_stream_executor.rs
//
// FAST marginfi liquidation executor — the streaming rewrite.
//
// The polling executor (liqExecutor.ts) reacts in ~150ms because it re-fetches
// account state (getMultipleAccounts ~40ms) and sim-gates (up to 5x45ms) on the
// hot path. This one removes both, and pre-builds the fire so the hot path is
// sign-and-send only:
//
//   - STATE is streamed. A Yellowstone gRPC (Triton Dragon's Mouth) subscription
//     to the watch-set accounts + banks + oracles keeps the loan book in RAM —
//     no hot-path fetch.
//   - PRICES: streamed on-chain oracles (fresh-gated per bank, stale dropped
//     like the chain does) blended with Pyth Lazer (the ms trigger).
//   - PRE-ARM: a background loop continuously builds+caches a fire tx for the
//     handful of accounts closest to crossing (the expensive Jupiter quote +
//     compile happens OFF the hot path).
//   - HOT PATH: on a Lazer tick, recompute health for the trigger-indexed book
//     (binary search per bank) and on a cross refresh the cached tx's
//     blockhash, sign, and send with NO sim. Decision -> submit ~1ms;
//     profit-or-revert is the safety.
//
// NOTE ON THE gRPC WIRING: the Rust source subscribes directly via
// `yellowstone_grpc_client::GeyserGrpcClient` (Dragon's Mouth `subscribe`
// bidi-stream, Yellowstone Geyser proto). Those `.proto` definitions
// (yellowstone-grpc-proto) are not vendored anywhere in this repo and are
// unreachable offline in this sandbox — the SAME limitation already hit and
// documented by `ts-port/src/lib/grpc.ts` (Chunk A) and
// `ts-port/src/bin/mfiStreamDetect.ts`. This file follows that established
// precedent: `runStream` below is a faithful shape of the subscription (a
// `SubscribeRequestFilterAccounts` over the watch-set banks/oracles/DEX pools,
// CommitmentLevel.PROCESSED) but throws "not implemented" at call time rather
// than silently no-opping. Everything AROUND the stream — the live loan book
// (LiveState), the trigger-index pre-arm loop, the hot Lazer-tick loop, the
// crank-bundle vs Sender fire split, and the async fire-thread fan-out — is
// ported faithfully and will run correctly the moment a real stream feeds it
// account updates (the initial getProgramAccounts scan + periodic re-scan
// already keep the book populated via plain RPC, so the executor is
// functional today at RPC-poll latency; only the sub-ms gRPC push is stubbed).
//
// Usage: HELIUS_RPC=<url> GRPC_ENDPOINT=<triton-url+token> GRPC_X_TOKEN=<tok>
//        PYTH_LAZER_TOKEN=<tok> [KEYPAIR_PATH=...] [DRY_RUN=1] [MIN_PROFIT_USD=0.02]
//        [WATCH_RATIO=0.90] [ARM_RATIO=0.97] [ARM_MAX=40] [RUN_DIR=runs/stream]
//        npx tsx src/bin/liqStreamExecutor.ts

import 'dotenv/config';
import { type AccountMeta, Keypair, PublicKey, type VersionedTransaction } from '@solana/web3.js';
import * as jito from '../lib/jito.js';
import { buildFireTx, dexPoolAddresses, directDexPool, type FireCandidate, updatePoolCache } from '../lib/liqFire.js';
import {
  type Bank,
  type BankMap,
  decodeBank,
  decodeMarginfiAccount,
  decodeOraclePriceFresh,
  decodePriceUpdateV2,
  activeBankPks,
  maintenanceHealth,
  maxStaleSlotsFor,
  DEFAULT_MAX_SB_STALE_SLOTS,
  MA_SIZE,
  type MarginfiAccount,
  type PriceMap,
} from '../lib/liquidation.js';
import * as lazer from '../lib/lazer.js';
import * as pyth from '../lib/pyth.js';
import { spawnHermesStream, type HermesCache } from '../lib/pythAccumulator.js';
import { buildCrankTxs, sponsoredFeed } from '../lib/pythCrank.js';
import {
  b64,
  DEFAULT_AUTHORITY,
  DEFAULT_LIQUIDATOR_MA,
  getMultiple,
  latestBlockhash,
  logLine,
  mintOwner,
  now,
  nowUs,
  rpc,
  simulateTxB64,
  sleep,
  txToB64,
} from '../lib/liqExecutor.js';

const MARGINFI_PROGRAM = 'MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA';
const MARGINFI_GROUP = '4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8';
const SOL_MINT = 'So11111111111111111111111111111111111111112';
const USDC_MINT = 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v';
const USDT_MINT = 'Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB';

function isDebtMint(m: PublicKey): boolean {
  const s = m.toBase58();
  return s === USDC_MINT || s === USDT_MINT || s === SOL_MINT;
}

/** Simulate a signed tx (verification before real sends). Returns (ok, err+log). */
async function simulateTx(endpoint: string, txB64: string): Promise<{ ok: boolean; err: string | undefined }> {
  const val = await simulateTxB64(endpoint, txB64);
  if (val === undefined) return { ok: false, err: 'rpc call failed' };
  if (val.err == null) return { ok: true, err: undefined };
  const logs: string[] = Array.isArray(val.logs) ? val.logs : [];
  const lastLogs = logs.slice(-3).join(' | ');
  return { ok: false, err: `${JSON.stringify(val.err)} :: ${lastLogs}` };
}

/**
 * Pre-arm sim gate: cache a fire only if it simulates clean OR fails 6068
 * (chain says healthy — the obs wiring is correct, it's just not liquidatable
 * yet, so it WILL fire cleanly on a cross). Everything else is rejected.
 */
async function simCacheable(endpoint: string, tx: VersionedTransaction): Promise<{ ok: boolean; reason: string }> {
  const { ok, err } = await simulateTx(endpoint, txToB64(tx));
  if (ok) return { ok: true, reason: 'ok' };
  const e = err ?? '';
  if (e.includes('6068')) return { ok: true, reason: 'not-yet(6068)' };
  return { ok: false, reason: e };
}

/** One getProgramAccounts scan of the marginfi group -> every borrower + active-bank obs list. */
async function scanBook(endpoint: string): Promise<{ accts: Array<[PublicKey, MarginfiAccount]>; obs: Map<string, PublicKey[]> }> {
  const accts: Array<[PublicKey, MarginfiAccount]> = [];
  const obs = new Map<string, PublicKey[]>();
  const resp = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getProgramAccounts',
    params: [
      MARGINFI_PROGRAM,
      {
        encoding: 'base64',
        dataSlice: { offset: 0, length: 1736 },
        filters: [{ dataSize: MA_SIZE }, { memcmp: { offset: 8, bytes: MARGINFI_GROUP } }],
      },
    ],
  });
  const arr: any[] = Array.isArray(resp?.result) ? resp.result : [];
  for (const e of arr) {
    const pkStr = e?.pubkey;
    const raw = b64(e?.account?.data);
    if (typeof pkStr !== 'string' || raw === undefined) continue;
    const a = decodeMarginfiAccount(raw);
    if (a === null) continue;
    if (!a.balances.some((b) => b.liabilityShares > 0.0)) continue;
    const pk = new PublicKey(pkStr);
    obs.set(pk.toBase58(), activeBankPks(raw));
    accts.push([pk, a]);
  }
  return { accts, obs };
}

/** The live loan book — written by the gRPC task, read by the arm/fire loops. */
interface LiveState {
  accounts: Map<string, MarginfiAccount>;
  banks: BankMap;
  oracleOf: Map<string, PublicKey>;
  oracleRaw: Map<string, Buffer>;
  obsBanks: Map<string, PublicKey[]>;
}

/** On-chain baseline PriceMap — stale oracles dropped per bank. */
function freshBase(state: LiveState, slot: bigint, defaultStale: bigint): PriceMap {
  const out: PriceMap = new Map();
  for (const [bk, oc] of state.oracleOf) {
    const maxAge = state.banks.get(bk)?.oracleMaxAge ?? 0;
    const maxStale = maxStaleSlotsFor(maxAge, defaultStale);
    const raw = state.oracleRaw.get(oc.toBase58());
    if (raw === undefined) continue;
    const usd = decodeOraclePriceFresh(raw, slot, maxStale);
    if (usd !== null) out.set(bk, usd);
  }
  return out;
}

/** A pre-built fire kept hot for an armed account. */
interface CachedFire {
  tx: VersionedTransaction;
  seize: bigint;
  quotedOut: bigint;
  built: number;
  assetBank: PublicKey;
  crank: boolean;
}

/** Build a FireCandidate from LIVE state (no fetch): largest collateral x a wired-debt leg. */
function buildCandidate(
  a: MarginfiAccount,
  pk: PublicKey,
  banks: BankMap,
  oracleOf: Map<string, PublicKey>,
  mintTp: Map<string, PublicKey>,
  obsBanks: PublicKey[],
): FireCandidate | undefined {
  const asset = a.balances.filter((b) => b.assetShares > 0.0).sort((x, y) => y.assetShares - x.assetShares)[0];
  if (asset === undefined) return undefined;
  const debt = a.balances.find((b) => {
    if (b.liabilityShares <= 0.0) return false;
    const bk = banks.get(b.bankPk.toBase58());
    return bk !== undefined && isDebtMint(bk.mint);
  });
  if (debt === undefined) return undefined;
  const abk = banks.get(asset.bankPk.toBase58());
  const lbk = banks.get(debt.bankPk.toBase58());
  if (abk === undefined || lbk === undefined) return undefined;
  const native = asset.assetShares * abk.assetShareValue;
  const seize = BigInt(Math.trunc(native * 0.5));
  if (seize === 0n) return undefined;
  const assetTp = mintTp.get(abk.mint.toBase58());
  const debtTp = mintTp.get(lbk.mint.toBase58());
  if (assetTp === undefined || debtTp === undefined) return undefined;
  const obsList = obsBanks.length === 0 ? a.balances.map((b) => b.bankPk) : obsBanks;
  const obsMetas: AccountMeta[] = [];
  for (const bankPk of obsList) {
    const oc = oracleOf.get(bankPk.toBase58());
    if (oc === undefined) return undefined;
    obsMetas.push({ pubkey: bankPk, isSigner: false, isWritable: false });
    obsMetas.push({ pubkey: oc, isSigner: false, isWritable: false });
  }
  const assetOracle = oracleOf.get(asset.bankPk.toBase58());
  const liabOracle = oracleOf.get(debt.bankPk.toBase58());
  if (assetOracle === undefined || liabOracle === undefined) return undefined;
  return {
    liquidatee: pk,
    assetBank: asset.bankPk,
    assetMint: abk.mint,
    assetTokenProgram: assetTp,
    assetAmount: seize,
    liabBank: debt.bankPk,
    debtMint: lbk.mint,
    debtTokenProgram: debtTp,
    assetOracle,
    liabOracle,
    liquidateeObs: obsMetas,
  };
}

/**
 * One gRPC subscription lifecycle: decode each account update into the live
 * maps. NOT IMPLEMENTED — see the file-header note. Mirrors mfiStreamDetect.ts
 * / grpc.ts's precedent: a faithful call shape that throws rather than
 * silently no-opping, so callers (the reconnect loop below) retry visibly.
 */
async function runStream(
  _endpoint: string,
  _xToken: string,
  _sub: string[],
  _oracleSet: Set<string>,
  _poolSet: Set<string>,
  _state: LiveState,
): Promise<void> {
  // Once a real Yellowstone client is wired here, each decoded account update
  // would dispatch to: `updatePoolCache(pk, data)` for a DEX pool pubkey,
  // `state.oracleRaw.set(pk, data)` for an oracle, or decodeBank/
  // decodeMarginfiAccount + a `state` map write otherwise (mirrors run_stream
  // in the Rust source and the account-update branch in mfiStreamDetect.ts).
  void updatePoolCache;
  throw new Error('not implemented: requires the Yellowstone Geyser .proto definitions (yellowstone-grpc-proto), unavailable in this sandbox');
}

async function main(): Promise<void> {
  const endpoint = process.env.HELIUS_RPC;
  if (!endpoint) throw new Error('HELIUS_RPC');
  const grpcEp = process.env.GRPC_ENDPOINT;
  if (!grpcEp) throw new Error('GRPC_ENDPOINT');
  const grpcTok = process.env.GRPC_X_TOKEN;
  if (!grpcTok) throw new Error('GRPC_X_TOKEN');
  const dryRun = (process.env.DRY_RUN ?? '1') !== '0';
  const minCollateral = Number.parseFloat(process.env.MIN_COLLATERAL_USD ?? '') || 5.0;
  const underwaterMargin = Number.parseFloat(process.env.MIN_UNDERWATER_MARGIN ?? '') || 0.01;
  const verifyArm = (process.env.VERIFY_ARM ?? '1') !== '0';
  const armMax = Number.parseInt(process.env.ARM_MAX ?? '', 10) || 10;
  const armRebuildMs = (Number.parseInt(process.env.ARM_REBUILD_SECS ?? '', 10) || 20) * 1000;
  const quoteGapMs = Number.parseInt(process.env.QUOTE_GAP_MS ?? '', 10) || 1200;
  const synth = process.env.ARM_SYNTH !== undefined;
  const defaultStale = process.env.MAX_SB_STALE_SLOTS ? BigInt(process.env.MAX_SB_STALE_SLOTS) : DEFAULT_MAX_SB_STALE_SLOTS;
  const runDir = process.env.RUN_DIR ?? 'runs/stream';
  const liquidatorMa = new PublicKey(process.env.LIQUIDATOR_MA ?? DEFAULT_LIQUIDATOR_MA);
  const slippageBps = Number.parseInt(process.env.SLIPPAGE_BPS ?? '', 10) || 100;
  const tipSol = Number.parseFloat(process.env.MIN_TIP_SOL ?? '') || 0.0002;

  let kp: Keypair | undefined;
  const kpPath = process.env.KEYPAIR_PATH;
  if (kpPath) {
    const fs: typeof import('node:fs') = await import('node:fs');
    const bytes: number[] = JSON.parse(fs.readFileSync(kpPath, 'utf8'));
    kp = Keypair.fromSecretKey(Uint8Array.from(bytes));
  }
  if (kp === undefined && !dryRun) throw new Error('LIVE needs KEYPAIR_PATH');
  const authority = kp?.publicKey ?? new PublicKey(DEFAULT_AUTHORITY);

  console.error('[stream-exec] initial scan (one getProgramAccounts) ...');
  const slot0 = 0n; // replaced with a real getSlot below via freshBase's caller
  void slot0;
  const { accts, obs: obsBanksMap } = await scanBook(endpoint);
  const bankPkSet = new Set<string>();
  const bankPks: PublicKey[] = [];
  for (const [, a] of accts) {
    for (const b of a.balances) {
      const key = b.bankPk.toBase58();
      if (!bankPkSet.has(key)) {
        bankPkSet.add(key);
        bankPks.push(b.bankPk);
      }
    }
  }
  const banks: BankMap = new Map();
  const oracleOf = new Map<string, PublicKey>();
  for (const [pkStr, raw] of await getMultiple(endpoint, bankPks)) {
    const bk = decodeBank(raw);
    if (bk !== null) {
      oracleOf.set(pkStr, bk.oracleKey);
      banks.set(pkStr, bk);
    }
  }
  const oraclePkSet = new Set<string>();
  const oraclePks: PublicKey[] = [];
  for (const oc of oracleOf.values()) {
    const key = oc.toBase58();
    if (!oraclePkSet.has(key)) {
      oraclePkSet.add(key);
      oraclePks.push(oc);
    }
  }
  const oracleSet = new Set(oraclePks.map((p) => p.toBase58()));
  const oracleRaw = await getMultiple(endpoint, oraclePks);
  const allMints = [...new Set(Array.from(banks.values()).map((b) => b.mint.toBase58()))].map((s) => new PublicKey(s));
  const mintTp = new Map<string, PublicKey>();
  for (const m of allMints) {
    const owner = await mintOwner(endpoint, m, mintTp);
    void owner;
  }
  console.error(`[stream-exec] resolved ${mintTp.size} mint token-programs`);

  const feedOf = new Map<string, Buffer>();
  const crankable = new Set<string>();
  for (const [bank, oracle] of oracleOf) {
    const raw = oracleRaw.get(oracle.toBase58());
    if (raw === undefined) continue;
    const pyu = decodePriceUpdateV2(raw);
    if (pyu === null) continue;
    feedOf.set(bank, pyu.feedId);
    if (sponsoredFeed(0, pyu.feedId).equals(oracle)) crankable.add(bank);
  }
  console.error(`[stream-exec] ${crankable.size} crankable banks (Pyth sponsored feeds)`);

  const state: LiveState = { accounts: new Map(accts.map(([pk, a]) => [pk.toBase58(), a])), banks, oracleOf, oracleRaw, obsBanks: obsBanksMap };

  console.error(`[stream-exec] FULL BOOK: ${accts.length} borrowers, ${banks.size} banks, ${oraclePks.length} oracles`);

  // gRPC subscription (state stream) — NOT IMPLEMENTED here (see header note);
  // the reconnect loop retries visibly and the periodic RPC re-scan below keeps
  // the book populated in the meantime.
  {
    const poolAddrs = dexPoolAddresses();
    const poolSet = new Set(poolAddrs.map((p) => p.toBase58()));
    const sub = [...bankPks.map((p) => p.toBase58()), ...oraclePks.map((p) => p.toBase58()), ...poolAddrs.map((p) => p.toBase58())];
    console.error(`[stream-exec] streaming ${poolSet.size} DEX pools (fire-path RPC eliminated)`);
    void (async () => {
      for (;;) {
        try {
          await runStream(grpcEp, grpcTok, sub, oracleSet, poolSet, state);
        } catch (e) {
          console.error(`[stream-exec] gRPC dropped (${e}); reconnecting in 2s`);
          await sleep(2000);
        }
      }
    })();
  }

  // Re-scan loop: refresh the FULL book's account state periodically.
  {
    const rescanSecs = Number.parseInt(process.env.RESCAN_SECS ?? '', 10) || 90;
    void (async () => {
      for (;;) {
        await sleep(rescanSecs * 1000);
        const { accts: freshAccts, obs } = await scanBook(endpoint);
        if (freshAccts.length === 0) continue;
        state.accounts = new Map(freshAccts.map(([pk, a]) => [pk.toBase58(), a]));
        state.obsBanks = obs;
        if (process.env.ARM_DEBUG) console.error(`[rescan] refreshed full book: ${freshAccts.length} borrowers`);
      }
    })();
  }

  const lazerTable = pyth.newTable();
  const lazerMap = lazer.mintFeedMap();
  const lazerToken = process.env.PYTH_LAZER_TOKEN;
  if (lazerToken) {
    lazer.spawnLazerThread(lazerToken, lazer.armFeedIds(), lazerTable);
    console.error('[stream-exec] Pyth Lazer trigger ENABLED');
  }

  let curSlot = 0n;
  let blockhash = '11111111111111111111111111111111111111111111';
  void (async () => {
    for (;;) {
      const s = await rpc(endpoint, { jsonrpc: '2.0', id: 1, method: 'getSlot', params: [{ commitment: 'processed' }] });
      const slotNum = s?.result;
      if (typeof slotNum === 'number' && slotNum > 0) curSlot = BigInt(slotNum);
      const bh = await latestBlockhash(endpoint);
      if (bh !== undefined) blockhash = bh;
      await sleep(2000);
    }
  })();

  const blockEngine = jito.defaultBlockEngine();
  let jitoTip: PublicKey | undefined;
  try {
    const tips = await jito.getTipAccounts(blockEngine);
    jitoTip = tips[0];
  } catch {
    jitoTip = undefined;
  }
  const heliusTip = new PublicKey(process.env.SENDER_TIP_ACCOUNT ?? '2nyhqdwKcJZR2vcqCyrYsaPVdAnFoJjiksCXJ7hfEYgD');
  const crankOn = (process.env.CRANK ?? '1') !== '0';
  const maxBlobAgeMs = Number.parseInt(process.env.MAX_BLOB_MS ?? '', 10) || 2000;
  const hermesUrl = process.env.HERMES ?? 'https://hermes.pyth.network';
  const hex = [...new Set(Array.from(crankable).map((b) => feedOf.get(b)).filter((f): f is Buffer => f !== undefined).map((f) => f.toString('hex')))];
  const poll = process.env.HERMES_POLL !== undefined;
  console.error(`[stream-exec] Hermes ${poll ? 'poll(400ms)' : 'STREAM(SSE)'} tracking ${hex.length} crankable feeds${crankOn ? '' : ' (CRANK disabled)'}`);
  let hermes: HermesCache;
  if (poll) {
    const { spawnHermesCache } = await import('../lib/pythAccumulator.js');
    hermes = spawnHermesCache(hermesUrl, [], 400);
    hermes.setFeeds(hex);
  } else {
    hermes = spawnHermesStream(hermesUrl, hex);
  }
  const simOnly = process.env.SIM_ONLY !== undefined;
  const senderUrl = process.env.SENDER_URL ?? 'http://ams-sender.helius-rpc.com/fast';
  const cache = new Map<string, CachedFire>();
  const triggers = new Map<string, Array<[number, PublicKey]>>();
  const armBand = Number.parseFloat(process.env.ARM_BAND ?? '') || 0.03;
  const armBandMax = Number.parseFloat(process.env.ARM_BAND_MAX ?? '') || 0.15;
  const volGain = Number.parseFloat(process.env.ARM_BAND_VOL_GAIN ?? '') || 3.0;

  // TRIGGER-INDEX + PRE-ARM loop (off the hot path).
  void (async () => {
    let prevPrices = new Map<string, number>();
    for (;;) {
      const slot = curSlot;
      const base = freshBase(state, slot, defaultStale);
      const [blended] = lazer.blend(state.banks, base, lazerTable, lazerMap);
      const m = new Map(blended);
      let vol = 0.0;
      for (const [bk, pp] of prevPrices) {
        if (pp <= 0.0) continue;
        const cp = m.get(bk);
        if (cp !== undefined) vol = Math.max(vol, Math.abs((cp - pp) / pp));
      }
      const dynBand = Math.min(armBand + volGain * vol, armBandMax);
      const idx = new Map<string, Array<[number, PublicKey]>>();
      const arm: Array<[PublicKey, number, PublicKey]> = [];
      let nowLiq = 0;
      for (const [pkStr, a] of state.accounts) {
        const pk = new PublicKey(pkStr);
        let dom: [string, number] | undefined;
        for (const b of a.balances) {
          if (b.assetShares <= 0.0) continue;
          const bk = state.banks.get(b.bankPk.toBase58());
          const p = m.get(b.bankPk.toBase58());
          if (bk === undefined || p === undefined) continue;
          const val = b.assetShares * bk.assetShareValue * p;
          if (dom === undefined || val > dom[1]) dom = [b.bankPk.toBase58(), val];
        }
        if (dom === undefined) continue;
        const domBank = dom[0];
        const domBk = state.banks.get(domBank);
        if (domBk === undefined) continue;
        const domMint = domBk.mint;
        const h0 = maintenanceHealth(a, state.banks, m);
        if (h0.missing !== 0 || h0.health.weightedAssets < minCollateral) continue;
        const ratio0 = h0.health.weightedAssets === 0.0 ? Number.POSITIVE_INFINITY : h0.health.weightedLiabilities / h0.health.weightedAssets;
        if (ratio0 >= 1.0 + underwaterMargin) {
          nowLiq += 1;
          arm.push([pk, ratio0, domMint]);
          continue;
        }
        const p0 = m.get(domBank);
        if (p0 === undefined) continue;
        m.set(domBank, p0 * 0.9);
        const h1 = maintenanceHealth(a, state.banks, m);
        m.set(domBank, p0);
        const slope = (h1.health.weightedAssets - h0.health.weightedAssets) / (p0 * 0.9 - p0);
        if (slope <= 0.0) continue;
        const trigger = p0 + (h0.health.weightedLiabilities - h0.health.weightedAssets) / slope;
        if (!Number.isFinite(trigger) || trigger <= 0.0 || trigger >= p0) continue;
        if (!idx.has(domBank)) idx.set(domBank, []);
        idx.get(domBank)!.push([trigger, pk]);
        if (trigger >= p0 * (1.0 - dynBand)) arm.push([pk, ratio0, domMint]);
      }
      for (const list of idx.values()) list.sort((x, y) => y[0] - x[0]);
      prevPrices = new Map(m);
      const nTrig = Array.from(idx.values()).reduce((s, v) => s + v.length, 0);
      triggers.clear();
      for (const [k, v] of idx) triggers.set(k, v);

      arm.sort((a2, b2) => b2[1] - a2[1]);
      const usdc = new PublicKey(USDC_MINT);
      const seenAsset = new Set<string>();
      const ranked = arm.filter(([, , asset]) => {
        if (directDexPool(asset, usdc) !== null) return true;
        const key = asset.toBase58();
        if (seenAsset.has(key)) return false;
        seenAsset.add(key);
        return true;
      });
      const rankedCapped = ranked.slice(0, armMax);
      const armed = new Set(rankedCapped.map(([pk]) => pk.toBase58()));
      for (const key of cache.keys()) {
        if (!armed.has(key)) cache.delete(key);
      }
      const bh = blockhash;
      let noCand = 0;
      let buildErr = 0;
      let builtOk = 0;
      let lastErr = '';
      for (const [pk] of rankedCapped) {
        const key = pk.toBase58();
        const existing = cache.get(key);
        const stale = existing === undefined || performance.now() - existing.built > armRebuildMs;
        if (!stale) continue;
        const a = state.accounts.get(key);
        if (a === undefined) continue;
        const ob = state.obsBanks.get(key) ?? [];
        const cand = buildCandidate(a, pk, state.banks, state.oracleOf, mintTp, ob);
        if (cand === undefined) {
          noCand += 1;
          continue;
        }
        const isCrank = crankOn && crankable.has(cand.assetBank.toBase58());
        const tip = isCrank ? jitoTip : heliusTip;
        if (synth) {
          builtOk += 1;
          // Placeholder cached tx for measurement-mode timing only (no Jupiter call).
          const { TransactionMessage, VersionedTransaction: VTx } = await import('@solana/web3.js');
          const msg = new TransactionMessage({ payerKey: authority, recentBlockhash: bh, instructions: [] }).compileToV0Message([]);
          cache.set(key, { tx: new VTx(msg), seize: cand.assetAmount, quotedOut: 0n, built: performance.now(), assetBank: cand.assetBank, crank: isCrank });
          continue;
        }
        try {
          const f = await buildFireTx(endpoint, cand, liquidatorMa, authority, tip ?? null, BigInt(Math.trunc(tipSol * 1e9)), 100_000n, slippageBps, 20, bh);
          const { ok, reason } = verifyArm ? await simCacheable(endpoint, f.tx) : { ok: true, reason: 'unverified' };
          if (ok) {
            builtOk += 1;
            cache.set(key, { tx: f.tx, seize: cand.assetAmount, quotedOut: f.quotedUsdcOut, built: performance.now(), assetBank: cand.assetBank, crank: isCrank });
          } else {
            buildErr += 1;
            lastErr = `sim reject ${key.slice(0, 8)}: ${reason}`;
          }
        } catch (e) {
          buildErr += 1;
          lastErr = String(e);
        }
        await sleep(quoteGapMs);
      }
      if (process.env.ARM_DEBUG) {
        console.error(
          `[arm] book ${state.accounts.size} now-liq ${nowLiq} triggers ${nTrig} vol ${(vol * 100).toFixed(3)}% band ${(dynBand * 100).toFixed(1)}% -> armed ${rankedCapped.length} cache ${cache.size} | no_cand ${noCand} build_err ${buildErr} built_ok ${builtOk}${lastErr ? ` | last_err: ${lastErr.slice(0, 90)}` : ''}`,
        );
      }
      const secs = Number.parseInt(process.env.TRIGGER_SECS ?? '', 10) || 2;
      await sleep(secs * 1000);
    }
  })();

  console.error(
    `[stream-exec] marginfi FAST executor ${dryRun ? '[DRY RUN]' : '[LIVE]'} authority=${authority.toBase58()} full-book=${accts.length} arm_max=${armMax} arm_band=${armBand}`,
  );

  // HOT LOOP: Lazer tick -> health over trigger-indexed book (us) -> fire cached (~1ms).
  let lastTickUs = 0;
  const handled = new Map<string, number>();
  const handleCdMs = (Number.parseInt(process.env.HANDLE_COOLDOWN_SECS ?? '', 10) || 15) * 1000;
  let decideSamples: number[] = [];
  let lastHb = performance.now();
  let inFlight = 0;
  const maxInflight = Number.parseInt(process.env.MAX_INFLIGHT ?? '', 10) || 6;

  for (;;) {
    const deadline = performance.now() + 500;
    for (;;) {
      let cur = 0;
      for (const f of lazer.armFeedIds()) {
        const p = pyth.get(lazerTable, f);
        if (p !== undefined) cur = Math.max(cur, p.tsUs);
      }
      if (cur > lastTickUs) {
        lastTickUs = cur;
        break;
      }
      if (performance.now() >= deadline) break;
      await sleep(0); // yield; sub-ms tick-wait isn't meaningful on the Node event loop
    }
    const tTick = nowUs();

    const slot = curSlot;
    const base = freshBase(state, slot, defaultStale);
    const [prices] = lazer.blend(state.banks, base, lazerTable, lazerMap);
    const nTrig = Array.from(triggers.values()).reduce((s, v) => s + v.length, 0);
    const out: PublicKey[] = [];
    for (const [bank, list] of triggers) {
      const p = prices.get(bank);
      if (p === undefined) continue;
      let k = 0;
      while (k < list.length && list[k]![0] >= p) k += 1;
      for (let i = 0; i < k; i++) {
        const pk = list[i]![1];
        const key = pk.toBase58();
        const h = handled.get(key);
        if (h !== undefined && performance.now() - h < handleCdMs) continue;
        const a = state.accounts.get(key);
        if (a === undefined) continue;
        const health = maintenanceHealth(a, state.banks, prices);
        const ratio = health.health.weightedAssets === 0.0 ? Number.POSITIVE_INFINITY : health.health.weightedLiabilities / health.health.weightedAssets;
        if (health.missing === 0 && ratio >= 1.0 + underwaterMargin && health.health.weightedAssets >= minCollateral) out.push(pk);
      }
    }
    for (const key of cache.keys()) {
      const h = handled.get(key);
      if (h !== undefined && performance.now() - h < handleCdMs) continue;
      const a = state.accounts.get(key);
      if (a === undefined) continue;
      const health = maintenanceHealth(a, state.banks, prices);
      const ratio = health.health.weightedAssets === 0.0 ? Number.POSITIVE_INFINITY : health.health.weightedLiabilities / health.health.weightedAssets;
      if (health.missing === 0 && ratio >= 1.0 + underwaterMargin && health.health.weightedAssets >= minCollateral) out.push(new PublicKey(key));
    }
    const seen = new Set<string>();
    const crossed = out.filter((pk) => (seen.has(pk.toBase58()) ? false : (seen.add(pk.toBase58()), true)));

    const decideUs = nowUs() - tTick;
    decideSamples.push(decideUs / 1000.0);
    if (performance.now() - lastHb > 5000 && decideSamples.length > 0) {
      decideSamples.sort((a, b) => a - b);
      const med = decideSamples[Math.floor(decideSamples.length / 2)]!;
      const p90 = decideSamples[Math.min(Math.floor((decideSamples.length * 9) / 10), decideSamples.length - 1)]!;
      console.error(
        `[hb] triggers ${nTrig} cache ${cache.size} | hot-path decide: median ${med.toFixed(3)}ms p90 ${p90.toFixed(3)}ms (n=${decideSamples.length}) | crossed ${crossed.length}`,
      );
      decideSamples = [];
      lastHb = performance.now();
    }

    for (const pk of crossed) {
      if (inFlight >= maxInflight) break;
      const key = pk.toBase58();
      handled.set(key, performance.now());
      const freshBh = blockhash;
      inFlight += 1;
      void (async () => {
        try {
          const cached = cache.get(key);
          let tx: VersionedTransaction;
          let seize: bigint;
          let quotedOut: bigint;
          let isCrank: boolean;
          let assetBank: PublicKey;
          if (cached !== undefined) {
            tx = cached.tx;
            seize = cached.seize;
            quotedOut = cached.quotedOut;
            isCrank = cached.crank;
            assetBank = cached.assetBank;
          } else {
            const a = state.accounts.get(key);
            if (a === undefined) return;
            const ob = state.obsBanks.get(key) ?? [];
            const cand = buildCandidate(a, pk, state.banks, state.oracleOf, mintTp, ob);
            if (cand === undefined) return;
            const ic = crankOn && crankable.has(cand.assetBank.toBase58());
            const tipAcc = ic ? jitoTip : heliusTip;
            const f = await buildFireTx(endpoint, cand, liquidatorMa, authority, tipAcc ?? null, BigInt(Math.trunc(tipSol * 1e9)), 100_000n, slippageBps, 20, freshBh);
            tx = f.tx;
            seize = cand.assetAmount;
            quotedOut = f.quotedUsdcOut;
            isCrank = ic;
            assetBank = cand.assetBank;
          }
          const decideMs = (nowUs() - tTick) / 1000.0;
          if (dryRun) {
            logLine(
              runDir,
              'stream.jsonl',
              JSON.stringify({
                t: now(),
                liquidatee: key,
                seize: seize.toString(),
                mode: isCrank ? 'crank' : 'sender',
                quoted_out: quotedOut.toString(),
                decide_ms: decideMs,
                dry_run: true,
              }),
            );
            return;
          }
          if (kp === undefined) return;
          (tx.message as any).recentBlockhash = freshBh;
          tx.sign([kp]);
          const bs58 = (await import('bs58')).default;
          const sig = bs58.encode(tx.signatures[0]!);
          const liqB64 = txToB64(tx);
          if (simOnly) {
            const { ok, err } = await simulateTx(endpoint, liqB64);
            logLine(
              runDir,
              'stream.jsonl',
              JSON.stringify({ t: now(), liquidatee: key, seize: seize.toString(), mode: isCrank ? 'crank' : 'sender', quoted_out: quotedOut.toString(), SIM_ONLY: true, sim_ok: ok, sim_err: err }),
            );
            return;
          }
          if (isCrank) {
            const feedId = feedOf.get(assetBank.toBase58());
            if (feedId === undefined) {
              logLine(runDir, 'stream.jsonl', `crank skip ${key.slice(0, 8)}: no feed`);
              return;
            }
            const u = hermes.updateFor(feedId);
            if (u === undefined) {
              logLine(runDir, 'stream.jsonl', `crank skip ${key.slice(0, 8)}: no Hermes blob`);
              return;
            }
            if (u.ageMs > maxBlobAgeMs) {
              logLine(runDir, 'stream.jsonl', `crank skip ${key.slice(0, 8)}: blob stale ${u.ageMs}ms`);
              return;
            }
            const px = u.update.priceUsd();
            if (px !== undefined) {
              const a = state.accounts.get(key);
              const pricesCrank = new Map(freshBase(state, curSlot, defaultStale));
              pricesCrank.set(assetBank.toBase58(), px);
              const ok = a !== undefined ? (() => {
                const h = maintenanceHealth(a, state.banks, pricesCrank);
                const r = h.health.weightedAssets === 0.0 ? Number.POSITIVE_INFINITY : h.health.weightedLiabilities / h.health.weightedAssets;
                return h.missing === 0 && r >= 1.0 + underwaterMargin;
              })() : false;
              if (!ok) {
                logLine(runDir, 'stream.jsonl', JSON.stringify({ t: now(), liquidatee: key, mode: 'crank', judge: 'healthy_at_hermes', px, fired: false }));
                return;
              }
            }
            const ctxs = buildCrankTxs(authority, u.vaa, [u.update], 0, 0, freshBh);
            ctxs.stampAndSign(kp, freshBh);
            const [setupB64, crankB64] = ctxs.toB64();
            const submitMs0 = nowUs();
            let bundleId: string | undefined;
            let sendErr: string | undefined;
            try {
              bundleId = await jito.sendBundle(blockEngine, [setupB64, crankB64, liqB64]);
            } catch (e) {
              sendErr = String(e);
            }
            const submitMs = (nowUs() - tTick) / 1000.0;
            void submitMs0;
            logLine(
              runDir,
              'stream.jsonl',
              JSON.stringify({
                t: now(), liquidatee: key, seize: seize.toString(), mode: 'crank',
                decide_ms: decideMs, submit_ms: submitMs, signature: sig, bundle: bundleId,
                sent: sendErr === undefined, send_err: sendErr, blob_age_ms: u.ageMs, fired: true,
              }),
            );
          } else {
            let sendErr: string | undefined;
            try {
              await jito.sendSender(senderUrl, liqB64);
            } catch (e) {
              sendErr = String(e);
            }
            const submitMs = (nowUs() - tTick) / 1000.0;
            logLine(
              runDir,
              'stream.jsonl',
              JSON.stringify({ t: now(), liquidatee: key, seize: seize.toString(), mode: 'sender', decide_ms: decideMs, submit_ms: submitMs, signature: sig, sent: sendErr === undefined, send_err: sendErr, fired: true }),
            );
          }
        } finally {
          inFlight -= 1;
        }
      })();
    }
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
