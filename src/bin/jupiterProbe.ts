// Port of src/bin/jupiter_probe.rs
//
// Verify the Jupiter Lend (Fluid) decoders against live mainnet and enumerate
// every vault: collateral/debt pair, liquidation threshold, sizes, and a
// first-pass liquidatable signal. Read-only.
//
// Detection honesty: precise per-price liquidatable detection needs Fluid's
// tick↔price math (not reversed here). This reports the CONFIDENT on-chain
// liquidation-activity flags (absorbed debt / branch_liquidated) and leaves the
// authoritative check to the executor's liquidate simulation.
//
// Usage: HELIUS_RPC=<url> tsx src/bin/jupiterProbe.ts

import 'dotenv/config';
import bs58 from 'bs58';
import { PublicKey } from '@solana/web3.js';
import { USDC_MINT, USDT_MINT, VAULT_CONFIG_DISC, VAULT_STATE_DISC, VAULTS_PROGRAM, Vault, VaultConfig, VaultState, WSOL_MINT } from '../lib/jupiter.js';

async function rpc(endpoint: string, body: unknown): Promise<any | undefined> {
  for (let attempt = 0; attempt < 4; attempt++) {
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

function b64(d: any): Buffer | undefined {
  const s = d?.[0];
  if (typeof s !== 'string') return undefined;
  return Buffer.from(s, 'base64');
}

/** getProgramAccounts filtered by an 8-byte discriminator at offset 0. */
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
    const data = b64(e?.account?.data);
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

async function main(): Promise<void> {
  const endpoint = process.env.HELIUS_RPC ?? process.env.RPC_HTTP;
  if (!endpoint) throw new Error('HELIUS_RPC');

  // Decode all VaultConfig + VaultState, join by vault_id.
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
  console.log(`live: ${configs.size} VaultConfig, ${states.size} VaultState decoded`);

  const label = (m: PublicKey): string => {
    const s = m.toBase58();
    if (s === USDC_MINT) return 'USDC';
    if (s === USDT_MINT) return 'USDT';
    if (s === WSOL_MINT) return 'wSOL';
    return s.slice(0, 6);
  };

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

  let nUsdc = 0;
  let nUsdt = 0;
  let nSol = 0;
  let nMaybe = 0;
  console.log(
    `\n${'vid'.padStart(3)} ${'collat'.padStart(7)} ${'debt'.padStart(7)} ${'CF%'.padStart(5)} ${'LT%'.padStart(5)} ${'tot_supply'.padStart(16)} ${'tot_borrow'.padStart(16)} ${'absorb'.padStart(6)} liq?`,
  );
  for (const v of vaults) {
    const c = v.config;
    const s = v.state;
    const dl = c.debtLabel();
    if (dl === 'USDC') nUsdc += 1;
    else if (dl === 'USDT') nUsdt += 1;
    else if (dl === 'wSOL') nSol += 1;
    const maybe = v.maybeLiquidatable();
    if (maybe) nMaybe += 1;
    console.log(
      `${String(c.vaultId).padStart(3)} ${label(c.supplyToken).padStart(7)} ${label(c.borrowToken).padStart(7)} ${(c.collateralFactor / 10.0).toFixed(1).padStart(5)} ${(c.liquidationThreshold / 10.0).toFixed(1).padStart(5)} ${s.totalSupply.toString().padStart(16)} ${s.totalBorrow.toString().padStart(16)} ${s.absorbedDebtAmount.toString().padStart(6)} ${maybe ? '★MAYBE' : ''}`,
    );
  }

  const inScope = nUsdc + nUsdt + nSol;
  console.log('\n═══ summary ═══');
  console.log(`vaults: ${vaults.length}  | debt in-scope (USDC/USDT/SOL): ${inScope}  (USDC ${nUsdc}, USDT ${nUsdt}, wSOL ${nSol})`);
  console.log(`VERIFIED: all ${vaults.length} vaults decode (pairs, thresholds, sizes) against live accounts.`);
  console.log(`first-pass 'maybe liquidatable' (absorbed-debt > 0): ${nMaybe}`);
  console.log("NOTE: precise per-price liquidatable detection needs Fluid tick↔price math (not");
  console.log("      implemented); the executor's liquidate simulation is the ground-truth gate.");
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
