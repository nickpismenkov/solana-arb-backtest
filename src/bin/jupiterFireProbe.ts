// Port of src/bin/jupiter_fire_probe.rs
//
// Verify the Jupiter Lend (Fluid) liquidate reversal end-to-end. Three stages,
// all read-only (never submits):
//
// 1. GROUND-TRUTH PDA CHECK — pull recent real `liquidate` txs off the Vaults
//    program, split each `remaining` list by `remaining_accounts_indices`
//    ([sources, branches, ticks, tick_has_debt]), read every branch/tick/
//    tick_has_debt account, decode its id, and re-derive the PDA from our seeds.
//    A 100% match proves the seed + layout reversal against real liquidators.
//
// 2+3. LIVE SELECTION + SIM-VERIFY — for each vault with a recent liquidate (so
//    its Liquidity PDAs + oracle sources are liftable), derive the
//    `remaining_accounts` FRESH from current on-chain state via
//    `build_remaining_accounts`, build a `liquidate` ix (captured liquidator-side
//    accounts + our fresh remaining, col_per_unit_debt = 0), and
//    simulateTransaction (sigVerify=false, replaceRecentBlockhash=true). Success
//    bar: a CLEAN sim, or a revert at the protocol's OWN liquidation gate
//    (VaultInvalidLiquidation etc.) — either proves every upstream leg composes
//    (oracle CPI via sources, exchange prices, branch/tick/tick_has_debt wiring).
//
// 4. FULL FIRE TX — for a USDC-debt vault, build the marginfi-flash-loan-wrapped
//    liquidate+swap+repay tx and report composition + byte size (a single-packet
//    fire needs a deployment ALT, like Save's SAVE_ALT).
//
// Usage: HELIUS_RPC=<url> [SCAN_SIGS=1000] [SIM_VAULT=<id>] tsx src/bin/jupiterFireProbe.ts

import 'dotenv/config';
import bs58 from 'bs58';
import { PublicKey, TransactionMessage, VersionedTransaction } from '@solana/web3.js';
import { decodeOracleSources, VAULT_CONFIG_DISC, VAULTS_PROGRAM, Vault, VaultConfig, VaultState } from '../lib/jupiter.js';
import {
  accountsFromCaptured,
  ADDRESS_DEAD,
  buildJupiterFireTx,
  buildLiquidateIx,
  buildRemainingAccounts,
  deriveLiquidateAccounts,
  LIQUIDATE_DISC,
  setLiquidatorSide,
  type JupiterFireCandidate,
  type JupiterFireTx,
  type LiquidateAccounts,
} from '../lib/jupiterFire.js';
import { ataFor } from '../lib/flashloan.js';
import { BranchLite, branchPda, findNextTickWithDebt, indexForTick, liquidationTickFromColPerDebt, tickHasDebtPda, tickPda } from '../lib/jupiterMath.js';

// solana_hash::Hash::default() (32 zero bytes) base58-encodes to this string.
const DEFAULT_BLOCKHASH = '11111111111111111111111111111111';
const TOKEN = 'TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA';

async function rpc(endpoint: string, body: unknown): Promise<any | undefined> {
  for (let attempt = 0; attempt < 5; attempt++) {
    try {
      const res = await fetch(endpoint, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify(body),
      });
      return await res.json();
    } catch {
      // fall through to retry
    }
    await new Promise((r) => setTimeout(r, 400 << attempt));
  }
  return undefined;
}

function b64field(d: any): Buffer | undefined {
  const s = d?.[0];
  if (typeof s !== 'string') return undefined;
  return Buffer.from(s, 'base64');
}

async function getAcct(endpoint: string, pk: PublicKey): Promise<Buffer | undefined> {
  const v = await rpc(endpoint, { jsonrpc: '2.0', id: 1, method: 'getAccountInfo', params: [pk.toBase58(), { encoding: 'base64' }] });
  if (v === undefined) return undefined;
  return b64field(v?.result?.value?.data);
}

async function mintOwner(endpoint: string, mint: PublicKey): Promise<PublicKey | undefined> {
  const v = await rpc(endpoint, { jsonrpc: '2.0', id: 1, method: 'getAccountInfo', params: [mint.toBase58(), { encoding: 'base64' }] });
  const owner: string | undefined = v?.result?.value?.owner;
  if (typeof owner !== 'string') return undefined;
  try {
    return new PublicKey(owner);
  } catch {
    return undefined;
  }
}

/** A decoded real liquidate ix: args + full ordered account list. */
interface RealLiq {
  sig: string;
  debtAmt: bigint;
  colPerUnitDebt: bigint;
  indices: number[];
  accounts: PublicKey[];
}

function decodeLiqArgs(data: Buffer): [bigint, bigint, number[]] | undefined {
  if (data.length < 32) return undefined;
  const debt = data.readBigUInt64LE(8);
  const colLo = data.readBigUInt64LE(16);
  const colHi = data.readBigUInt64LE(24);
  const col = (colHi << 64n) | colLo;
  let o = 32;
  if (o >= data.length) return undefined;
  o += 1; // absorb
  if (o >= data.length) return undefined;
  o += data[o] === 1 ? 2 : 1; // transfer_type
  if (o + 4 > data.length) return undefined;
  const ilen = data.readUInt32LE(o);
  o += 4;
  if (o + ilen > data.length) return undefined;
  const idx = Array.from(data.subarray(o, o + ilen));
  return [debt, col, idx];
}

/** Pull recent liquidate ixs off the program (named + loaded addresses resolved). */
async function recentLiquidates(endpoint: string, scan: number, want: number): Promise<RealLiq[]> {
  const prog = new PublicKey(VAULTS_PROGRAM);
  const sigs = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getSignaturesForAddress',
    params: [VAULTS_PROGRAM, { limit: scan }],
  });
  const out: RealLiq[] = [];
  const sigArr: any[] = Array.isArray(sigs?.result) ? sigs.result : [];
  for (const e of sigArr) {
    if (e?.err !== null && e?.err !== undefined) continue;
    const sig: string | undefined = e?.signature;
    if (typeof sig !== 'string') continue;
    const tx = await rpc(endpoint, {
      jsonrpc: '2.0',
      id: 1,
      method: 'getTransaction',
      params: [sig, { encoding: 'json', maxSupportedTransactionVersion: 0, commitment: 'confirmed' }],
    });
    if (tx === undefined) continue;
    const msg = tx?.result?.transaction?.message;
    const base: any[] | undefined = Array.isArray(msg?.accountKeys) ? msg.accountKeys : undefined;
    if (base === undefined) continue;
    const keys: PublicKey[] = [];
    for (const k of base) {
      if (typeof k === 'string') {
        try {
          keys.push(new PublicKey(k));
        } catch {
          // skip
        }
      }
    }
    const la = tx?.result?.meta?.loadedAddresses;
    if (la !== undefined && la !== null) {
      for (const side of ['writable', 'readonly']) {
        const arr: any[] = Array.isArray(la?.[side]) ? la[side] : [];
        for (const k of arr) {
          if (typeof k === 'string') {
            try {
              keys.push(new PublicKey(k));
            } catch {
              // skip
            }
          }
        }
      }
    }
    const check = (ix: any): RealLiq | undefined => {
      const pidx = ix?.programIdIndex;
      if (typeof pidx !== 'number') return undefined;
      if (!keys[pidx] || !keys[pidx].equals(prog)) return undefined;
      const dataStr = ix?.data;
      if (typeof dataStr !== 'string') return undefined;
      let data: Buffer;
      try {
        data = Buffer.from(bs58.decode(dataStr));
      } catch {
        return undefined;
      }
      if (data.length < 8 || !data.subarray(0, 8).equals(LIQUIDATE_DISC)) return undefined;
      const decoded = decodeLiqArgs(data);
      if (decoded === undefined) return undefined;
      const [debt, col, idx] = decoded;
      const accIdxArr: any[] = Array.isArray(ix?.accounts) ? ix.accounts : [];
      const accts: PublicKey[] = [];
      for (const i of accIdxArr) {
        if (typeof i === 'number' && keys[i]) accts.push(keys[i]);
      }
      return { sig, debtAmt: debt, colPerUnitDebt: col, indices: idx, accounts: accts };
    };
    let found: RealLiq | undefined;
    const topIxs: any[] = Array.isArray(msg?.instructions) ? msg.instructions : [];
    for (const ix of topIxs) {
      const r = check(ix);
      if (r !== undefined) {
        found = r;
        break;
      }
    }
    if (found === undefined) {
      const innerGroups: any[] = Array.isArray(tx?.result?.meta?.innerInstructions) ? tx.result.meta.innerInstructions : [];
      outer: for (const grp of innerGroups) {
        const innerIxs: any[] = Array.isArray(grp?.instructions) ? grp.instructions : [];
        for (const ix of innerIxs) {
          const r = check(ix);
          if (r !== undefined) {
            found = r;
            break outer;
          }
        }
      }
    }
    if (found !== undefined) {
      out.push(found);
      if (out.length >= want) break;
    }
  }
  return out;
}

/** Load one vault (config+state) by its vault_config pubkey. */
async function loadVault(endpoint: string, configPk: PublicKey): Promise<Vault | undefined> {
  const cfgRaw = await getAcct(endpoint, configPk);
  if (cfgRaw === undefined) return undefined;
  const cfg = VaultConfig.decode(cfgRaw);
  if (cfg === undefined) return undefined;
  const vaultIdBytes = Buffer.alloc(2);
  vaultIdBytes.writeUInt16LE(cfg.vaultId, 0);
  const [statePk] = PublicKey.findProgramAddressSync([Buffer.from('vault_state'), vaultIdBytes], new PublicKey(VAULTS_PROGRAM));
  const stRaw = await getAcct(endpoint, statePk);
  if (stRaw === undefined) return undefined;
  const st = VaultState.decode(stRaw);
  if (st === undefined) return undefined;
  const v = new Vault();
  v.configPubkey = configPk;
  v.statePubkey = statePk;
  v.config = cfg;
  v.state = st;
  return v;
}

async function simulate(endpoint: string, tx: VersionedTransaction): Promise<any | undefined> {
  const b = Buffer.from(tx.serialize()).toString('base64');
  return rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'simulateTransaction',
    params: [b, { sigVerify: false, replaceRecentBlockhash: true, commitment: 'processed', encoding: 'base64' }],
  });
}

/**
 * Deployed JUP_ALT (mainnet) — the fixed-liquidate-accounts lookup table. Used as
 * the "WITH ALT" A/B input when JUP_ALT isn't exported in the environment (it IS
 * on-chain, so the effect is provable either way).
 */
const JUP_ALT_DEPLOYED = 'DtGiu3mSRTyxypMjwgFLqWqp2rcpPQDHCaC8Rfaf2cyA';

/**
 * Build the full flash-loan fire tx with a chosen JUP_ALT setting by toggling the
 * env var `buildJupiterFireTx` reads (same path saveFire uses for SAVE_ALT).
 * `alt = undefined` strips JUP_ALT + LIQ_ALT so only Jupiter's own swap ALTs apply
 * (the A/B baseline); a pubkey folds that table in. Restores the prior env
 * afterward.
 */
async function buildFireWithAlt(
  endpoint: string,
  cand: JupiterFireCandidate,
  liquidatorMa: PublicKey,
  authority: PublicKey,
  alt: PublicKey | undefined,
): Promise<JupiterFireTx> {
  const savedJup = process.env.JUP_ALT;
  const savedLiq = process.env.LIQ_ALT;
  delete process.env.LIQ_ALT;
  if (alt !== undefined) {
    process.env.JUP_ALT = alt.toBase58();
  } else {
    delete process.env.JUP_ALT;
  }
  try {
    return await buildJupiterFireTx(endpoint, cand, liquidatorMa, authority, undefined, 0n, 50_000n, 100, 16, DEFAULT_BLOCKHASH);
  } finally {
    if (savedJup !== undefined) process.env.JUP_ALT = savedJup;
    else delete process.env.JUP_ALT;
    if (savedLiq !== undefined) process.env.LIQ_ALT = savedLiq;
    else delete process.env.LIQ_ALT;
  }
}

/** Load every in-scope USDC-debt vault straight from getProgramAccounts (no tx). */
async function loadAllInscopeUsdc(endpoint: string): Promise<Vault[]> {
  const disc58 = bs58.encode(VAULT_CONFIG_DISC);
  const v = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getProgramAccounts',
    params: [VAULTS_PROGRAM, { encoding: 'base64', filters: [{ memcmp: { offset: 0, bytes: disc58 } }] }],
  });
  const out: Vault[] = [];
  const arr: any[] = Array.isArray(v?.result) ? v.result : [];
  for (const e of arr) {
    const cpkStr = e?.pubkey;
    if (typeof cpkStr !== 'string') continue;
    let cpk: PublicKey;
    try {
      cpk = new PublicKey(cpkStr);
    } catch {
      continue;
    }
    const data = b64field(e?.account?.data);
    if (data === undefined) continue;
    const cfg = VaultConfig.decode(data);
    if (cfg === undefined) continue;
    if (cfg.debtLabel() !== 'USDC') continue;
    const vault = await loadVault(endpoint, cpk);
    if (vault !== undefined) out.push(vault);
  }
  out.sort((a, b) => a.config.vaultId - b.config.vaultId);
  return out;
}

/** Cheap check: does any recent tx on this vault_config carry a liquidate ix? */
async function resolveRecentLiquidateExists(endpoint: string, vaultConfig: PublicKey): Promise<boolean> {
  const sigs = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getSignaturesForAddress',
    params: [vaultConfig.toBase58(), { limit: 30 }],
  });
  const prog = new PublicKey(VAULTS_PROGRAM);
  const arr: any[] = Array.isArray(sigs?.result) ? sigs.result : [];
  for (const e of arr) {
    if (e?.err !== null && e?.err !== undefined) continue;
    const sig: string | undefined = e?.signature;
    if (typeof sig !== 'string') continue;
    const tx = await rpc(endpoint, {
      jsonrpc: '2.0',
      id: 1,
      method: 'getTransaction',
      params: [sig, { encoding: 'json', maxSupportedTransactionVersion: 0, commitment: 'confirmed' }],
    });
    if (tx === undefined) continue;
    const msg = tx?.result?.transaction?.message;
    const keyArr: any[] = Array.isArray(msg?.accountKeys) ? msg.accountKeys : [];
    const keys: (PublicKey | undefined)[] = keyArr.map((k) => {
      if (typeof k !== 'string') return undefined;
      try {
        return new PublicKey(k);
      } catch {
        return undefined;
      }
    });
    const ixs: any[] = Array.isArray(msg?.instructions) ? msg.instructions : [];
    for (const ix of ixs) {
      const pidx = typeof ix?.programIdIndex === 'number' ? ix.programIdIndex : 999;
      const k = keys[pidx];
      if (k === undefined || !k.equals(prog)) continue;
      const dataStr = ix?.data;
      if (typeof dataStr === 'string') {
        try {
          const d = bs58.decode(dataStr);
          if (d.length >= 8 && Buffer.from(d.subarray(0, 8)).equals(LIQUIDATE_DISC)) return true;
        } catch {
          // skip
        }
      }
    }
  }
  return false;
}

async function main(): Promise<void> {
  const endpoint = process.env.HELIUS_RPC ?? process.env.RPC_HTTP;
  if (!endpoint) throw new Error('HELIUS_RPC');
  const scan = Number.parseInt(process.env.SCAN_SIGS ?? '', 10) || 1000;
  const authority = new PublicKey(process.env.AUTHORITY ?? 'DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak');
  const liquidatorMa = new PublicKey(process.env.LIQUIDATOR_MA ?? 'B6e37TbC5n56tWbcgC3RRafUXSuEwRz9ZbhL8Ksro6vD');

  console.log(`[jup-fire] pulling recent real liquidates (scan ${scan} sigs)…`);
  const reals = await recentLiquidates(endpoint, scan, 12);
  console.log(`[jup-fire] captured ${reals.length} real liquidate ixs\n`);

  // ── STAGE 1: PDA-seed + layout ground-truth check ──────────────────────────
  console.log('═══ STAGE 1: PDA-seed reversal vs real txs ═══');
  let ok = 0;
  let bad = 0;
  let checked = 0;
  for (const r of reals) {
    if (r.indices.length !== 4) {
      console.log(`  ${r.sig.slice(0, 12)} : indices ${JSON.stringify(r.indices)} not len-4 ?!`);
      continue;
    }
    if (r.accounts.length < 26) continue;
    const vault = await loadVault(endpoint, r.accounts[4]!);
    if (vault === undefined) continue;
    const vid = vault.config.vaultId;
    const rem = r.accounts.slice(26);
    const [ns, nb, nt, nh] = r.indices as [number, number, number, number];
    if (ns + nb + nt + nh !== rem.length) {
      console.log(`  ${r.sig.slice(0, 12)} vault ${vid}: indices ${JSON.stringify(r.indices)} sum != ${rem.length} remaining`);
      continue;
    }
    const branches = rem.slice(ns, ns + nb);
    const ticks = rem.slice(ns + nb, ns + nb + nt);
    const thd = rem.slice(ns + nb + nt);
    let vok = true;
    // branches: read → branch_id → re-derive
    for (const pk of branches) {
      checked += 1;
      const raw = await getAcct(endpoint, pk);
      const b = raw !== undefined ? BranchLite.decode(raw) : undefined;
      if (b !== undefined && branchPda(vid, b.branchId).equals(pk)) {
        ok += 1;
      } else {
        bad += 1;
        vok = false;
      }
    }
    // ticks: read → tick → re-derive (Tick.tick @ offset 10, i32)
    for (const pk of ticks) {
      checked += 1;
      const raw = await getAcct(endpoint, pk);
      const tick = raw !== undefined && raw.length >= 14 ? raw.readInt32LE(10) : undefined;
      if (tick !== undefined && tickPda(vid, tick).equals(pk)) {
        ok += 1;
      } else {
        bad += 1;
        vok = false;
      }
    }
    // tick_has_debt: read → index (u8 @ offset 10) → re-derive
    for (const pk of thd) {
      checked += 1;
      const raw = await getAcct(endpoint, pk);
      const idx = raw !== undefined && raw.length > 10 ? raw.readUInt8(10) : undefined;
      if (idx !== undefined && tickHasDebtPda(vid, idx).equals(pk)) {
        ok += 1;
      } else {
        bad += 1;
        vok = false;
      }
    }
    console.log(
      `  ${r.sig.slice(0, 12)} vault ${String(vid).padStart(3)} [${vault.config.supplyToken.toBase58().slice(0, 4)}→${vault.config.debtLabel()}] idx ${JSON.stringify(r.indices)}  branches ${nb} ticks ${nt} thd ${nh}  ${vok ? '✓ all PDAs reproduce' : '✗ mismatch'}`,
    );
  }
  console.log(`  → ${ok}/${checked} tick/branch/tick_has_debt PDAs reproduced from seeds (${bad} mismatch)\n`);

  // ── STAGE 2+3: for EACH captured candidate, derive remaining accounts FRESH
  // from current state and sim the resolver liquidate. The first that composes
  // (VaultLiquidationResult / gated revert, not a size/RPC error) is the proof.
  const simVaultId = process.env.SIM_VAULT ? Number.parseInt(process.env.SIM_VAULT, 10) : undefined;
  const usable = (r: RealLiq): boolean => r.accounts.length >= 26 && r.indices.length === 4 && r.colPerUnitDebt > 0n;
  const fetch = (pk: PublicKey): Promise<Buffer | undefined> => getAcct(endpoint, pk);

  console.log('═══ STAGE 2+3: live selection + resolver sim (per candidate) ═══');
  let resolverProved: [Vault, PublicKey[], [number, number, number, number]] | undefined;
  for (const r of reals.filter(usable)) {
    const vault = await loadVault(endpoint, r.accounts[4]!);
    if (vault === undefined) continue;
    if (simVaultId !== undefined && vault.config.vaultId !== simVaultId) continue;
    const vid = vault.config.vaultId;
    const s = vault.state;
    const ns = r.indices[0]!;
    const oracleSources = r.accounts.slice(26, 26 + ns);
    // liquidation_tick reconstructed from the captured col_per_unit_debt
    // (production derives it live from the Lazer price).
    const liqTick =
      liquidationTickFromColPerDebt(r.colPerUnitDebt, vault.config.liquidationPenalty, vault.config.liquidationThreshold) ?? s.topmostTick - 1;
    const [remaining, indices] = await buildRemainingAccountsAsync(vid, s.topmostTick, s.currentBranchId, liqTick, oracleSources, fetch);

    // Keep the CAPTURED liquidator-side accounts (they satisfy the program's
    // token-owner constraints, as in the real tx); only the remaining/tick
    // accounts are our fresh derivation. col_per_unit_debt=0 = accept oracle
    // price (no false slippage revert). to != DEAD, so this exercises the real
    // liquidate path (not the resolver, which needs to's ATA to exist).
    const a = accountsFromCaptured(vault, r.accounts);
    if (a === undefined) continue;
    a.remaining = remaining;
    const resolverIx = buildLiquidateIx(a, r.debtAmt, 0n, false, 1, Uint8Array.from(indices));
    const msg = new TransactionMessage({ payerKey: r.accounts[0]!, recentBlockhash: DEFAULT_BLOCKHASH, instructions: [resolverIx] }).compileToV0Message(
      [],
    );
    const tx = new VersionedTransaction(msg);
    const txBytes = tx.serialize().length;
    process.stdout.write(
      `  vault ${String(vid).padStart(3)} [${vault.config.supplyToken.toBase58().slice(0, 4)}→${vault.config.debtLabel()}] liq_tick=${liqTick} derived idx ${JSON.stringify(indices)} → ${txBytes}B: `,
    );
    if (txBytes > 1232) {
      console.log('resolver tx > 1232 (needs ALT for a single-packet fire) — skip sim');
      continue;
    }
    const raw = await simulate(endpoint, tx);
    const val = raw?.result?.value;
    if (val !== undefined && val !== null) {
      const logs: string[] = Array.isArray(val?.logs) ? val.logs.filter((l: unknown): l is string => typeof l === 'string') : [];
      // "Composes" = the ix passed account validation and reached the
      // liquidation logic (a Vault* liquidation/slippage gate), or ran clean.
      const gate = logs.find((l) => l.includes('Vault') && (l.includes('Liquidat') || l.includes('Slippage') || l.includes('TopTick') || l.includes('Tick')));
      if (val?.err === null || val?.err === undefined) {
        console.log('★★ liquidate SIMULATES CLEAN — composes end-to-end; vault liquidatable at live price');
        resolverProved = [vault, remaining, indices];
        break;
      } else if (gate !== undefined) {
        console.log("★ composes → gated at the protocol's own liquidation gate");
        console.log(`     ${gate.trim()}`);
        resolverProved = [vault, remaining, indices];
        break;
      } else {
        console.log(`upstream revert: ${JSON.stringify(val?.err)}`);
        for (const l of logs.slice(-4)) console.log(`       ${l}`);
      }
    } else {
      console.log(`RPC error: ${raw?.error !== undefined ? JSON.stringify(raw.error) : ''}`);
    }
  }
  if (resolverProved === undefined) {
    console.log('  (no candidate composed cleanly — see per-candidate reasons above)');
  }

  // Full flash-loan-wrapped fire tx (USDC debt only) — compose + size + sim.
  let provedVault: Vault | undefined;
  if (resolverProved !== undefined) {
    const [vault, remaining, indices] = resolverProved;
    let r: RealLiq | undefined;
    for (const cand of reals) {
      if (!usable(cand)) continue;
      const v = await loadVault(endpoint, cand.accounts[4]!);
      if (v !== undefined && v.config.vaultId === vault.config.vaultId) {
        r = cand;
        break;
      }
    }
    if (r === undefined) throw new Error('resolver-proved vault vanished');
    console.log(`\n═══ STAGE 4: flash-loan-wrapped fire tx (vault ${vault.config.vaultId}) ═══`);
    if (vault.config.debtLabel() === 'USDC') {
      console.log('\n  ── full flash-loan-wrapped fire tx ──');
      const collatMint = vault.config.supplyToken;
      const ctp = (await mintOwner(endpoint, collatMint)) ?? new PublicKey(TOKEN);
      const fa = accountsFromCaptured(vault, r.accounts);
      if (fa === undefined) throw new Error('accountsFromCaptured');
      fa.remaining = remaining;
      // size the seize by the resolver-implied col if available, else a nominal
      const denom = 10n ** 15n;
      const colFloor = 10n ** 13n;
      const col = r.colPerUnitDebt > colFloor ? r.colPerUnitDebt : colFloor;
      const seize = (r.debtAmt * col) / denom;
      const cand: JupiterFireCandidate = {
        accts: fa,
        debtAmt: r.debtAmt,
        colPerUnitDebt: 0n,
        remaining,
        remainingIndices: indices,
        seizeUnderlying: seize > 1n ? seize : 1n,
        collateralMint: collatMint,
        collateralTokenProgram: ctp,
      };
      let jupAlt: PublicKey;
      try {
        jupAlt = new PublicKey(process.env.JUP_ALT ?? JUP_ALT_DEPLOYED);
      } catch {
        jupAlt = new PublicKey(JUP_ALT_DEPLOYED);
      }
      try {
        const without = await buildFireWithAlt(endpoint, cand, liquidatorMa, authority, undefined);
        const w = await buildFireWithAlt(endpoint, cand, liquidatorMa, authority, jupAlt);
        console.log(
          `     A/B  without JUP_ALT: ${without.txBytes}B   with JUP_ALT: ${w.txBytes}B   (Δ −${without.txBytes - w.txBytes}B; quoted USDC out ${w.quotedUsdcOut})`,
        );
        if (w.txBytes <= 1232) {
          const simVal = (await simulate(endpoint, w.tx))?.result?.value;
          if (simVal !== undefined && (simVal?.err === null || simVal?.err === undefined)) {
            console.log(`     ★★ FIRE TX SIMULATES CLEAN — would liquidate profitably now (${simVal?.unitsConsumed} CU)`);
          } else if (simVal !== undefined) {
            console.log(`     fire tx gated/other: ${JSON.stringify(simVal?.err)}`);
            const logs: string[] = Array.isArray(simVal?.logs) ? simVal.logs.filter((l: unknown): l is string => typeof l === 'string') : [];
            for (const l of logs.slice(-6)) console.log(`        ${l}`);
          } else {
            console.log('     fire sim returned nothing');
          }
        } else {
          console.log(`     ⓘ WITH JUP_ALT still ${w.txBytes}B > 1232 for this vault — size-gates off (never cached/submitted).`);
        }
      } catch (e) {
        console.log(`     fire build failed (often: Jupiter quote for tiny/odd size): ${e instanceof Error ? e.message : String(e)}`);
      }
    } else {
      console.log(`  (vault debt is ${vault.config.debtLabel()}, not USDC — flash-loan wrap is USDC-only; resolver sim already proved the liquidate leg composes.)`);
    }
  }

  // ── STAGE 5: PURE-SEED derivation on a NO-RECENT-TX vault ──────────────────
  // The crux: derive the FULL liquidate account set from seeds + on-chain state
  // (NO captured tx), for a vault that has no recent liquidate to lift from, and
  // sim it. Success = resolver revert (VaultLiquidationResult) / a protocol
  // liquidation gate (VaultInvalidLiquidation 6027 = composition proven).
  console.log('\n═══ STAGE 5: pure-seed account set on a NO-RECENT-TX vault ═══');
  const authorityUsdc = (() => {
    // authority's USDC ATA is our resolver signer_token_account (must exist).
    const usdc = new PublicKey('EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v');
    const tp = new PublicKey(TOKEN);
    return ataFor(authority, usdc, tp);
  })();
  // vaults that appeared as a config in a real liquidate → "has recent tx"; the
  // rest are our standalone targets.
  const withTx = new Set<string>();
  for (const r of reals) {
    if (r.accounts[4]) withTx.add(r.accounts[4].toBase58());
  }
  const allVaults = await loadAllInscopeUsdc(endpoint);
  console.log(`  ${allVaults.length} in-scope USDC vaults; ${withTx.size} of the captured liquidates map to a vault`);
  let provedStandalone = false;
  for (const v of allVaults) {
    if (simVaultId !== undefined && v.config.vaultId !== simVaultId) continue;
    const hasTx = withTx.has(v.configPubkey.toBase58()) || (await resolveRecentLiquidateExists(endpoint, v.configPubkey));
    const vid = v.config.vaultId;
    // oracle sources straight from the decoded oracle account.
    const oracleRaw = await getAcct(endpoint, v.config.oracle);
    const sources = oracleRaw !== undefined ? decodeOracleSources(oracleRaw) : undefined;
    if (sources === undefined) {
      console.log(`  vault ${vid}: oracle decode failed — skip`);
      continue;
    }
    const stp = (await mintOwner(endpoint, v.config.supplyToken)) ?? new PublicKey(TOKEN);
    const btp = (await mintOwner(endpoint, v.config.borrowToken)) ?? new PublicKey(TOKEN);
    const a: LiquidateAccounts = deriveLiquidateAccounts(v, stp, btp);
    // resolver: to = ADDRESS_DEAD (program computes + reverts with the exact
    // liquidation result); signer = authority (its USDC ATA exists as required).
    setLiquidatorSide(a, authority, authorityUsdc, ADDRESS_DEAD, ataFor(ADDRESS_DEAD, v.config.supplyToken, stp));
    const liqTick = v.state.topmostTick - 1; // minimal band: include topmost only
    const [remaining, indices] = await buildRemainingAccountsAsync(vid, v.state.topmostTick, v.state.currentBranchId, liqTick, sources, fetch);
    a.remaining = remaining;
    const debt = v.state.totalBorrow / 50n > 1_000_000n ? v.state.totalBorrow / 50n : 1_000_000n;
    const ix = buildLiquidateIx(a, debt, 0n, false, 1, Uint8Array.from(indices));
    const msg = new TransactionMessage({ payerKey: authority, recentBlockhash: DEFAULT_BLOCKHASH, instructions: [ix] }).compileToV0Message([]);
    const tx = new VersionedTransaction(msg);
    const bytes = tx.serialize().length;
    process.stdout.write(
      `  vault ${String(vid).padStart(3)} [${v.config.supplyToken.toBase58().slice(0, 4)}→${v.config.debtLabel()}] recent_tx=${hasTx} src=${sources.length} idx=${JSON.stringify(indices)} liquidate-only ${bytes}B: `,
    );
    if (bytes > 1232) {
      console.log('(> 1232, needs ALT to sim standalone) — deriving-only');
      continue;
    }
    const raw = await simulate(endpoint, tx);
    const val = raw?.result?.value;
    if (val !== undefined && val !== null) {
      const logs: string[] = Array.isArray(val?.logs) ? val.logs.filter((l: unknown): l is string => typeof l === 'string') : [];
      const gate = logs.find(
        (l) => l.includes('VaultLiquidationResult') || l.includes('VaultInvalidLiquidation') || (l.includes('Vault') && (l.includes('Liquidat') || l.includes('Slippage') || l.includes('Tick'))),
      );
      if (val?.err === null || val?.err === undefined) {
        console.log('★★ SIMULATES CLEAN — full seed-derived set composes, vault liquidatable now');
        provedStandalone = true;
        if (provedVault === undefined) provedVault = v;
      } else if (gate !== undefined) {
        console.log("★ composes → protocol liquidation gate (seed set validated on-chain)");
        console.log(`      ${gate.trim()}`);
        provedStandalone = true;
        if (provedVault === undefined) provedVault = v;
      } else {
        console.log(`revert: ${JSON.stringify(val?.err)}`);
        for (const l of logs.slice(-5)) console.log(`        ${l}`);
      }
    } else {
      console.log('RPC/sim error');
    }
    if (provedStandalone && simVaultId === undefined) break;
  }
  if (!provedStandalone) {
    console.log('  (no standalone vault reached a clean/gated sim under 1232B without an ALT — the');
    console.log('   seed derivation is validated by jupiter_seed_probe PROOF A; the full fire tx below');
    console.log('   needs the JUP_ALT to fit a single packet, then sims through the liquidity CPI.)');
  }

  // ── STAGE 6: full flash-loan fire tx — JUP_ALT A/B (undeniable size proof) ──
  // Build the SAME wrapped fire twice: once with only Jupiter's own swap ALTs
  // (baseline), once with JUP_ALT folded in. Print both sizes + the delta; when
  // the WITH-ALT packet is ≤ 1232, actually simulate it (sigVerify=false,
  // replaceRecentBlockhash=true, processed). Mirrors the Save A/B (1804→1274).
  if (provedVault !== undefined) {
    const v = provedVault;
    console.log(`\n═══ STAGE 6: full flash-loan fire tx — JUP_ALT A/B (vault ${v.config.vaultId}) ═══`);
    let jupAlt: PublicKey;
    try {
      jupAlt = new PublicKey(process.env.JUP_ALT ?? JUP_ALT_DEPLOYED);
    } catch {
      jupAlt = new PublicKey(JUP_ALT_DEPLOYED);
    }
    const stp = (await mintOwner(endpoint, v.config.supplyToken)) ?? new PublicKey(TOKEN);
    const btp = (await mintOwner(endpoint, v.config.borrowToken)) ?? new PublicKey(TOKEN);
    const oracleRaw = await getAcct(endpoint, v.config.oracle);
    const sources = (oracleRaw !== undefined ? decodeOracleSources(oracleRaw) : undefined) ?? [];
    const a = deriveLiquidateAccounts(v, stp, btp);
    const liqTick = v.state.topmostTick - 1;
    const [remaining, indices] = await buildRemainingAccountsAsync(v.config.vaultId, v.state.topmostTick, v.state.currentBranchId, liqTick, sources, fetch);
    a.remaining = remaining;
    // Nominal repay for the sim: a fraction of total_borrow, but CAPPED so the
    // marginfi flash-borrow leg doesn't trip the USDC bank's utilization gate
    // (IllegalUtilizationRatio 6026) — that would mask the downstream liquidate
    // gate we want to reach. The real executor sim-gates on a CLEAN sim anyway.
    let debt = v.state.totalBorrow / 50n;
    if (debt < 1_000_000n) debt = 1_000_000n;
    if (debt > 25_000_000n) debt = 25_000_000n;
    const cand: JupiterFireCandidate = {
      accts: a,
      debtAmt: debt,
      colPerUnitDebt: 0n,
      remaining,
      remainingIndices: indices,
      seizeUnderlying: debt > 1n ? debt : 1n,
      collateralMint: v.config.supplyToken,
      collateralTokenProgram: stp,
    };
    console.log(`     using JUP_ALT ${jupAlt.toBase58()}  (nominal repay ${debt} native)`);
    try {
      const without = await buildFireWithAlt(endpoint, cand, liquidatorMa, authority, undefined);
      const w = await buildFireWithAlt(endpoint, cand, liquidatorMa, authority, jupAlt);
      const delta = without.txBytes - w.txBytes;
      console.log(`     A/B  without JUP_ALT: ${without.txBytes}B   with JUP_ALT: ${w.txBytes}B   (Δ −${delta}B; limit 1232)`);
      if (w.txBytes <= 1232) {
        console.log('     ✓ WITH JUP_ALT the full wrapped fire fits a single packet — simulating it:');
        const simVal = (await simulate(endpoint, w.tx))?.result?.value;
        if (simVal !== undefined && (simVal?.err === null || simVal?.err === undefined)) {
          console.log(
            `     ★★ FULL FIRE TX SIMULATES CLEAN (${simVal?.unitsConsumed} CU) — seed liquidate + flash-loan + swap composes end-to-end; would liquidate now`,
          );
        } else if (simVal !== undefined) {
          // A revert at the protocol's own liquidation gate (6027 /
          // VaultInvalidLiquidation) = composition proven; the vault
          // just isn't underwater at the live price.
          const logs: string[] = Array.isArray(simVal?.logs) ? simVal.logs.filter((l: unknown): l is string => typeof l === 'string') : [];
          const gate = logs.find(
            (l) => l.includes('6027') || l.includes('VaultInvalidLiquidation') || (l.includes('Vault') && (l.includes('Liquidat') || l.includes('Slippage') || l.includes('Tick'))),
          );
          if (gate !== undefined) {
            console.log("     ★ FULL FIRE composes → gated at the protocol's OWN liquidation gate (fireable wiring, vault healthy now)");
            console.log(`        ${gate.trim()}`);
          } else {
            console.log(`     full fire tx reverted upstream: ${JSON.stringify(simVal?.err)}`);
          }
          for (const l of logs.slice(-6)) console.log(`        ${l}`);
        } else {
          const errRaw = await simulate(endpoint, w.tx);
          console.log(`     full fire sim not returned (RPC error): ${errRaw?.error?.message ?? ''}`);
        }
      } else {
        console.log(`     ⓘ WITH JUP_ALT still ${w.txBytes}B > 1232 for this vault — it SIZE-GATES OFF (the executor never`);
        console.log('       caches/submits a >1232 tx). Other in-scope USDC vaults with fewer tick/branch remaining');
        console.log('       accounts fit; the liquidate LEG is sim-proven above (6027 gate, sub-1232).');
      }
    } catch (e) {
      console.log(`     fire build failed (often a Jupiter quote hiccup for the nominal size): ${e instanceof Error ? e.message : String(e)}`);
    }
  }

  console.log('\n[jup-fire] done.');
}

/**
 * Async pre-fetch wrapper around the lib's synchronous `buildRemainingAccounts`
 * (its `fetch` callback is async here over RPC, unlike the sync Rust closure
 * over a blocking `ureq` call). Walks the same branch-chain / tick-bitmap PDAs
 * the lib function will need, populates a cache via async RPC calls, then
 * delegates the actual selection algorithm to the lib (single source of truth).
 */
async function buildRemainingAccountsAsync(
  vaultId: number,
  topmostTick: number,
  currentBranchId: number,
  liquidationTick: number,
  oracleSources: PublicKey[],
  fetch: (addr: PublicKey) => Promise<Buffer | undefined>,
): Promise<[PublicKey[], [number, number, number, number]]> {
  const cache = new Map<string, Buffer | undefined>();
  const need = async (addr: PublicKey): Promise<Buffer | undefined> => {
    const key = addr.toBase58();
    if (cache.has(key)) return cache.get(key);
    const v = await fetch(addr);
    cache.set(key, v);
    return v;
  };

  // Pre-fetch the branch chain (bounded: a real branch chain is short).
  let connected = currentBranchId > 0 ? currentBranchId : 0;
  const seenBranches = new Set<number>();
  while (connected > 0 && !seenBranches.has(connected)) {
    seenBranches.add(connected);
    const raw = await need(branchPda(vaultId, connected));
    const b = raw !== undefined ? BranchLite.decode(raw) : undefined;
    if (b === undefined) break;
    connected = b.connectedBranchId;
  }
  await need(branchPda(vaultId, 0));

  // Pre-fetch every tick_has_debt array from topmost's index down to 0, and
  // walk the bitmap synchronously (pure in-memory) to discover which tick PDAs
  // to pre-fetch next, repeating until the walk reaches liquidationTick.
  for (let i = indexForTick(topmostTick); i >= 0; i--) {
    await need(tickHasDebtPda(vaultId, i));
  }
  const arrayFetch = (idx: number): Buffer | undefined => cache.get(tickHasDebtPda(vaultId, idx).toBase58());
  await need(tickPda(vaultId, topmostTick));
  let walkTick = topmostTick;
  const seenTicks = new Set<number>();
  for (;;) {
    const nextTick = findNextTickWithDebt(walkTick, arrayFetch);
    if (nextTick <= liquidationTick || seenTicks.has(nextTick)) break;
    seenTicks.add(nextTick);
    await need(tickPda(vaultId, nextTick));
    if (nextTick === walkTick) break;
    walkTick = nextTick;
  }

  const syncFetch = (addr: PublicKey): Buffer | undefined => cache.get(addr.toBase58());
  return buildRemainingAccounts(vaultId, topmostTick, currentBranchId, liquidationTick, oracleSources, syncFetch);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
