// Port of src/bin/mfi_health_debug.rs
//
// Root-cause the health divergence: for one account marginfi calls healthy but
// our maintenanceHealth calls underwater, print the per-bank breakdown (shares
// · share_value · price · our maint weight → contribution) and scan each bank's
// bytes for ALL "weight-like" i80f48 values (0<v<2) with offsets — revealing
// whether an emode boosted-weight config is present beyond the 4 config weights.
//
// Usage: HELIUS_RPC=<url> ACCOUNT=<pubkey> npx tsx src/bin/mfiHealthDebug.ts

import 'dotenv/config';
import { PublicKey } from '@solana/web3.js';
import {
  decodeBank,
  decodeMarginfiAccount,
  decodeOraclePrice,
  maintenanceHealth,
  healthRatio,
  healthLiquidatable,
  type Bank,
  type BankMap,
  type PriceMap,
} from '../lib/liquidation.js';

const DEFAULT_ACCOUNT = 'BH736MqzFt2dNMeytao6wDn9M1JtMYT2PJnrFxGzknUr';

async function rpc(endpoint: string, body: unknown): Promise<any | null> {
  for (let attempt = 0; attempt < 4; attempt++) {
    try {
      const resp = await fetch(endpoint, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify(body),
      });
      return await resp.json();
    } catch {
      /* retry */
    }
    await new Promise((r) => setTimeout(r, 400 << attempt));
  }
  return null;
}

function b64(d: any): Buffer | null {
  const s = d?.[0];
  if (typeof s !== 'string') return null;
  try {
    return Buffer.from(s, 'base64');
  } catch {
    return null;
  }
}

async function getMultiple(endpoint: string, keys: PublicKey[]): Promise<Map<string, Buffer>> {
  const out = new Map<string, Buffer>();
  const strs = keys.map((k) => k.toBase58());
  const v = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getMultipleAccounts',
    params: [strs, { encoding: 'base64' }],
  });
  const arr: any[] = Array.isArray(v?.result?.value) ? v.result.value : [];
  arr.forEach((acc, i) => {
    const b = b64(acc?.data);
    if (b) out.set(keys[i].toBase58(), b);
  });
  return out;
}

async function getOne(endpoint: string, pk: PublicKey): Promise<Buffer | null> {
  const v = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getAccountInfo',
    params: [pk.toBase58(), { encoding: 'base64' }],
  });
  return b64(v?.result?.value?.data);
}

function i80f48(bytes: Buffer, off: number): number | undefined {
  if (off + 16 > bytes.length) return undefined;
  let result = 0n;
  for (let i = 15; i >= 0; i--) {
    result = (result << 8n) | BigInt(bytes[off + i]);
  }
  const SIGN_BIT = 1n << 127n;
  if (result & SIGN_BIT) result -= 1n << 128n;
  return Number(result) / Number(1n << 48n);
}

async function main(): Promise<void> {
  const endpoint = process.env.HELIUS_RPC ?? process.env.RPC_HTTP;
  if (!endpoint) throw new Error('HELIUS_RPC');
  const account = new PublicKey(process.env.ACCOUNT ?? DEFAULT_ACCOUNT);

  const raw = await getOne(endpoint, account);
  if (!raw) throw new Error('account');
  const a = decodeMarginfiAccount(raw);
  if (!a) throw new Error('decode account');
  console.log(`account ${account.toBase58()}\n  authority ${a.authority.toBase58()}\n  ${a.balances.length} active balances`);

  // Account bytes after balances (1736..) — flags + emode region.
  console.log(`  ── account tail (post-balances @1736, len ${raw.length}) ──`);
  if (raw.length >= 1736) {
    const tail = raw.subarray(1736);
    if (tail.length >= 8) {
      const f = tail.readBigUInt64LE(0);
      console.log(`     account_flags @1736 = ${f} (0x${f.toString(16)})`);
    }
    // Scan the tail for u16 values that could be an emode_tag (small nonzero).
    const tags: Array<[number, number]> = [];
    for (let i = 0; i < tail.length - 2 && tags.length < 8; i += 2) {
      const v = tail.readUInt16LE(i);
      if (v > 0 && v < 4096) tags.push([1736 + i, v]);
    }
    console.log(`     small-u16 (emode_tag candidates) in tail: [${tags.map(([o, v]) => `(${o}, ${v})`).join(', ')}]`);
  }

  const bankPkSet = new Map<string, PublicKey>();
  for (const b of a.balances) bankPkSet.set(b.bankPk.toBase58(), b.bankPk);
  const bankPks = [...bankPkSet.values()];
  const bankRaw = await getMultiple(endpoint, bankPks);
  const banks: BankMap = new Map();
  for (const [pkStr, r] of bankRaw) {
    const bk = decodeBank(r);
    if (bk) banks.set(pkStr, bk);
  }

  // Prices from each bank's oracle.
  const oraclePkSet = new Map<string, PublicKey>();
  for (const bk of banks.values()) oraclePkSet.set(bk.oracleKey.toBase58(), bk.oracleKey);
  const oraclePks = [...oraclePkSet.values()];
  const oracleRaw = await getMultiple(endpoint, oraclePks);
  const priceOf = new Map<string, number>();
  for (const [pkStr, r] of oracleRaw) {
    const p = decodeOraclePrice(r);
    if (p !== null) priceOf.set(pkStr, p);
  }

  console.log('\n  ── per-balance health breakdown (our maintenance_health) ──');
  let wa = 0.0;
  let wl = 0.0;
  for (const b of a.balances) {
    const bankKey = b.bankPk.toBase58();
    const bank = banks.get(bankKey);
    if (!bank) {
      console.log(`    ${bankKey.slice(0, 8)} … BANK MISSING`);
      continue;
    }
    const price = priceOf.get(bank.oracleKey.toBase58()) ?? Number.NaN;
    const scale = 10 ** bank.mintDecimals;
    if (b.assetShares > 0.0) {
      const ui = (b.assetShares * bank.assetShareValue) / scale;
      const contrib = ui * price * bank.assetWeightMaint;
      wa += contrib;
      console.log(
        `    ASSET ${bankKey.slice(0, 8)}…  ui=${ui.toFixed(4)} price=$${price.toFixed(4)} w_maint=${bank.assetWeightMaint.toFixed(4)} (w_init=${bank.assetWeightInit.toFixed(4)}) → $${contrib.toFixed(2)}`,
      );
    }
    if (b.liabilityShares > 0.0) {
      const ui = (b.liabilityShares * bank.liabilityShareValue) / scale;
      const contrib = ui * price * bank.liabilityWeightMaint;
      wl += contrib;
      console.log(
        `    LIAB  ${bankKey.slice(0, 8)}…  ui=${ui.toFixed(4)} price=$${price.toFixed(4)} w_maint=${bank.liabilityWeightMaint.toFixed(4)} → $${contrib.toFixed(2)}`,
      );
    }
  }
  console.log(
    `  → [no-emode] weighted_assets $${wa.toFixed(2)}  weighted_liabilities $${wl.toFixed(2)}  ratio ${(wa > 0.0 ? wl / wa : Number.POSITIVE_INFINITY).toFixed(4)}  ${wa < wl ? 'UNDERWATER' : 'healthy'}`,
  );

  // Emode-aware verdict via the production maintenance_health (should match marginfi).
  const priceMap: PriceMap = new Map();
  for (const [pkStr, bk] of banks) {
    const p = priceOf.get(bk.oracleKey.toBase58());
    if (p !== undefined) priceMap.set(pkStr, p);
  }
  const r = maintenanceHealth(a, banks, priceMap);
  console.log(
    `  → [emode]    weighted_assets $${r.health.weightedAssets.toFixed(2)}  weighted_liabilities $${r.health.weightedLiabilities.toFixed(2)}  ratio ${healthRatio(r.health).toFixed(4)}  ${healthLiquidatable(r.health) ? 'UNDERWATER' : 'healthy'} (missing ${r.missing})`,
  );
  // What asset-weight boost on the collateral would make marginfi's verdict (healthy) consistent?
  if (wa > 0.0 && wa < wl) {
    console.log(`  → to be healthy, collateral asset-weight would need ≈${(wl / wa).toFixed(2)}× boost (emode?)`);
  }

  // Emode decode at the hypothesized layout: EmodeSettings starts @1240
  // (emode_tag u16), emode entries[10] start @1264, each 40 bytes:
  // collateral_bank_emode_tag u16 @0, asset_weight_init @8, asset_weight_maint @24.
  const EMODE_ENTRIES = 1264;
  const ENTRY_SIZE = 40;
  for (const [pkStr, rawbank] of bankRaw) {
    const role = a.balances.some((b) => b.bankPk.toBase58() === pkStr && b.liabilityShares > 0.0) ? 'LIAB' : 'ASSET';
    console.log(`\n  ── ${role} bank ${pkStr.slice(0, 8)} ──`);
    // Hunt for this bank's own emode_tag: print every u16 in 880..1268 so we
    // can see where 619/871 (the tags USDC references) sit for the collateral.
    const tagline: Array<[number, number]> = [];
    for (let i = 880; i < 1268; i += 2) {
      if (i + 2 > rawbank.length) break;
      const v = rawbank.readUInt16LE(i);
      if (v > 0 && v < 60000) tagline.push([i, v]);
    }
    console.log(`     u16 in 880..1268 (nonzero): [${tagline.map(([o, v]) => `(${o}, ${v})`).join(', ')}]`);
    // Entries (only the clean, in-range ones).
    for (let e = 0; e < 10; e++) {
      const base = EMODE_ENTRIES + e * ENTRY_SIZE;
      if (base + 2 > rawbank.length) break;
      const tag = rawbank.readUInt16LE(base);
      const init = i80f48(rawbank, base + 8) ?? 0.0;
      const maint = i80f48(rawbank, base + 24) ?? 0.0;
      if (init >= 0.0 && init <= 2.0 && maint >= 0.0 && maint <= 2.0 && (init > 0.0 || maint > 0.0)) {
        console.log(`     entry[${e}] collat_tag=${tag}  w_init=${init.toFixed(4)}  w_maint=${maint.toFixed(4)}`);
      }
    }
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
