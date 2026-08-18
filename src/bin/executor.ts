// Port of src/main.rs (executor.rs)
//
// Arb executor v2 (pragmatic fast reactor, blind-guarded fire). Hot path is
// memory reads + sign + submit ONLY — no RPC, no disk, no network calls in
// the reaction. Slow work on background threads:
//   - RPC poll (~10s) → PoolData cache (pool accounts for building)
//   - RPC poll (~20s) → recent blockhash
//   - config hot-reload (~3s) → Arc<RwLock<Config>> (pause / size / tip)
//   - log writer thread ← mpsc channel (decisions/trades JSONL, off hot path)
//   - realized-P&L readback (detached, later)
//
// On a shred trigger (not paused): build guarded arb from cached state +
// blockhash, sign, submit to Jito. The exact-out leg-2 guard is the real
// profitability check — unprofitable txs revert for free, tips only pay on
// wins. No price filtering; every trigger fires unless paused/dry_run.
// DRY_RUN=1 (default) logs and never submits.
//
// Env: RPC_ENDPOINT, ALT_ADDRESS, KEYPAIR_PATH, SHREDSTREAM_PORT, RUN_DIR,
//      DRY_RUN, CONFIG_PATH, JITO_BLOCK_ENGINE, WALLET_MIN_SOL,
//      MAX_DAILY_TIP_SOL, ALERT_WEBHOOK.

import 'dotenv/config';
import { readFileSync } from 'node:fs';
import bs58 from 'bs58';
import { AddressLookupTableAccount, Keypair, PublicKey } from '@solana/web3.js';
import { buildArbTx, loadAlt, type PoolData } from '../lib/arb.js';
import { afterBaseSwap, clmmFromOrca, clmmFromRay, optimalArb, uiPrice, wsol, type ClmmState } from '../lib/clmm.js';
import { Dir } from '../lib/decode.js';
import { sendSender } from '../lib/jito.js';
import { alert, logDecision, logTrade, realizedUsdc } from '../lib/observe.js';
import { pair } from '../lib/pools.js';
import { runShredstreamFeed, type Trigger } from '../lib/shredstream.js';

interface Config {
  paused: boolean;
  borrowUsdc: number;
  priorityMicroLamports: bigint;
  /** Tip as a fraction of computed profit (bps). Jito's auction is won by
   * paying a fraction of profit; capped at 80% so we always net positive. */
  tipFractionBps: bigint;
  /** Minimum computed profit (lamports) to fire. Must clear tip + fees. */
  minProfitLamports: bigint;
}

function defaultConfig(): Config {
  return {
    paused: false,
    borrowUsdc: 500.0,
    priorityMicroLamports: 10_000n,
    tipFractionBps: 3000n, // 30% of computed profit
    minProfitLamports: 500_000n, // 0.0005 SOL; must clear Sender's 0.0002 tip floor + fees + buffer
  };
}
const SENDER_MIN_TIP = 200_000; // Helius Sender requires ≥0.0002 SOL tip
// SOL/USDC Orca pool (SOL=mintA/dec9, USDC=mintB/dec6) — independent SOL price
// reference for USDC→SOL tip conversion, regardless of the traded pair.
const SOL_USDC_REF = 'Czfq3xZZDmsdGdUyrNLtRhGc47cXcZtLG4crryfu44zE';

interface DecisionLog {
  t: number;
  venue: string;
  slot: number;
  fired: boolean;
  reason: string;
}
interface TradeLog {
  t: number;
  borrow_usdc: number;
  tip_lamports: number;
  bundle_id: string | null;
  signature: string | null;
  bundle_status: string | null;
  realized_usdc: number | null;
  error: string | null;
}

type LogMsg = { kind: 'decision'; row: DecisionLog } | { kind: 'trade'; row: TradeLog };

function now(): number {
  return Math.floor(Date.now() / 1000);
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function rpc(endpoint: string, body: unknown): Promise<any | undefined> {
  for (let attempt = 0; attempt < 3; attempt++) {
    try {
      const resp = await fetch(endpoint, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify(body),
      });
      return await resp.json();
    } catch {
      // fall through to retry
    }
    await sleep(300 << attempt);
  }
  return undefined;
}

async function accountData(endpoint: string, addr: string): Promise<Buffer | undefined> {
  const v = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getAccountInfo',
    params: [addr, { encoding: 'base64' }],
  });
  const s = v?.result?.value?.data?.[0];
  if (typeof s !== 'string') return undefined;
  try {
    return Buffer.from(s, 'base64');
  } catch {
    return undefined;
  }
}

async function latestBlockhash(endpoint: string): Promise<string | undefined> {
  const v = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getLatestBlockhash',
    params: [{ commitment: 'confirmed' }],
  });
  const bh = v?.result?.value?.blockhash;
  return typeof bh === 'string' ? bh : undefined;
}

async function solBalance(endpoint: string, pk: string): Promise<number> {
  const v = await rpc(endpoint, { jsonrpc: '2.0', id: 1, method: 'getBalance', params: [pk] });
  const lamports = v?.result?.value;
  return (typeof lamports === 'number' ? lamports : 0) / 1e9;
}

function loadConfig(path: string): Config {
  try {
    const s = readFileSync(path, 'utf8');
    const parsed = JSON.parse(s);
    const d = defaultConfig();
    return {
      paused: typeof parsed?.paused === 'boolean' ? parsed.paused : d.paused,
      borrowUsdc: typeof parsed?.borrow_usdc === 'number' ? parsed.borrow_usdc : d.borrowUsdc,
      priorityMicroLamports:
        typeof parsed?.priority_micro_lamports === 'number' ? BigInt(parsed.priority_micro_lamports) : d.priorityMicroLamports,
      tipFractionBps: typeof parsed?.tip_fraction_bps === 'number' ? BigInt(parsed.tip_fraction_bps) : d.tipFractionBps,
      minProfitLamports: typeof parsed?.min_profit_lamports === 'number' ? BigInt(parsed.min_profit_lamports) : d.minProfitLamports,
    };
  } catch {
    return defaultConfig();
  }
}

async function main(): Promise<void> {
  const endpoint = process.env.RPC_ENDPOINT;
  if (endpoint === undefined) throw new Error('RPC_ENDPOINT');
  const altAddr = process.env.ALT_ADDRESS;
  if (altAddr === undefined) throw new Error('ALT_ADDRESS');
  const port = Number.parseInt(process.env.SHREDSTREAM_PORT ?? '', 10) || 20000;
  const runDir = process.env.RUN_DIR ?? 'runs';
  const dryRun = process.env.DRY_RUN !== undefined ? process.env.DRY_RUN !== '0' : true;
  const configPath = process.env.CONFIG_PATH ?? 'arb.config.json';
  // Helius Sender: fast dual-route landing (validators + Jito), no 1/sec cap.
  const senderUrl = process.env.SENDER_URL ?? 'http://ams-sender.helius-rpc.com/fast';
  const paceMs = Number.parseInt(process.env.PACE_MS ?? '', 10) || 250;
  const walletMinSol = Number.parseFloat(process.env.WALLET_MIN_SOL ?? '') || 0.02;
  const maxDailyTipSol = Number.parseFloat(process.env.MAX_DAILY_TIP_SOL ?? '') || 0.05;
  const webhook = process.env.ALERT_WEBHOOK;
  const cfg = pair();

  const kp = process.env.KEYPAIR_PATH !== undefined ? loadKeypair(process.env.KEYPAIR_PATH) : undefined;
  if (kp === undefined && !dryRun) throw new Error('LIVE needs KEYPAIR_PATH');
  const signer = kp !== undefined ? kp.publicKey : new PublicKey('Anu6Awu4kxaEDrg1nkpcikx6tJ2xhfVci5TvDrZBsZEB');

  // Static, one-time: ALT + tip account.
  const altData = await accountData(endpoint, altAddr);
  if (altData === undefined) throw new Error('ALT');
  const alt: AddressLookupTableAccount = loadAlt(altAddr, altData);
  // Helius Sender requires the tip go to one of ITS wallets (not a Jito tip
  // account). Overridable via SENDER_TIP_ACCOUNT.
  const tipAccount: PublicKey | undefined = (() => {
    const s = process.env.SENDER_TIP_ACCOUNT;
    if (s !== undefined) {
      try {
        return new PublicKey(s);
      } catch {
        // fall through to default
      }
    }
    return new PublicKey('2nyhqdwKcJZR2vcqCyrYsaPVdAnFoJjiksCXJ7hfEYgD');
  })();

  // Shared caches.
  let pooldata: PoolData | undefined;
  let blockhash = '';
  let config = loadConfig(configPath);
  // SOL/USDC reference price (USDC per SOL) for converting USDC profit → SOL
  // tip. When the traded base ISN'T SOL (e.g. SPYx), the trading pool's price
  // is the wrong denominator; we always convert via this independent SOL feed.
  let solUsd = 0.0;

  // Seed pool data + blockhash + SOL price before starting.
  {
    const [o, r] = await Promise.all([accountData(endpoint, cfg.orcaPool), accountData(endpoint, cfg.rayPool)]);
    if (o !== undefined && r !== undefined) pooldata = { orca: o, ray: r };
  }
  {
    const bh = await latestBlockhash(endpoint);
    if (bh !== undefined) blockhash = bh;
  }
  {
    const d = await accountData(endpoint, SOL_USDC_REF);
    if (d !== undefined) {
      const s = clmmFromOrca(d, 9, 6, 4.0);
      if (s !== undefined) solUsd = uiPrice(s);
    }
  }

  console.error(
    `executor v2 ${dryRun ? '[DRY RUN]' : '[LIVE]'} pair=${cfg.label} alt=${altAddr.slice(0, 8)} wallet=${signer.toBase58()} dry_run=${dryRun} — hot path: blind-guarded fire`,
  );
  if (!dryRun) {
    const bal = await solBalance(endpoint, signer.toBase58());
    console.error(`wallet balance: ${bal} SOL`);
    if (bal < walletMinSol) throw new Error(`wallet below floor ${walletMinSol}`);
  }

  // ── background: pool data (12s) + blockhash (3s) refresh ──
  // Blockhash refreshes frequently because Jito rejects expired blockhashes.
  // Falls back to a secondary RPC if the primary fails (the shredstream
  // feed's ALT fetches share the primary and can rate-limit it).
  {
    const fb = process.env.RPC_FALLBACK ?? 'https://api.mainnet-beta.solana.com';
    const { orcaPool: op, rayPool: rp } = cfg;
    // Pool state drives the profit prediction, so keep it as fresh as the
    // RPC allows (POOL_POLL_MS, default 1s). A stale snapshot quantises the
    // predicted profit to the refresh cycle — visible as identical profits
    // across distinct victims. Blockhash only needs ~every few seconds.
    const pollMs = Number.parseInt(process.env.POOL_POLL_MS ?? '', 10) || 1000;
    void (async () => {
      let tick = 0;
      let bhFails = 0;
      for (;;) {
        await sleep(pollMs);
        // Pool state EVERY tick (freshness matters most).
        const [o, r] = await Promise.all([
          accountData(endpoint, op).then((v) => v ?? accountData(fb, op)),
          accountData(endpoint, rp).then((v) => v ?? accountData(fb, rp)),
        ]);
        if (o !== undefined && r !== undefined) {
          pooldata = { orca: o, ray: r };
        } else {
          console.error('[warn] pool data refresh failed on both endpoints');
        }
        // Blockhash + SOL/USDC reference price roughly every 3s.
        tick += 1;
        if (tick % Math.max(1, Math.trunc(3000 / pollMs)) === 0) {
          const bh = (await latestBlockhash(endpoint)) ?? (await latestBlockhash(fb));
          if (bh !== undefined) {
            bhFails = 0;
            blockhash = bh;
          } else {
            bhFails += 1;
            console.error(`[warn] blockhash refresh failed on BOTH endpoints (${bhFails} in a row)`);
          }
          const d = (await accountData(endpoint, SOL_USDC_REF)) ?? (await accountData(fb, SOL_USDC_REF));
          if (d !== undefined) {
            const s = clmmFromOrca(d, 9, 6, 4.0);
            if (s !== undefined) solUsd = uiPrice(s);
          }
        }
      }
    })();
  }

  // ── background: config hot-reload (3s) ──
  void (async () => {
    for (;;) {
      await sleep(3000);
      config = loadConfig(configPath);
    }
  })();

  // ── background: log writer (channel → JSONL), OFF hot path ──
  const logQueue: LogMsg[] = [];
  let logWake: (() => void) | undefined;
  const sendLog = (msg: LogMsg): void => {
    logQueue.push(msg);
    if (logWake) {
      const w = logWake;
      logWake = undefined;
      w();
    }
  };
  void (async () => {
    for (;;) {
      if (logQueue.length === 0) {
        await new Promise<void>((resolve) => {
          logWake = resolve;
        });
        continue;
      }
      const msg = logQueue.shift()!;
      if (msg.kind === 'decision') logDecision(runDir, msg.row);
      else logTrade(runDir, msg.row);
    }
  })();

  // ── shred trigger feed → queue bridge ──
  const trigQueue: Trigger[] = [];
  let trigWake: (() => void) | undefined;
  runShredstreamFeed(port, endpoint, (t) => {
    trigQueue.push(t);
    if (trigWake) {
      const w = trigWake;
      trigWake = undefined;
      w();
    }
  });
  const nextTrigger = (): Promise<Trigger> =>
    new Promise((resolve) => {
      if (trigQueue.length > 0) {
        resolve(trigQueue.shift()!);
        return;
      }
      trigWake = () => resolve(trigQueue.shift()!);
    });

  let dailyTipSol = 0.0;
  let triggers = 0;
  let fired = 0;

  const base = wsol();
  const seenSigs = new Set<string>();
  // Jito's unauthenticated lane hard-limits to 1 bundle/sec — firing faster
  // just 429s. Pace to ~1/sec (an auth key would lift this).
  let lastSubmit = Date.now() - 10_000;
  // ═══ HOT PATH ═══ decode victim → predict exact profit → gate → co-bundle.
  // All arithmetic on cached state; the only network call is the Jito submit.
  for (;;) {
    const trigger = await nextTrigger();
    triggers += 1;
    const c = config;
    if (c.paused) continue;

    // Only co-bundle DECODABLE direct victims. Routed/CPI swaps decode to
    // empty (we can't predict their pool effect) → skip silently (logging
    // every such trigger would flood the ledger; they're the majority).
    const victim = trigger.swaps.find((s) => s.amountIsInput && s.amount > 0n);
    if (victim === undefined) continue;

    const bh = blockhash;
    const pd = pooldata;
    if (pd === undefined) continue;
    // Decode both pools. Orca decimals from mintA (offset 101); Ray self-describes.
    let orcaMintA: PublicKey | undefined;
    try {
      orcaMintA = new PublicKey(pd.orca.subarray(101, 133));
    } catch {
      orcaMintA = undefined;
    }
    const [oda, odb] = orcaMintA !== undefined && orcaMintA.equals(base) ? [cfg.baseDec, cfg.quoteDec] : [cfg.quoteDec, cfg.baseDec];
    const orca0 = clmmFromOrca(pd.orca, oda, odb, cfg.orcaFeeBps);
    const ray0 = clmmFromRay(pd.ray, cfg.rayFeeBps);
    if (orca0 === undefined || ray0 === undefined) continue;

    // Apply the victim's swap to the pool it hits → predicted post-victim state.
    const sellBase = victim.dir === Dir.SellBase;
    const amt = Number(victim.amount);
    let orcaP: ClmmState;
    let rayP: ClmmState;
    if (victim.venue === 'Orca') {
      orcaP = afterBaseSwap(orca0, base, sellBase, amt);
      rayP = ray0;
    } else {
      orcaP = orca0;
      rayP = afterBaseSwap(ray0, base, sellBase, amt);
    }
    // Exact optimal arb over the predicted state (borrow capped by config).
    const [sizeRaw, profitRaw, buyOrca] = optimalArb(orcaP, rayP, base, c.borrowUsdc * 1e6);
    // Convert USDC profit → SOL lamports via the independent SOL/USDC price
    // (NOT the trading pool's price — wrong denominator when base ≠ SOL).
    const solPrice = solUsd; // USDC per SOL
    const profitLamports = solPrice > 0.0 ? (profitRaw / 1e6 / solPrice) * 1e9 : 0.0;

    // GATE: fire only genuinely profitable arbs (clears tip + fees).
    const fire = profitLamports > Number(c.minProfitLamports) && sizeRaw > 1_000_000.0;
    sendLog({
      kind: 'decision',
      row: {
        t: now(),
        venue: trigger.venue,
        slot: trigger.slot,
        fired: fire && !dryRun,
        reason: fire ? 'profitable' : 'below_threshold',
      },
    });
    if (!fire) continue;

    const dir = buyOrca ? 'orca→ray' : 'ray→orca';
    // Tip ≤ 50% of profit (leaves margin). The repay_buffer forces leg2 to
    // yield borrow + tip + fees in USDC, so a landed trade is net-positive
    // even if the prediction is optimistic; too-small gaps revert for free.
    const tip = BigInt(
      Math.trunc(
        Math.min(Math.max(profitLamports * (Number(c.tipFractionBps) / 1e4), SENDER_MIN_TIP), profitLamports * 0.8),
      ),
    );
    const FEE_LAMPORTS = 20_000.0; // tx + priority + cushion
    const repayBuffer = solPrice > 0.0 ? BigInt(Math.trunc(((Number(tip) + FEE_LAMPORTS) / 1e9) * solPrice * 1e6 * 1.05)) : 0n;
    const borrowAmount = BigInt(Math.trunc(sizeRaw));

    // Dedup: the same victim tx can arrive multiple times (retransmits);
    // fire it at most once (a duplicate bundle would fail anyway).
    if (seenSigs.has(trigger.sig)) continue;
    seenSigs.add(trigger.sig);
    if (seenSigs.size > 5000) seenSigs.clear();

    if (dryRun) {
      console.error(
        `[dry] would co-bundle ${dir} borrow=${(Number(borrowAmount) / 1e6).toFixed(1)}USDC profit=${(profitLamports / 1e9).toFixed(6)}SOL tip=${tip} buffer=${(Number(repayBuffer) / 1e6).toFixed(3)}USDC (victim ${victim.venue} ${sellBase ? 'sellBase' : 'buyBase'} ${amt.toFixed(1)})`,
      );
      continue;
    }

    // Daily tip cap: CHECK only (don't pre-charge). Tips are paid only on a
    // landed bundle, so we count actual spend after acceptance, not per
    // attempt — otherwise non-landing fires falsely exhaust the cap.
    if (dailyTipSol + Number(tip) / 1e9 > maxDailyTipSol) {
      alert(webhook, 'daily_cap', 'daily tip cap reached');
      continue;
    }
    // Pace submissions (PACE_MS; Sender lifts the Jito 1/sec cap so this can
    // be small). Skip if we submitted too recently.
    if (Date.now() - lastSubmit < paceMs) continue;
    if (kp === undefined) continue;
    let tx;
    try {
      tx = buildArbTx(pd, signer, alt, borrowAmount, buyOrca, tipAccount, tip, c.priorityMicroLamports, bh, repayBuffer);
    } catch {
      continue;
    }

    tx.sign([kp]);
    const sig = bs58.encode(tx.signatures[0]);
    const arbB64 = Buffer.from(tx.serialize()).toString('base64');

    fired += 1;
    console.error(
      `[debug] BACKRUN ${dir} borrow=${(Number(borrowAmount) / 1e6).toFixed(1)}USDC profit=${(profitLamports / 1e9).toFixed(6)}SOL tip=${tip} slot=${trigger.slot} sig=${sig.slice(0, 16)}`,
    );
    // Submit our arb ALONE (not [victim, arb]): the victim is already
    // propagating to land via its own path (shred = already broadcasting),
    // so bundling it → "already processed". The victim's landing creates the
    // gap on-chain; our guarded arb bundle races to capture it. Guard reverts
    // free if the gap is already gone.
    lastSubmit = Date.now();
    try {
      const returnedSig = await sendSender(senderUrl, arbB64);
      sendLog({
        kind: 'trade',
        row: {
          t: now(),
          borrow_usdc: Number(borrowAmount) / 1e6,
          tip_lamports: Number(tip),
          bundle_id: null,
          signature: sig,
          bundle_status: null,
          realized_usdc: null,
          error: null,
        },
      });
      console.error(`⚡ backrun ${dir} sent ${returnedSig.slice(0, 16)}`);
      const ownerStr = signer.toBase58();
      const sCopy = sig;
      const borrowUi = Number(borrowAmount) / 1e6;
      const tipCopy = tip;
      void (async () => {
        // Landing truth = the tx on-chain (getTransaction via realized_usdc);
        // Sender returns a signature, not a Jito bundle id, so poll the chain.
        let pnl: number | undefined;
        for (const delay of [4, 8, 20]) {
          await sleep(delay * 1000);
          pnl = await realizedUsdc(endpoint, sCopy, ownerStr);
          if (pnl !== undefined) break;
        }
        // Count the tip against the daily cap ONLY on a confirmed landing
        // (accepted-but-dropped pays no tip).
        if (pnl !== undefined) dailyTipSol += Number(tipCopy) / 1e9;
        console.error(`[readback] ${sCopy.slice(0, 8)}… landed=${pnl !== undefined} realized_usdc=${pnl ?? 'undefined'}`);
        sendLog({
          kind: 'trade',
          row: {
            t: now(),
            borrow_usdc: borrowUi,
            tip_lamports: Number(tipCopy),
            bundle_id: null,
            signature: sCopy,
            bundle_status: null,
            realized_usdc: pnl ?? null,
            error: null,
          },
        });
      })();
    } catch (e) {
      const errStr = String(e);
      console.error(`[debug] submit error (${dir}): ${errStr.slice(0, 400)}`);
      sendLog({
        kind: 'trade',
        row: {
          t: now(),
          borrow_usdc: Number(borrowAmount) / 1e6,
          tip_lamports: Number(tip),
          bundle_id: null,
          signature: null,
          bundle_status: null,
          realized_usdc: null,
          error: errStr,
        },
      });
    }

    if (triggers % 100 === 0) console.error(`[executor] triggers=${triggers} fired=${fired}`);
  }
}

function loadKeypair(path: string): Keypair {
  const bytes: number[] = JSON.parse(readFileSync(path, 'utf8'));
  return Keypair.fromSecretKey(Uint8Array.from(bytes));
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
