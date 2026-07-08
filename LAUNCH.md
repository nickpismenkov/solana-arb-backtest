# Liquidation executor — launch runbook

Two production liquidation executors, both **DRY_RUN by default** (detect, size,
quote, full-simulate, log "would fire" — never submit). They need **no
inventory capital**: each fire is one atomic flash-loan-wrapped transaction that
is profit-or-revert (the repay/payback leg fails unless the seized collateral
covered the debt, so an unprofitable fire reverts for only the base fee, and the
tip is in-tx so it's paid only on landing).

| Executor | Bin | Protocol | Debt (v1) |
|----------|-----|----------|-----------|
| marginfi | `liq_executor` | marginfi v2 | USDC |
| Kamino   | `liq_kamino_executor` | KLend main market | USDC |

## What's verified

- **Fire paths** simulate end-to-end on mainnet against real accounts. With a
  healthy market (≈0 genuinely liquidatable now) the liquidate reverts on the
  protocol's own health/eligibility gate — proving every upstream leg (flash
  borrow, refresh, liquidate account wiring, Jupiter swap, repay) composes and
  the tx fits under 1232 bytes (marginfi 837 B, Kamino 842 B, via dedicated
  ALTs). The first genuinely liquidatable account will run the whole tx.
- **Oracle coverage** (marginfi): 106/140 banks priced (Pyth + Switchboard),
  76,705 fully-priced borrowers.
- **Both executors** boot, scan, build a watch-set, and poll cleanly with zero
  false fires.
- **Lazer pre-positioning** blend is unit-tested; live feed verified previously
  on the box via `pyth_probe` (token lives in the box `.env`).
- **Self-crank bundle** (marginfi, the stale-oracle edge): marginfi's Pyth
  feeds lag the true price by 8–44 s; when an account is underwater at the true
  (Lazer) price but healthy on-chain, `liq_executor` fires an atomic Jito
  bundle `[crank_setup, crank_fire, liquidate]` that posts the fresh Hermes
  price and liquidates against it in one shot. Verified live via
  `pyth_crank_probe` (sponsored feed's publish_time advanced 21 s in
  simulateBundle) and `liq_crank_probe` (3/3 real borrower candidates: crank
  landed, marginfi judged the liquidate AT the cranked price). 42/140 banks
  have crankable (shard-0 sponsored) oracles, incl. SOL. Bundles are
  all-or-nothing: a losing fire never lands and pays nothing.

## Preconditions

1. **Wallet** `DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak` (keypair
   `~/arb-keypair.json`) funded. Currently ~0.05 SOL — **top up before sustained
   live running** (SOL only covers tips + base fees + one-time ATA rents;
   principal is flash-borrowed). Suggest ≥ 0.3 SOL.
2. **marginfi account** `B6e37TbC5n56tWbcgC3RRafUXSuEwRz9ZbhL8Ksro6vD` exists
   (created earlier) — required by the marginfi liquidate flash-loan wrap.
3. **ALTs** live on-chain (authority = wallet):
   marginfi `DEMhLvSJbSZQfCdiH7YicYNopo3EhhapjfoEjt2kJVij`,
   Kamino `6X77KtDupVYqU4SBjWsY93ycFW2bPm3AWpAuPWfxraKo`.
4. **`.env`** on the box: `HELIUS_RPC`, `KEYPAIR_PATH`, `PYTH_LAZER_TOKEN`
   (optional but required for the self-crank edge), `ALERT_WEBHOOK` (optional).
   See `.env.example`.
5. **Crank float**: a live crank bundle fronts ~0.008 SOL rent for the
   encoded-VAA buffer, reclaimed in the same bundle (`close_encoded_vaa`) —
   covered by the ≥ 0.3 SOL wallet suggestion, no separate step.

## Step 1 — DRY_RUN shadow (safe, no money)

Run both on the box for a session. They log to `runs/decisions.jsonl` +
`runs/trades.jsonl`. A `★ … LIQUIDATABLE` line with a positive est-profit that
passes the full-sim gate is a would-fire — that's the signal the market is
producing takeable liquidations we'd catch.

```
./runs/liq.sh                 # marginfi, DRY_RUN=1
./runs/liq_kamino.sh          # Kamino, DRY_RUN=1
PYTH_LAZER_TOKEN=<tok> cargo run --release --bin lazer_probe   # confirm Lazer leads on-chain
```

Watch for: watch-set size > 0, poll cycles running, no repeated build/sim
errors. Zero would-fires over a session = healthy market (expected recently),
not a bug — the sim gate is ground truth.

## Step 2 — go live (real money)

Only after a clean DRY_RUN session and a funded wallet:

```
DRY_RUN=0 ./runs/liq.sh
DRY_RUN=0 ./runs/liq_kamino.sh
```

Live safety, all already enforced: profit gate (`MIN_PROFIT_USD`), profit-
proportional tip floored at Sender's 0.0002 SOL min, **landed-only** daily tip
cap (`MAX_DAILY_TIP_SOL`), wallet floor (`WALLET_MIN_SOL`), and the atomic
profit-or-revert guard. A losing fire that somehow lands costs only the base
fee; the tip reverts with it.

## Monitor

- `runs/decisions.jsonl` — every candidate: ratio, sizing, quote, est profit,
  sim result, fired/skipped + reason.
- `runs/trades.jsonl` — submissions + realized on-chain USDC P&L (readback
  thread) + bundle/landing status.
- Alerts (if `ALERT_WEBHOOK` set): `*-landed`, `*-miss`, `*-cap`, `*-floor`.

## Kill switch

`Ctrl-C` (or `pkill -f liq_executor` / `pkill -f liq_kamino_executor`). Nothing
persists between fires; stopping is instant and safe.

## Known limits (honest)

- **Reaction speed**: with Lazer + self-crank, the marginfi path detects the
  true-price cross in ms and fires a crank+liquidate bundle within ~2–4 s
  (Jupiter quote + bundle sims on the inline arm) — inside the 8–44 s stale
  window, i.e. before poll-based bots can even see the opportunity on-chain.
  Competing crank-liquidators exist (we copied the pattern from one); winning
  contested events is a tip auction. Without Lazer (poll mode) reaction is
  ~5–10 s and crank mode never triggers.
- **Crank scope**: only banks whose oracle is the shard-0 sponsored feed
  (42/140, incl. SOL) can be cranked; emode positions still phantom out at the
  sim gate; the crank fire tx fits ONE feed update — multi-feed opportunities
  crank the asset feed only (liab = USDC, cranked constantly by others).
- **v1 scope**: USDC debt, single-collateral/single-debt, non-elevation
  (marginfi) / non-elevation-group (Kamino) positions. Others are logged +
  skipped. Multi-position and non-USDC debt are the next extension.
- **Kamino collateral** outside the top-20-by-deposit reserves falls back to
  inline accounts and may exceed the size limit → logged + skipped (extend the
  ALT to cover more).
