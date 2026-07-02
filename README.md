# solana-arb-backtest

Backtesting whether cross-venue arbitrage on Solana is worth pursuing — starting
with **Stage 1: how many opportunities existed in the last 24h, and the
theoretical gross if you won all of them.**

This is a research pipeline, separate from the `oblivion` app.

## Approach (and honest limits)

Historical **DEX swap** data (Flipside `solana.defi.ez_dex_swaps`) → per-venue
last-price series → detect where two venues diverge beyond a cost threshold →
count opportunities + sum theoretical profit.

What this **can** tell you: whether gaps exist, how big, how often, and the
ceiling profit if you captured every one.

What it **can't**: whether *you* would win them. Capture is a latency + Jito
auction race that historical data doesn't encode. That's Stage 2 (track actual
winners + tips), then Stage 3 (live shadow test). "Won all of them" here is a
strict upper bound, not a forecast.

Additional caveats baked into the numbers:

- Price = last **executed trade** price per time bucket (a proxy for pool mid).
- Gross scales with an **assumed notional** (`SIZE_USD`, default $10k) — real
  capturable size is bounded by pool depth, which trades alone don't give us.
- Sub-bucket (sub-second) gaps are under-counted at the default 1s bucket.

## Setup

```bash
npm install
cp .env.example .env      # then paste your FLIPSIDE_API_KEY (free: flipsidecrypto.xyz)
```

## Run

```bash
npm run probe      # confirm which platforms traded the pair + sanity-check columns/price
npm run backtest   # 24h opportunity count + theoretical net across a sweep of cost thresholds
```

Config (pair, venues, size, buckets, cost sweep) lives in `src/config.ts`.
`VENUES` is a first guess — run `probe` and correct it to the real `platform`
values before trusting `backtest`.

## Alternative data source

Built for **Flipside** (free arbitrary-SQL API). If you have a **Dune** key
instead (`dex_solana.trades`), the query layer (`src/sql.ts` + `src/flipside.ts`)
is small and swappable.
