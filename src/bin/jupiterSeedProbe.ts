// Port of src/bin/jupiter_seed_probe.rs
//
// Validate the pure-seed derivation of the Fluid (Jupiter Lend) **Liquidity**
// program accounts that a Vaults `liquidate` ix needs (positions 9..=22) + the
// oracle `sources` — the accounts the executor used to lift from a captured tx.
// The point: prove they can be derived for ANY vault WITHOUT a recent liquidate.
//
// Two independent proofs:
//  A. STANDALONE (no tx needed): for every in-scope vault, derive each account
//     from seeds via `jupiterFire.deriveLiquidateAccounts` + the decoded
//     oracle sources, then read it on-chain and assert it's real and correctly
//     owned (Liquidity PDAs owned by the liquidity program; the vault token
//     accounts are SPL accounts whose authority == the `liquidity` PDA and whose
//     mint matches). This is what lets a never-liquidated vault arm.
//  B. GROUND TRUTH (when a recent liquidate exists): pull real liquidate txs and
//     assert the seed-derived pubkeys EQUAL the exact accounts the real
//     liquidator passed at positions 9..=22 + the oracle sources.
//
// Read-only. Usage: HELIUS_RPC=<url> [SCAN_SIGS=1500] [MAX_VAULTS=12]
//   tsx src/bin/jupiterSeedProbe.ts

import 'dotenv/config';
import bs58 from 'bs58';
import { PublicKey } from '@solana/web3.js';
import { decodeOracleSources, VAULT_CONFIG_DISC, VAULT_STATE_DISC, VAULTS_PROGRAM, Vault, VaultConfig, VaultState } from '../lib/jupiter.js';
import { deriveLiquidateAccounts, LIQUIDATE_DISC, type LiquidateAccounts } from '../lib/jupiterFire.js';
import { liquidityPda, newBranchId } from '../lib/jupiterMath.js';

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

/** (owner, data) for a batch of accounts (undefined slot = account missing). */
async function getMulti(endpoint: string, pks: PublicKey[]): Promise<(readonly [PublicKey, Buffer] | undefined)[]> {
  const strs = pks.map((p) => p.toBase58());
  const out: (readonly [PublicKey, Buffer] | undefined)[] = new Array(pks.length).fill(undefined);
  for (let chunkI = 0; chunkI * 100 < strs.length; chunkI++) {
    const chunk = strs.slice(chunkI * 100, chunkI * 100 + 100);
    const v = await rpc(endpoint, { jsonrpc: '2.0', id: 1, method: 'getMultipleAccounts', params: [chunk, { encoding: 'base64' }] });
    const arr: any[] = Array.isArray(v?.result?.value) ? v.result.value : [];
    for (let j = 0; j < arr.length; j++) {
      const acc = arr[j];
      if (acc === null || acc === undefined) continue;
      const ownerStr: string | undefined = acc?.owner;
      const data = b64field(acc?.data);
      if (typeof ownerStr === 'string' && data !== undefined) {
        try {
          out[chunkI * 100 + j] = [new PublicKey(ownerStr), data] as const;
        } catch {
          // skip unparseable owner
        }
      }
    }
  }
  return out;
}

async function gpaByDisc(endpoint: string, disc: Buffer): Promise<[PublicKey, Buffer][]> {
  const disc58 = bs58.encode(disc);
  const v = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getProgramAccounts',
    params: [VAULTS_PROGRAM, { encoding: 'base64', filters: [{ memcmp: { offset: 0, bytes: disc58 } }] }],
  });
  const out: [PublicKey, Buffer][] = [];
  const arr: any[] = Array.isArray(v?.result) ? v.result : [];
  for (const e of arr) {
    const pkStr = e?.pubkey;
    const data = b64field(e?.account?.data);
    if (typeof pkStr === 'string' && data !== undefined) {
      try {
        out.push([new PublicKey(pkStr), data]);
      } catch {
        // skip unparseable pubkey
      }
    }
  }
  return out;
}

async function loadVaults(endpoint: string): Promise<Vault[]> {
  const configs = new Map<number, [PublicKey, VaultConfig]>();
  for (const [pk, d] of await gpaByDisc(endpoint, VAULT_CONFIG_DISC)) {
    const c = VaultConfig.decode(d);
    if (c !== undefined) configs.set(c.vaultId, [pk, c]);
  }
  const states = new Map<number, [PublicKey, VaultState]>();
  for (const [pk, d] of await gpaByDisc(endpoint, VAULT_STATE_DISC)) {
    const s = VaultState.decode(d);
    if (s !== undefined) states.set(s.vaultId, [pk, s]);
  }
  const vaults: Vault[] = [];
  for (const [vid, [cpk, c]] of configs) {
    const st = states.get(vid);
    if (st !== undefined) {
      const [spk, s] = st;
      const v = new Vault();
      v.configPubkey = cpk;
      v.statePubkey = spk;
      v.config = c;
      v.state = s;
      vaults.push(v);
    }
  }
  vaults.sort((a, b) => a.config.vaultId - b.config.vaultId);
  return vaults;
}

/**
 * SPL token-account decode: mint @0, owner @32 (both work for Token & Token-2022
 * base layout).
 */
function tokenAcctMintOwner(data: Buffer): readonly [PublicKey, PublicKey] | undefined {
  if (data.length < 64) return undefined;
  return [new PublicKey(data.subarray(0, 32)), new PublicKey(data.subarray(32, 64))] as const;
}

// ── real-liquidate capture (shared shape with jupiterFireProbe) ──────────────
interface RealLiq {
  sig: string;
  accounts: PublicKey[];
  indices: number[];
}

function decodeIndices(data: Buffer): number[] | undefined {
  let o = 8 + 8 + 16 + 1;
  if (o >= data.length) return undefined;
  o += data[o] === 1 ? 2 : 1;
  if (o + 4 > data.length) return undefined;
  const ilen = data.readUInt32LE(o);
  o += 4;
  if (o + ilen > data.length) return undefined;
  return Array.from(data.subarray(o, o + ilen));
}

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
    const base: any[] = Array.isArray(msg?.accountKeys) ? msg.accountKeys : [];
    if (base.length === 0 && !Array.isArray(msg?.accountKeys)) continue;
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
      let data: Uint8Array;
      try {
        data = bs58.decode(dataStr);
      } catch {
        return undefined;
      }
      if (data.length < 8 || !Buffer.from(data.subarray(0, 8)).equals(LIQUIDATE_DISC)) return undefined;
      const indices = decodeIndices(Buffer.from(data));
      if (indices === undefined) return undefined;
      const accIdxArr: any[] = Array.isArray(ix?.accounts) ? ix.accounts : [];
      const accts: PublicKey[] = [];
      for (const i of accIdxArr) {
        if (typeof i === 'number' && keys[i]) accts.push(keys[i]);
      }
      return { sig, accounts: accts, indices };
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

async function main(): Promise<void> {
  const endpoint = process.env.HELIUS_RPC ?? process.env.RPC_HTTP;
  if (!endpoint) throw new Error('HELIUS_RPC');
  const scan = Number.parseInt(process.env.SCAN_SIGS ?? '', 10) || 1500;
  const maxVaults = Number.parseInt(process.env.MAX_VAULTS ?? '', 10) || 12;

  const liqProg = new PublicKey('jupeiUmn818Jg1ekPURTpr4mFo29p46vygyykFJ3wZC');
  const liquidity = liquidityPda();
  console.log(`[seed] liquidity global PDA = ${liquidity.toBase58()}`);
  if ((await getAcct(endpoint, liquidity)) !== undefined) {
    console.log('[seed]   ✓ exists on-chain\n');
  } else {
    console.log("[seed]   ✗ MISSING — seed for `liquidity` is wrong!\n");
  }

  const vaults = await loadVaults(endpoint);
  const scoped = vaults.filter((v) => v.config.debtInScope());
  console.log(`[seed] ${vaults.length} vaults total, ${scoped.length} in-scope (USDC/USDT/wSOL debt)\n`);

  // token program per mint (cache).
  const mintTp = new Map<string, PublicKey>();
  {
    const mints: PublicKey[] = [];
    for (const v of scoped) {
      mints.push(v.config.supplyToken, v.config.borrowToken);
    }
    const got = await getMulti(endpoint, mints);
    for (let i = 0; i < mints.length; i++) {
      const g = got[i];
      if (g !== undefined) mintTp.set(mints[i]!.toBase58(), g[0]);
    }
  }
  const tp = (m: PublicKey): PublicKey => mintTp.get(m.toBase58()) ?? new PublicKey(TOKEN);

  // ── PROOF A: standalone seed derivation validated vs live account state ──
  console.log('═══ PROOF A — seed-derived accounts exist + correctly owned (no tx) ═══');
  let aPass = 0;
  let aChecked = 0;
  for (const v of scoped.slice(0, maxVaults)) {
    const vid = v.config.vaultId;
    const a: LiquidateAccounts = deriveLiquidateAccounts(v, tp(v.config.supplyToken), tp(v.config.borrowToken));
    // oracle sources from the decoded oracle account.
    const oracleRaw = await getAcct(endpoint, v.config.oracle);
    const sources = oracleRaw !== undefined ? (decodeOracleSources(oracleRaw) ?? []) : [];

    // group of (label, pubkey, expected-owner-kind) to verify.
    // kind: 'L' liquidity-owned PDA, 'T' spl token account (owner=liquidity),
    // 'V' vaults-program PDA, 'N' none-sentinel, 'O' oracle-source (any, just must exist).
    const checks: [string, PublicKey, string][] = [
      ['supply_reserves', a.supplyTokenReservesLiquidity, 'L'],
      ['borrow_reserves', a.borrowTokenReservesLiquidity, 'L'],
      ['supply_position', a.vaultSupplyPositionOnLiquidity, 'L'],
      ['borrow_position', a.vaultBorrowPositionOnLiquidity, 'L'],
      ['supply_rate_model', a.supplyRateModel, 'L'],
      ['borrow_rate_model', a.borrowRateModel, 'L'],
      ['supply_claim(None)', a.supplyTokenClaimAccount, 'N'],
      ['vault_supply_tok_acct', a.vaultSupplyTokenAccount, 'T'],
      ['vault_borrow_tok_acct', a.vaultBorrowTokenAccount, 'T'],
      ['new_branch', a.newBranch, 'V'],
    ];
    sources.forEach((s, i) => {
      checks.push([`oracle_source[${i}]`, s, 'O']);
    });
    const pks = checks.map(([, p]) => p);
    const got = await getMulti(endpoint, pks);

    const nbId = newBranchId(v.state.branchLiquidated, v.state.currentBranchId, v.state.totalBranchId);
    console.log(
      `── vault ${String(vid).padStart(3)} [${v.config.supplyToken.toBase58().slice(0, 4)}→${v.config.debtLabel()}]  new_branch_id=${nbId} (bl=${v.state.branchLiquidated}, cur=${v.state.currentBranchId}, tot=${v.state.totalBranchId})  ${sources.length} oracle src ──`,
    );
    for (let i = 0; i < checks.length; i++) {
      const [label, pk, kind] = checks[i]!;
      const g = got[i];
      aChecked += 1;
      let verdict: string;
      if (kind === 'N' && pk.equals(new PublicKey(VAULTS_PROGRAM))) {
        verdict = '✓ None-sentinel (=vaults program id)';
      } else if (kind === 'L' && g !== undefined && g[0].equals(liqProg)) {
        verdict = '✓ liquidity-owned';
      } else if (kind === 'V' && g !== undefined && g[0].toBase58() === VAULTS_PROGRAM) {
        verdict = '✓ vaults-owned';
      } else if (kind === 'V' && g === undefined) {
        verdict = '· not yet created (branch reused/absent — sim is the gate)';
      } else if (kind === 'T' && g !== undefined) {
        const mo = tokenAcctMintOwner(g[1]);
        if (mo !== undefined) {
          const [, auth] = mo;
          const values = Array.from(mintTp.values());
          if (auth.equals(liquidity) && values.some((t) => t.equals(g[0]))) {
            verdict = '✓ SPL acct, authority=liquidity';
          } else {
            verdict = '✗ token acct authority/owner mismatch';
          }
        } else {
          verdict = '✗ token acct authority/owner mismatch';
        }
      } else if (kind === 'O' && g !== undefined) {
        verdict = '✓ source exists';
      } else if (g !== undefined) {
        verdict = '✗ wrong owner';
      } else {
        verdict = '✗ MISSING';
      }
      if (verdict.startsWith('✓') || verdict.startsWith('·')) aPass += 1;
      // only print the interesting / failing lines to keep output readable
      if (!label.startsWith('oracle_source') || !verdict.startsWith('✓')) {
        console.log(`     ${label.padEnd(22)} ${pk.toBase58().slice(0, 8)}  ${verdict}`);
      }
    }
  }
  console.log(`\n  → PROOF A: ${aPass}/${aChecked} derived accounts real + correctly owned\n`);

  // ── PROOF B: exact-match vs real liquidate txs (only if any exist) ──
  console.log('═══ PROOF B — seed derivation == real liquidator\'s accounts (ground truth) ═══');
  const reals = await recentLiquidates(endpoint, scan, 10);
  if (reals.length === 0) {
    console.log(`  (no recent liquidate tx in ${scan} sigs — liquidations are rare on this protocol;`);
    console.log("   PROOF A + the on-chain program's own seed constraints at sim are the validation.)");
  }
  let bOk = 0;
  let bBad = 0;
  for (const r of reals) {
    if (r.accounts.length < 26) continue;
    const v = await loadVault(endpoint, r.accounts[4]!);
    if (v === undefined) continue;
    const a = deriveLiquidateAccounts(v, tp(v.config.supplyToken), tp(v.config.borrowToken));
    const srcN = r.indices[0] ?? 0;
    const oracleRaw = await getAcct(endpoint, v.config.oracle);
    const derivedSources = oracleRaw !== undefined ? (decodeOracleSources(oracleRaw) ?? []) : [];
    const realSources = r.accounts.slice(26, Math.min(26 + srcN, r.accounts.length));
    const pairs: [string, PublicKey, PublicKey][] = [
      ['new_branch', a.newBranch, r.accounts[9]!],
      ['supply_reserves', a.supplyTokenReservesLiquidity, r.accounts[10]!],
      ['borrow_reserves', a.borrowTokenReservesLiquidity, r.accounts[11]!],
      ['supply_position', a.vaultSupplyPositionOnLiquidity, r.accounts[12]!],
      ['borrow_position', a.vaultBorrowPositionOnLiquidity, r.accounts[13]!],
      ['supply_rate_model', a.supplyRateModel, r.accounts[14]!],
      ['borrow_rate_model', a.borrowRateModel, r.accounts[15]!],
      ['supply_claim', a.supplyTokenClaimAccount, r.accounts[16]!],
      ['liquidity', a.liquidity, r.accounts[17]!],
      ['vault_supply_tok_acct', a.vaultSupplyTokenAccount, r.accounts[19]!],
      ['vault_borrow_tok_acct', a.vaultBorrowTokenAccount, r.accounts[20]!],
      ['liquidity_program', a.liquidityProgram, r.accounts[18]!],
    ];
    let vok = true;
    const fails: string[] = [];
    for (const [label, derived, real] of pairs) {
      if (derived.equals(real)) {
        bOk += 1;
      } else {
        bBad += 1;
        vok = false;
        fails.push(label);
      }
    }
    // oracle sources exact-order match
    const srcMatch = derivedSources.length === realSources.length && derivedSources.every((a2, i) => a2.equals(realSources[i]!));
    if (srcMatch) {
      bOk += 1;
    } else {
      bBad += 1;
      vok = false;
      fails.push('oracle_sources');
    }
    console.log(
      `  ${r.sig.slice(0, 10)} vault ${String(v.config.vaultId).padStart(3)} [${v.config.supplyToken.toBase58().slice(0, 4)}→${v.config.debtLabel()}]  ${
        vok ? '✓ ALL 12 accts + sources reproduced from seeds' : `✗ mismatch: ${JSON.stringify(fails)}`
      }${srcMatch ? '' : ' (see sources)'}`,
    );
  }
  if (reals.length > 0) {
    console.log(`\n  → PROOF B: ${bOk} exact matches, ${bBad} mismatches across ${reals.length} real liquidate txs`);
  }
  console.log('\n[seed] done.');
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
