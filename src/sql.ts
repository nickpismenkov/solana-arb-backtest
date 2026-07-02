// SQL builders for Flipside's decoded Solana swaps (solana.defi.ez_dex_swaps).
// Window = "yesterday" in UTC: [today-1 00:00, today 00:00).

import { PAIR, VENUES } from './config.js';

const SOL = PAIR.base.mint;
const USDC = PAIR.quote.mint;
const venueList = VENUES.map((v) => `'${v}'`).join(', ');

const YESTERDAY = `
  block_timestamp >= DATEADD('day', -1, DATE_TRUNC('day', SYSDATE()))
  AND block_timestamp <  DATE_TRUNC('day', SYSDATE())
`;

const PAIR_FILTER = `
  (
    (swap_from_mint = '${SOL}'  AND swap_to_mint = '${USDC}') OR
    (swap_from_mint = '${USDC}' AND swap_to_mint = '${SOL}')
  )
`;

// Cheap schema/scale probe: which platforms traded this pair yesterday, how many
// swaps each, and the time span. Confirms the `platform` values + that the table
// and columns are what we expect before we pull the full series.
export function probeSql(): string {
  return `
    SELECT platform,
           COUNT(*)                AS swaps,
           MIN(block_timestamp)    AS first_swap,
           MAX(block_timestamp)    AS last_swap
    FROM solana.defi.ez_dex_swaps
    WHERE ${YESTERDAY}
      AND ${PAIR_FILTER}
      AND swap_from_amount > 0 AND swap_to_amount > 0
    GROUP BY platform
    ORDER BY swaps DESC
    LIMIT 100
  `;
}

// A handful of raw rows so we can eyeball column names + price magnitude
// (USDC/SOL should look like ~$100–300; if it's ~1000x off, amounts are raw and
// we need to divide by 10^(9-6)).
export function sampleSql(): string {
  return `
    SELECT block_timestamp, platform, swap_from_mint, swap_from_amount,
           swap_to_mint, swap_to_amount
    FROM solana.defi.ez_dex_swaps
    WHERE ${YESTERDAY}
      AND ${PAIR_FILTER}
      AND platform IN (${venueList})
      AND swap_from_amount > 0 AND swap_to_amount > 0
    ORDER BY block_timestamp
    LIMIT 10
  `;
}

// Per-(bucket, venue) last trade price, as USDC per SOL. Aggregating to the
// bucket keeps the result small enough for one page while preserving the
// spread signal.
export function seriesSql(bucketSeconds: number): string {
  const bucket =
    bucketSeconds <= 1
      ? `DATE_TRUNC('second', block_timestamp)`
      : `TO_TIMESTAMP(FLOOR(DATE_PART('epoch_second', block_timestamp) / ${bucketSeconds}) * ${bucketSeconds})`;
  return `
    WITH swaps AS (
      SELECT
        ${bucket} AS ts,
        block_timestamp,
        platform,
        CASE
          WHEN swap_from_mint = '${SOL}' THEN swap_to_amount   / swap_from_amount
          ELSE                                 swap_from_amount / swap_to_amount
        END AS price_usdc_per_sol
      FROM solana.defi.ez_dex_swaps
      WHERE ${YESTERDAY}
        AND ${PAIR_FILTER}
        AND platform IN (${venueList})
        AND swap_from_amount > 0 AND swap_to_amount > 0
    )
    SELECT ts, platform, price_usdc_per_sol
    FROM swaps
    QUALIFY ROW_NUMBER() OVER (PARTITION BY ts, platform ORDER BY block_timestamp DESC) = 1
    ORDER BY ts
  `;
}
