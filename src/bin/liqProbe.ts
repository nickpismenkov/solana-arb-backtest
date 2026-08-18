// Port of src/bin/liq_probe.rs
//
// Liquidation opportunity probe (read-only). For each lending/perps protocol,
// scan recent program transactions, identify LIQUIDATIONS (by instruction
// discriminator at top-level or via CPI, or by a log marker), and measure:
// frequency (→ liquidations/day), liquidator concentration (are a few bots
// winning everything?), and a rough profit proxy (fee-payer USDC delta). Tells
// us which protocols are worth building an adapter for, before we build one.
//
// Discriminators/program IDs are VERIFIED per protocol before trust (marginfi
// computed; others pending research). Read-only, no money.
//
// Usage: RPC_ENDPOINT=<helius> [LIMIT=1000] [SLEEP_MS=25] tsx src/bin/liqProbe.ts

import 'dotenv/config';
import bs58 from 'bs58';

const USDC = 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v';

interface Protocol {
  name: string;
  program: string;
  discs: Buffer[]; // Anchor liquidation ix discriminators (8 bytes each)
  tags: number[]; // non-Anchor: match instruction data[0]
  logMarkers: string[]; // fallback: log substrings
}

// Verified program IDs + discriminators (research + local sha256). Profit note:
// Kamino/Solend expose liquidator gain in token balances; marginfi/Drift keep it
// as internal share/margin deltas → our USDC-delta proxy under-reads those (the
// reliable signals there are frequency + liquidator concentration).
function protocols(): Protocol[] {
  return [
    {
      name: 'kamino-klend',
      program: 'KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD',
      discs: [
        Buffer.from([177, 71, 154, 188, 226, 133, 74, 55]),
        Buffer.from([162, 161, 35, 143, 30, 187, 185, 103]),
      ],
      tags: [],
      logMarkers: ['LiquidateObligationAndRedeemReserveCollateral'],
    },
    {
      name: 'marginfi-v2',
      program: 'MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA',
      discs: [Buffer.from([214, 169, 151, 213, 251, 167, 86, 219])],
      tags: [],
      logMarkers: ['LendingAccountLiquidate'],
    },
    {
      name: 'drift-v2',
      program: 'dRiftyHA39MWEi3m9aunc5MzRF1JYuBsbn6VPcn33UH',
      discs: [
        Buffer.from([75, 35, 119, 247, 191, 18, 139, 2]), // liquidate_perp
        Buffer.from([107, 0, 128, 41, 35, 229, 251, 18]), // liquidate_spot
        Buffer.from([95, 111, 124, 105, 86, 169, 187, 34]), // liquidate_perp_with_fill
      ],
      tags: [],
      logMarkers: [],
    },
    {
      name: 'save-solend',
      program: 'So1endDq2YkqhipRh3WViPa8hdiSpxWy6z3Z6tMCpAo',
      discs: [],
      tags: [12, 17], // LiquidateObligation / …AndRedeemReserveCollateral
      logMarkers: ['LiquidateObligation'],
    },
  ];
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

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
    await sleep(300 << attempt);
  }
  return undefined;
}

async function recentSigs(endpoint: string, program: string, limit: number): Promise<string[]> {
  const v = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getSignaturesForAddress',
    params: [program, { limit }],
  });
  const arr: any[] = Array.isArray(v?.result) ? v.result : [];
  return arr
    .filter((e) => e?.err === null || e?.err === undefined)
    .map((e) => e?.signature)
    .filter((s): s is string => typeof s === 'string');
}

function disc8(b58: string): Buffer | undefined {
  let bytes: Uint8Array;
  try {
    bytes = bs58.decode(b58);
  } catch {
    return undefined;
  }
  if (bytes.length < 8) return undefined;
  return Buffer.from(bytes.subarray(0, 8));
}

function pct(sorted: number[], p: number): number {
  if (sorted.length === 0) return 0.0;
  const idx = Math.round((sorted.length - 1) * p);
  return sorted[idx];
}

async function main(): Promise<void> {
  const endpoint = process.env.RPC_ENDPOINT;
  if (!endpoint) throw new Error('RPC_ENDPOINT (use Helius)');
  const limit = Number.parseInt(process.env.LIMIT ?? '', 10) || 1000;
  const sleepMs = Number.parseInt(process.env.SLEEP_MS ?? '', 10) || 25;

  for (const p of protocols()) {
    console.error(`\n═══ ${p.name} (${p.program.slice(0, 8)}) — scanning ${limit} recent program txns ═══`);
    const sigs = await recentSigs(endpoint, p.program, limit);
    if (sigs.length === 0) {
      console.log('  no signatures (program not found / RPC issue)');
      continue;
    }

    let liqs = 0;
    let minT = Number.MAX_SAFE_INTEGER;
    let maxT = 0;
    const byLiquidator = new Map<string, number>();
    let profits: number[] = [];
    let scanned = 0;

    const checkIx = (ix: any): boolean => {
      if (ix?.programId !== p.program) return false;
      const data = ix?.data;
      if (typeof data !== 'string') return false;
      // Anchor: 8-byte discriminator. Non-Anchor (Solend): data[0] tag.
      const d = disc8(data);
      if (d !== undefined && p.discs.some((pd) => pd.equals(d))) return true;
      try {
        const decoded = bs58.decode(data);
        if (decoded.length > 0 && p.tags.includes(decoded[0])) return true;
      } catch {
        // ignore
      }
      return false;
    };

    for (const sig of sigs) {
      await sleep(sleepMs);
      const v = await rpc(endpoint, {
        jsonrpc: '2.0',
        id: 1,
        method: 'getTransaction',
        params: [sig, { encoding: 'jsonParsed', maxSupportedTransactionVersion: 0, commitment: 'confirmed' }],
      });
      if (v === undefined) continue;
      const r = v?.result;
      if (r === null || r === undefined) continue;
      if (r?.meta?.err !== null && r?.meta?.err !== undefined) continue;
      scanned += 1;

      // Is this a liquidation? Check top-level + inner instructions for our
      // program + a liquidation discriminator, or a log marker.
      let isLiq = false;
      const topIxs: any[] = Array.isArray(r?.transaction?.message?.instructions) ? r.transaction.message.instructions : [];
      for (const ix of topIxs) {
        if (checkIx(ix)) {
          isLiq = true;
          break;
        }
      }
      if (!isLiq) {
        const innerGroups: any[] = Array.isArray(r?.meta?.innerInstructions) ? r.meta.innerInstructions : [];
        outer: for (const grp of innerGroups) {
          const innerIxs: any[] = Array.isArray(grp?.instructions) ? grp.instructions : [];
          for (const ix of innerIxs) {
            if (checkIx(ix)) {
              isLiq = true;
              break outer;
            }
          }
        }
      }
      if (!isLiq && p.logMarkers.length > 0) {
        const logs: string[] = Array.isArray(r?.meta?.logMessages)
          ? r.meta.logMessages.filter((l: unknown): l is string => typeof l === 'string')
          : [];
        const joined = logs.join('\n');
        if (p.logMarkers.some((m) => joined.includes(m))) isLiq = true;
      }
      if (!isLiq) continue;

      liqs += 1;
      const t = r?.blockTime;
      if (typeof t === 'number') {
        minT = Math.min(minT, t);
        maxT = Math.max(maxT, t);
      }
      const payer: string = r?.transaction?.message?.accountKeys?.[0]?.pubkey ?? '';
      byLiquidator.set(payer, (byLiquidator.get(payer) ?? 0) + 1);
      // Rough profit proxy: fee-payer USDC delta.
      const sum = (key: string): number => {
        const arr: any[] = Array.isArray(r?.meta?.[key]) ? r.meta[key] : [];
        return arr
          .filter((b) => b?.mint === USDC && b?.owner === payer)
          .map((b) => b?.uiTokenAmount?.uiAmount)
          .filter((x): x is number => typeof x === 'number')
          .reduce((a, b) => a + b, 0);
      };
      profits.push(sum('postTokenBalances') - sum('preTokenBalances'));
    }

    const spanH = maxT > minT ? (maxT - minT) / 3600.0 : 0.0;
    const perDay = spanH > 0.0 ? (liqs / spanH) * 24.0 : 0.0;
    console.log(`  scanned ${scanned} txns → ${liqs} liquidations over ${spanH.toFixed(1)}h  (~${perDay.toFixed(0)}/day)`);
    if (liqs === 0) {
      console.log('  → none found in window (disc/marker may need fixing, or genuinely rare here)');
      continue;
    }
    // Liquidator concentration.
    const top = Array.from(byLiquidator.entries());
    top.sort((a, b) => b[1] - a[1]);
    const total = Array.from(byLiquidator.values()).reduce((a, b) => a + b, 0);
    console.log(`  distinct liquidators: ${byLiquidator.size}  | top 3 share:`);
    for (const [who, n] of top.slice(0, 3)) {
      console.log(`    ${who.slice(0, Math.min(8, who.length))}… ${n} (${((100.0 * n) / total).toFixed(0)}%)`);
    }
    profits = profits.filter((x) => x > 0.0);
    profits.sort((a, b) => a - b);
    if (profits.length > 0) {
      console.log(
        `  liquidator USDC gain (rough, n=${profits.length}): med $${pct(profits, 0.5).toFixed(2)}  p90 $${pct(profits, 0.9).toFixed(2)}  max $${pct(profits, 1.0).toFixed(2)}`,
      );
    }
  }
  console.log();
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
