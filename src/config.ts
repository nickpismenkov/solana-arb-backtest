// What we backtest. Start narrow (one deep pair, two venues) and widen later.

export interface Token {
  symbol: string;
  mint: string;
  decimals: number;
}

export const TOKENS: Record<string, Token> = {
  SOL: { symbol: 'SOL', mint: 'So11111111111111111111111111111111111111112', decimals: 9 },
  USDC: { symbol: 'USDC', mint: 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v', decimals: 6 },
};

// The pair we price as base/quote (price is quote-per-base, i.e. USDC per SOL).
export const PAIR = { base: TOKENS.SOL, quote: TOKENS.USDC };

// Flipside `platform` values to compare against each other. These are a guess —
// the `probe` command prints the real distribution so we can correct them.
export const VENUES = ['orca', 'raydium'];

export const PARAMS = {
  // Notional we assume we could push per opportunity, in USD. The theoretical
  // gross scales linearly with this; it's an assumption, not a measured depth.
  sizeUsd: Number(process.env.SIZE_USD || 10_000),
  // Time bucket for the per-venue last-price series (seconds). Smaller = finer
  // but more rows; 1s is a good start for a day.
  bucketSeconds: Number(process.env.BUCKET_SECONDS || 1),
  // Round-trip cost thresholds to sweep (bps): both venues' swap fees + our fee
  // + gas + slippage. A "spread" only counts as an opportunity above threshold.
  costSweepBps: [5, 10, 20, 30, 50],
};
