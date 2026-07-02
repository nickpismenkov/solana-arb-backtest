// What we backtest. Start narrow (one deep pair, two real AMMs) and widen later.

export interface Token {
  symbol: string;
  mint: string;
  decimals: number;
}

export const TOKENS: Record<string, Token> = {
  SOL: { symbol: 'SOL', mint: 'So11111111111111111111111111111111111111112', decimals: 9 },
  USDC: { symbol: 'USDC', mint: 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v', decimals: 6 },
};

// We price BASE in QUOTE (USDC per SOL). BASE is the Bitquery `Currency`, QUOTE
// is the `Side.Currency`.
export const BASE = TOKENS.SOL;
export const QUOTE = TOKENS.USDC;

// Real AMM venues (Bitquery `Trade.Dex.ProtocolFamily`). Aggregators (Jupiter,
// DFlow, BisonFi) are excluded — they route to these pools, so comparing them
// is the "can't beat the aggregator" trap. Note Orca's exact spelling.
export const VENUES = ['Raydium', 'OrcaWhirpool', 'Meteora'];

export const PARAMS = {
  // The two venues we compare for spatial arb.
  venuePair: ['Raydium', 'OrcaWhirpool'] as [string, string],
  // Assumed notional per opportunity (USD). Theoretical gross scales with this;
  // it's an assumption, not measured pool depth.
  sizeUsd: Number(process.env.SIZE_USD || 10_000),
  // Aggregation bucket in minutes (Bitquery reliably buckets per minute; finer
  // needs per-trade retrieval — a later fidelity step).
  intervalMinutes: Number(process.env.INTERVAL_MINUTES || 1),
  // Drop dust: only count trades at/above this notional (USD). Tiny swaps have
  // terrible effective prices that pollute the signal and aren't arbable anyway.
  minUsd: Number(process.env.MIN_USD || 1000),
  // Sanity ceiling (bps): a cross-venue "spread" above this is treated as a data
  // artifact (dust/extreme slippage), not a real arb, and ignored. Real SOL/USDC
  // cross-venue gaps are well under this.
  sanityMaxBps: Number(process.env.SANITY_MAX_BPS || 300),
  // Pull the day in chunks to stay well under any per-query row cap.
  chunkHours: 6,
  // Round-trip cost thresholds to sweep (bps): both venues' swap fees + our fee
  // + gas + slippage. A gap only counts as an opportunity above threshold.
  costSweepBps: [5, 10, 20, 30, 50],
};

// Yesterday in UTC as [start, end) ISO strings.
export function yesterdayUtc(): { after: string; before: string; label: string } {
  const now = new Date();
  const end = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate()));
  const start = new Date(end.getTime() - 24 * 3600 * 1000);
  return { after: start.toISOString(), before: end.toISOString(), label: start.toISOString().slice(0, 10) };
}
