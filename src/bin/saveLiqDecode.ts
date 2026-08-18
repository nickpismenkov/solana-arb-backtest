// Port of src/bin/save_liq_decode.rs
//
// Recon for the Save (formerly Solend) liquidation integration: derive the
// liquidate instruction from captured mainnet truth (the marginfi/Kamino
// lesson). Save is the original SPL token-lending model — a NATIVE program, so
// each instruction is identified by its first data byte (a u8 tag), not an
// 8-byte Anchor discriminator.
//
// Two passes over recent program txs: (1) histogram the instruction tags to
// see what exists and how often, (2) dump the first example of each tag with
// full account list + data, so we can identify the liquidate ix (the classic
// LiquidateObligation is tag 12; Solend's atomic
// LiquidateObligationAndRedeemReserveCollateral is a later tag) and its exact
// account layout before building anything.
//
// Usage: HELIUS_RPC=<url> [PAGES=3] npx tsx src/bin/saveLiqDecode.ts

import 'dotenv/config';
import bs58 from 'bs58';

const SOLEND_PROGRAM = 'So1endDq2YkqhipRh3WViPa8hdiSpxWy6z3Z6tMCpAo';

async function rpc(endpoint: string, body: unknown): Promise<any | null> {
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
  return null;
}

async function main(): Promise<void> {
  const endpoint = process.env.HELIUS_RPC ?? process.env.RPC_HTTP;
  if (endpoint === undefined) throw new Error('HELIUS_RPC');
  const pages = Number.parseInt(process.env.PAGES ?? '', 10) || 3;

  // Page back through recent program signatures.
  const sigs: string[] = [];
  let before: string | undefined;
  for (let i = 0; i < pages; i++) {
    const params: any = { limit: 1000 };
    if (before !== undefined) params.before = before;
    const resp = await rpc(endpoint, {
      jsonrpc: '2.0',
      id: 1,
      method: 'getSignaturesForAddress',
      params: [SOLEND_PROGRAM, params],
    });
    const page: any[] = resp?.result ?? [];
    if (page.length === 0) break;
    before = page[page.length - 1]?.signature;
    for (const e of page) {
      if (e?.err === null && typeof e?.signature === 'string') sigs.push(e.signature);
    }
    console.error(`[save] paged: ${sigs.length} sigs`);
  }

  // Targeted: dump the FULL tx for the liquidate tags only —
  // 12 = LiquidateObligation, 17 = LiquidateObligationAndRedeemReserveCollateral.
  // Print every Solend ix in the tx (tag + accounts + data) so we get the
  // liquidate account layout AND the surrounding refresh_reserve/obligation ixs.
  const want = [12, 17];
  const target = Number.parseInt(process.env.TARGET ?? '', 10) || 3;
  let found = 0;
  for (const sig of sigs) {
    if (found >= target) break;
    const tx = await rpc(endpoint, {
      jsonrpc: '2.0',
      id: 1,
      method: 'getTransaction',
      params: [sig, { encoding: 'jsonParsed', maxSupportedTransactionVersion: 0, commitment: 'confirmed' }],
    });
    const result = tx?.result;
    if (result === null || result === undefined) continue;
    const ixs: any[] = [...(result?.transaction?.message?.instructions ?? [])];
    for (const inner of result?.meta?.innerInstructions ?? []) {
      ixs.push(...(inner?.instructions ?? []));
    }
    const hasLiq = ixs.some((ix) => {
      if (ix?.programId !== SOLEND_PROGRAM) return false;
      try {
        const data = bs58.decode(typeof ix?.data === 'string' ? ix.data : '');
        return data.length > 0 && want.includes(data[0]);
      } catch {
        return false;
      }
    });
    if (!hasLiq) {
      await new Promise((r) => setTimeout(r, 20));
      continue;
    }
    found += 1;
    console.log(`\n════════ LIQUIDATION tx #${found}: ${sig}`);
    console.log(`  fee payer: ${result?.transaction?.message?.accountKeys?.[0]?.pubkey}`);
    for (const ix of ixs) {
      if (ix?.programId !== SOLEND_PROGRAM) continue;
      let data: Uint8Array;
      try {
        data = bs58.decode(typeof ix?.data === 'string' ? ix.data : '');
      } catch {
        continue;
      }
      if (data.length === 0) continue;
      const tag = data[0];
      const name =
        tag === 3
          ? 'RefreshReserve'
          : tag === 7
            ? 'RefreshObligation'
            : tag === 12
              ? 'LiquidateObligation'
              : tag === 17
                ? 'LiquidateObligationAndRedeemReserveCollateral'
                : '?';
      const hex = Array.from(data)
        .map((b) => b.toString(16).padStart(2, '0'))
        .join('');
      console.log(`  ── tag ${tag} ${name}  (${data.length}B data)  data=${hex}`);
      const accts: any[] = ix?.accounts ?? [];
      accts.forEach((a, i) => {
        console.log(`      [${i.toString().padStart(2, ' ')}] ${typeof a === 'string' ? a : '?'}`);
      });
    }
  }
  if (found === 0) console.log(`no liquidation (tag 12/17) in ${sigs.length} sigs — raise PAGES`);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
