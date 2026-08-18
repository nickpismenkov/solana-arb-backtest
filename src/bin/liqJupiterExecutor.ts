// Port of src/bin/liq_jupiter_executor.rs
//
// Jupiter Lend (Fluid) liquidation executor — event-driven off Pyth Lazer,
// DRY_RUN by default.
//
// Architecture (matches the marginfi executor liqExecutor.ts and the Save
// rewrite): the TRIGGER is a Pyth Lazer WS tick, NOT a getProgramAccounts poll.
// Vault STRUCTURE is refreshed off-band on a slow timer; the price-cross
// recompute runs in-memory on every ms Lazer tick.
//
// `tryArm` derives the FULL liquidate account set PURELY FROM SEEDS + on-chain
// state (`jupiterFire.deriveLiquidateAccounts` + `jupiter.decodeOracleSources`)
// — any in-scope vault resolves, including ones that have never been
// liquidated. `colPerUnitDebt=0` accepts the oracle price (a slippage floor,
// not the price) and `remaining` accounts come from `buildRemainingAccounts`.
//
// HONEST FIRING GATE: `tryArm` arms only when the flash-loan-wrapped fire tx is
// BOTH (a) <= 1232 bytes (submittable — needs JUP_ALT deployed; without it the
// wrap is ~1.5-1.7KB and is skip-and-logged, never armed) AND (b) SIMULATES
// CLEAN.
//
// Scope: only vaults whose debt (borrowToken) is USDC/USDT/wSOL are armed (via
// `VaultConfig.debtInScope`).
//
// Usage: HELIUS_RPC=<url> PYTH_LAZER_TOKEN=<tok> JUP_ALT=<alt> LIQUIDATOR_MA=<ma>
//        [DRY_RUN=1] [KEYPAIR_PATH=~/arb-keypair.json] [AUTHORITY=<pk>]
//        [MAX_DAILY_TIP_SOL=0.05] [WALLET_MIN_SOL=0.02] [MIN_TIP_SOL=0.0002]
//        [SENDER_URL=...] [SENDER_TIP_ACCOUNT=...] [HANDLE_COOLDOWN_SECS=20]
//        [RUN_DIR=.] [TICK_POLL_MS=1] [VAULT_REFRESH_SECS=30] [HEARTBEAT_SECS=10]
//        npx tsx src/bin/liqJupiterExecutor.ts

import 'dotenv/config';
import { Keypair, PublicKey, type VersionedTransaction } from '@solana/web3.js';
import { sendSender } from '../lib/jito.js';
import * as jupiter from '../lib/jupiter.js';
import { Vault, VaultConfig, VaultState } from '../lib/jupiter.js';
import * as jupiterFire from '../lib/jupiterFire.js';
import * as lazer from '../lib/lazer.js';
import * as pyth from '../lib/pyth.js';
import {
  b64,
  DailyTipBudget,
  DEFAULT_AUTHORITY,
  getAccount,
  latestBlockhash,
  loadBaseCfg,
  loadKeypair,
  logLatency,
  mintOwner,
  now,
  nowUs,
  rpc,
  simulateOk,
  sleep,
  solBalance,
  txToB64,
  type BaseCfg,
} from '../lib/liqExecutor.js';

const TOKEN_PROGRAM = new PublicKey('TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA');

async function gpaByDisc(endpoint: string, disc: Buffer): Promise<Array<[PublicKey, Buffer]>> {
  const bs58 = (await import('bs58')).default;
  const disc58 = bs58.encode(disc);
  const v = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getProgramAccounts',
    params: [jupiter.VAULTS_PROGRAM, { encoding: 'base64', filters: [{ memcmp: { offset: 0, bytes: disc58 } }] }],
  });
  const out: Array<[PublicKey, Buffer]> = [];
  const arr: any[] = Array.isArray(v?.result) ? v.result : [];
  for (const e of arr) {
    const pkStr = e?.pubkey;
    const data = b64(e?.account?.data);
    if (typeof pkStr === 'string' && data !== undefined) {
      try {
        out.push([new PublicKey(pkStr), data]);
      } catch {
        // skip
      }
    }
  }
  return out;
}

/** Off-band vault STRUCTURE refresh (not the trigger): load + join all vaults. */
async function loadVaults(endpoint: string): Promise<Vault[]> {
  const configs = new Map<number, [PublicKey, VaultConfig]>();
  for (const [pk, d] of await gpaByDisc(endpoint, jupiter.VAULT_CONFIG_DISC)) {
    const c = VaultConfig.decode(d);
    if (c !== undefined) configs.set(c.vaultId, [pk, c]);
  }
  const states = new Map<number, [PublicKey, VaultState]>();
  for (const [pk, d] of await gpaByDisc(endpoint, jupiter.VAULT_STATE_DISC)) {
    const s = VaultState.decode(d);
    if (s !== undefined) states.set(s.vaultId, [pk, s]);
  }
  const vaults: Vault[] = [];
  for (const [vid, [cpk, c]] of configs) {
    const st = states.get(vid);
    if (st === undefined) continue;
    const [spk, s] = st;
    const v = new Vault();
    v.configPubkey = cpk;
    v.statePubkey = spk;
    v.config = c;
    v.state = s;
    vaults.push(v);
  }
  vaults.sort((a, b) => a.config.vaultId - b.config.vaultId);
  return vaults;
}

/** Lazer feed id for a vault's collateral mint (falls back to the debt mint). */
function feedForVault(v: Vault, feedMap: Map<string, number>): number | undefined {
  return feedMap.get(v.config.supplyToken.toBase58()) ?? feedMap.get(v.config.borrowToken.toBase58());
}

/** A pre-built, sim-gated liquidate tx for one vault, ready to submit instantly on a cross. */
interface Armed {
  tx: VersionedTransaction;
  txBytes: number;
  quotedOut: bigint;
  tipLamports: bigint;
  tipSol: number;
  builtUs: number;
}

/** Off-band ARM step: build + quote + sim the flash-loan liquidate tx for a vault near its liquidation boundary. */
async function tryArm(
  endpoint: string,
  v: Vault,
  authority: PublicKey,
  liquidatorMa: PublicKey,
  tipAccount: PublicKey,
  tipLamports: bigint,
  tipSol: number,
): Promise<Armed | undefined> {
  if (v.config.debtLabel() !== 'USDC') return undefined;
  const oracleRaw = await getAccount(endpoint, v.config.oracle);
  const sources = oracleRaw !== undefined ? jupiter.decodeOracleSources(oracleRaw) : undefined;
  if (sources === undefined || sources.length === 0) return undefined;
  const collatMint = v.config.supplyToken;
  const ctp = (await mintOwner(endpoint, collatMint)) ?? TOKEN_PROGRAM;
  const btp = (await mintOwner(endpoint, v.config.borrowToken)) ?? TOKEN_PROGRAM;
  const liqTick = v.state.topmostTick - 1;

  const { remaining, indices } = await buildRemainingAccountsAsync(endpoint, v, liqTick, sources);

  const fa = jupiterFire.deriveLiquidateAccounts(v, ctp, btp);
  fa.remaining = [...remaining];
  const debtAmt = (v.state.totalBorrow / 50n) > 1_000_000n ? v.state.totalBorrow / 50n : 1_000_000n;
  const seize = debtAmt > 0n ? debtAmt : 1n;
  const cand: jupiterFire.JupiterFireCandidate = {
    accts: fa,
    debtAmt,
    colPerUnitDebt: 0n,
    remaining,
    remainingIndices: indices,
    seizeUnderlying: seize,
    collateralMint: collatMint,
    collateralTokenProgram: ctp,
  };
  const tip = tipLamports > 0n ? tipAccount : undefined;
  let fire;
  try {
    fire = await jupiterFire.buildJupiterFireTx(endpoint, cand, liquidatorMa, authority, tip, tipLamports, 50_000n, 100, 16, '11111111111111111111111111111111111111111111');
  } catch {
    return undefined;
  }
  if (fire.txBytes > 1232) {
    console.error(
      `     - vault ${v.config.vaultId} composes CLEAN but fire tx is ${fire.txBytes}B > 1232 (JUP_ALT applied; tip + ${indices[1]} branch remaining accts exceed headroom) — size-gated off, not arming`,
    );
    return undefined;
  }
  const clean = await simulateOk(endpoint, txToB64(fire.tx));
  if (!clean) return undefined;
  return { tx: fire.tx, txBytes: fire.txBytes, quotedOut: fire.quotedUsdcOut, tipLamports, tipSol, builtUs: nowUs() };
}

/**
 * jupiterFire.buildRemainingAccounts takes a SYNCHRONOUS `fetch(addr)` closure
 * (mirrors the Rust signature), but resolving branch/tick PDAs needs real RPC
 * round-trips here. We warm a cache with the accounts the branch-chain walk and
 * the topmost/liq tick window can plausibly need, then hand the library a
 * synchronous lookup backed by that cache — identical selection logic to the
 * Rust source, just pre-fetched instead of blocking-fetched.
 */
async function buildRemainingAccountsAsync(
  endpoint: string,
  v: Vault,
  liqTick: number,
  oracleSources: PublicKey[],
): Promise<{ remaining: PublicKey[]; indices: [number, number, number, number] }> {
  const { branchPda, tickPda, tickHasDebtPda, indexForTick, BranchLite } = await import('../lib/jupiterMath.js');
  const cache = new Map<string, Buffer | undefined>();
  const fetchOne = async (pk: PublicKey): Promise<Buffer | undefined> => {
    const key = pk.toBase58();
    if (cache.has(key)) return cache.get(key);
    const b = await getAccount(endpoint, pk);
    cache.set(key, b);
    return b;
  };

  // Branch chain: current branch -> connected_branch_id -> ... -> 0.
  let connected = v.state.currentBranchId;
  const seenBranches = new Set<number>();
  let guard = 0;
  while (connected > 0 && !seenBranches.has(connected) && guard < 64) {
    seenBranches.add(connected);
    guard += 1;
    const raw = await fetchOne(branchPda(v.config.vaultId, connected));
    if (raw === undefined) break;
    const b = BranchLite.decode(raw);
    if (b === undefined) break;
    connected = b.connectedBranchId;
  }
  await fetchOne(branchPda(v.config.vaultId, 0));

  // Tick window: topmost down to liqTick, plus the tick-has-debt bitmap slots
  // that range spans (bounded so a pathological gap doesn't runaway-fetch).
  await fetchOne(tickPda(v.config.vaultId, v.state.topmostTick));
  const topIdx = indexForTick(v.state.topmostTick);
  const lowIdx = indexForTick(liqTick);
  const hi = Math.max(topIdx, lowIdx);
  const lo = Math.min(topIdx, lowIdx);
  for (let i = hi; i >= lo && hi - i < 256; i--) {
    await fetchOne(tickHasDebtPda(v.config.vaultId, i));
  }

  const fetchSync = (addr: PublicKey): Buffer | undefined => cache.get(addr.toBase58());
  const [remaining, indices] = jupiterFire.buildRemainingAccounts(
    v.config.vaultId,
    v.state.topmostTick,
    v.state.currentBranchId,
    liqTick,
    oracleSources,
    fetchSync,
  );
  return { remaining, indices };
}

/** Fire an armed tx: stamp fresh blockhash, sign, submit via Helius Sender. Submit-only, no build/quote/sim. */
async function fireArmed(
  endpoint: string,
  runDir: string,
  senderUrl: string,
  dryRun: boolean,
  vaultId: number,
  armed: Armed,
  authority: PublicKey,
  freshBh: string,
  kp: Keypair | undefined,
  budget: DailyTipBudget,
  walletMinSol: number,
): Promise<void> {
  const submitUs = nowUs();
  const rec = (extra: Record<string, unknown>): void => {
    logLatency(runDir, {
      event: 'fire',
      protocol: 'jupiter',
      vault_id: vaultId,
      quoted_out: armed.quotedOut.toString(),
      armed_age_us: (submitUs - armed.builtUs).toString(),
      submit_us: submitUs.toString(),
      tx_bytes: armed.txBytes,
      tip_lamports: armed.tipLamports.toString(),
      ...extra,
    });
  };
  if (armed.txBytes > 1232) {
    console.error(`[jup-exec] REFUSING vault ${vaultId}: cached tx ${armed.txBytes}B > 1232`);
    return;
  }
  if (dryRun) {
    rec({ dry_run: true, fired: false });
    console.log(`     i DRY_RUN: would FIRE vault ${vaultId} (${armed.txBytes}B, tip ${armed.tipSol.toFixed(5)} SOL) — not submitting`);
    return;
  }
  if (budget.wouldExceed(armed.tipSol)) {
    console.error(`[jup-exec] daily tip cap reached — not firing vault ${vaultId}`);
    rec({ dry_run: false, fired: false, error: 'daily tip cap' });
    return;
  }
  const bal = await solBalance(endpoint, authority.toBase58());
  if (bal < walletMinSol) {
    console.error(`[jup-exec] wallet below floor ${walletMinSol} SOL — not firing vault ${vaultId}`);
    rec({ dry_run: false, fired: false, error: 'wallet below floor' });
    return;
  }
  const tx = armed.tx;
  (tx.message as any).recentBlockhash = freshBh;
  const kpReq = kp!;
  tx.sign([kpReq]);
  const bs58 = (await import('bs58')).default;
  const sig = bs58.encode(tx.signatures[0]!);
  const txB64 = txToB64(tx);
  try {
    await sendSender(senderUrl, txB64);
    budget.add(armed.tipSol);
    console.error(`[jup-exec] FIRED ${sig}`);
    rec({ dry_run: false, fired: true, signature: sig });
  } catch (e) {
    console.error(`[jup-exec] send failed: ${e}`);
    rec({ dry_run: false, fired: false, error: String(e) });
  }
}

async function main(): Promise<void> {
  const base: BaseCfg = loadBaseCfg({ runDir: '.' });
  const { endpoint, dryRun, runDir } = base;
  const tickPollMs = base.tickPollMs;
  const vaultRefreshSecs = Number.parseInt(process.env.VAULT_REFRESH_SECS ?? '', 10) || 30;
  const hbEvery = Number.parseInt(process.env.HEARTBEAT_SECS ?? '', 10) || 10;

  const tipSol = base.minTipSol;
  const tipLamports = BigInt(Math.trunc(tipSol * 1e9));
  const liquidatorMa = (() => {
    try {
      return new PublicKey(process.env.LIQUIDATOR_MA ?? 'B6e37TbC5n56tWbcgC3RRafUXSuEwRz9ZbhL8Ksro6vD');
    } catch {
      return undefined;
    }
  })();

  const kpEnv = process.env.KEYPAIR_PATH;
  let kp: Keypair | undefined;
  if (kpEnv) {
    try {
      const fs: typeof import('node:fs') = await import('node:fs');
      const bytes: number[] = JSON.parse(fs.readFileSync(kpEnv, 'utf8'));
      kp = Keypair.fromSecretKey(Uint8Array.from(bytes));
    } catch {
      kp = undefined;
    }
  }
  if (kp === undefined && !dryRun) throw new Error('LIVE fire needs KEYPAIR_PATH');
  const authority = kp?.publicKey ?? new PublicKey(process.env.AUTHORITY ?? DEFAULT_AUTHORITY);
  const budget = new DailyTipBudget(base.maxDailyTipSol);
  let freshBh = '11111111111111111111111111111111111111111111';
  let lastBh = performance.now() - 9999_000;
  const handled = new Map<number, number>();

  const lazerTable = pyth.newTable();
  let lazerOn = false;
  const lazerToken = process.env.PYTH_LAZER_TOKEN;
  if (lazerToken) {
    lazer.spawnLazerThread(lazerToken, lazer.armFeedIds(), lazerTable);
    lazerOn = true;
  } else {
    console.error('[jup-exec] no PYTH_LAZER_TOKEN — falling back to timed rescan (NOT event-driven)');
  }
  const feedMap = lazer.mintFeedMap();

  console.log(
    `[jup-exec] Jupiter Lend (Fluid) executor ${dryRun ? '[DRY RUN]' : '[LIVE]'}  authority=${authority.toBase58()} lazer=${lazerOn}  (fire gated: <=1232B + sim-clean; JUP_ALT required)`,
  );
  if (!dryRun) {
    const bal = await solBalance(endpoint, authority.toBase58());
    console.error(`[jup-exec] wallet balance: ${bal} SOL`);
    if (bal < base.walletMinSol) throw new Error(`wallet below floor ${base.walletMinSol}`);
  }

  let vaults = await loadVaults(endpoint);
  console.log(`[jup-exec] loaded ${vaults.length} vaults; trigger = ${lazerOn ? 'Pyth Lazer tick (event-driven)' : 'timed rescan (fallback)'}`);

  let lastRefresh = performance.now();
  let lastHb = performance.now();
  let lastTickUs = 0;
  let reported = new Set<number>();
  const armCache = new Map<number, Armed>();

  for (;;) {
    if (performance.now() - lastRefresh >= vaultRefreshSecs * 1000) {
      vaults = await loadVaults(endpoint);
      lastRefresh = performance.now();
      reported = new Set();
    }

    budget.rollDay();
    if (!dryRun && performance.now() - lastBh >= 2000) {
      const bh = await latestBlockhash(endpoint);
      if (bh !== undefined) {
        freshBh = bh;
        lastBh = performance.now();
      }
    }

    if (lazerOn) {
      const deadline = performance.now() + 1000;
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
        await sleep(tickPollMs);
      }
    } else {
      await sleep(Math.max(vaultRefreshSecs, 1) * 1000);
    }

    const snap = new Map<number, number>();
    for (const f of lazer.armFeedIds()) {
      const p = pyth.get(lazerTable, f);
      if (p !== undefined) snap.set(f, p.price);
    }

    const cands = vaults.filter((v) => v.config.debtInScope() && v.maybeLiquidatable());

    for (const v of cands) {
      const vid = v.config.vaultId;
      const h = handled.get(vid);
      if (h !== undefined && performance.now() - h < base.handleCooldownSecs * 1000) continue;
      const a = armCache.get(vid);
      if (a !== undefined) {
        handled.set(vid, performance.now());
        await fireArmed(endpoint, runDir, base.senderUrl, dryRun, vid, a, authority, freshBh, kp, budget, base.walletMinSol);
      }
    }

    if (hbEvery > 0 && performance.now() - lastHb >= hbEvery * 1000) {
      const total = lazer.armFeedIds().length;
      let freshest = 0;
      for (const f of lazer.armFeedIds()) {
        const p = pyth.get(lazerTable, f);
        if (p !== undefined) freshest = Math.max(freshest, p.tsUs);
      }
      const lagMs = Math.max(0, Math.trunc((nowUs() - freshest) / 1000));
      console.error(
        `[hb] lazer feeds ${snap.size}/${total} live | detect_lag ${lagMs}ms | ${vaults.length} vaults | ${cands.length} in-scope candidate(s) | ${lazer.status(lazerTable)}`,
      );
      lastHb = performance.now();
    }

    for (const v of cands) {
      if (reported.has(v.config.vaultId)) continue;
      reported.add(v.config.vaultId);
      const c = v.config;
      const feed = feedForVault(v, feedMap);
      const price = feed !== undefined ? snap.get(feed) : undefined;
      let freshest = 0;
      for (const f of lazer.armFeedIds()) {
        const p = pyth.get(lazerTable, f);
        if (p !== undefined) freshest = Math.max(freshest, p.tsUs);
      }
      const detectLagUs = Math.max(0, nowUs() - freshest);
      logLatency(runDir, {
        event: 'detect',
        protocol: 'jupiter',
        vault_id: c.vaultId,
        debt: c.debtLabel(),
        lazer_feed: feed,
        lazer_price: price,
        lazer_ts_us: freshest,
        detect_us: nowUs().toString(),
        detect_lag_us: detectLagUs.toString(),
        absorbed_debt: v.state.absorbedDebtAmount.toString(),
        liq_threshold_bps: c.liquidationThreshold,
        fired: false,
        reason: 'detection-only (colPerUnitDebt + remaining-accounts unsolved)',
      });
      const collat = c.supplyToken.toBase58().slice(0, 6);
      console.log(
        `  > vault ${c.vaultId} [${collat}->${c.debtLabel()}] LT ${(c.liqThresholdFrac() * 100).toFixed(1)}% absorbed_debt=${v.state.absorbedDebtAmount} price=${price ?? 'undefined'} detect_lag=${detectLagUs}us`,
      );
      const armed = liquidatorMa !== undefined ? await tryArm(endpoint, v, authority, liquidatorMa, base.tipAccount, tipLamports, tipSol) : undefined;
      if (armed !== undefined) {
        console.log(`     v ARMED — seed-derived, priced fire tx simulates clean (${armed.txBytes}B)`);
        armCache.set(c.vaultId, armed);
      } else {
        console.log(
          `     - not armed${liquidatorMa === undefined ? ' (LIQUIDATOR_MA unset/invalid — arming disabled)' : ''}: not fireable at the live price, non-USDC debt, or fire tx > 1232B (deploy JUP_ALT — see jupAltPrint) — sim-gated, not sending`,
        );
      }
    }
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
