# arb-engine

Rust HFT engine for Solana cross-venue arbitrage research: shred-triggered
detection on Orca Whirlpool ↔ Raydium CLMM, built to answer "can a newcomer
profitably capture cross-venue gaps?" with measurements, not guesses.

Three legs of speed:

1. **L1 — fastest feed:** Jito ShredStream (UDP shreds, ahead of Turbine),
   pure-Rust `shredstream` crate. ALT resolution gives full coverage of
   aggregator-routed swaps.
2. **L2 — fastest detect:** Rust hot path — pool-account decode
   (sqrtPrice Q64.64 for both venues), fee-adjusted cross-venue detector.
3. **L3 — fastest landing:** Jito bundles (plumbing in `jito.rs`); a guarded
   backrun tx reverts if unprofitable, so losses are bounded at fees+tip.

## Binaries

| bin | what it does |
|---|---|
| `shadow` | gRPC price feed (Turbine-sourced) + fee-adjusted arb detector; reports arb count / peak net / lifetime |
| `shred_probe` | standalone box check: binds the ShredStream UDP port, prints txn/pool-hit heartbeats |
| `decode_probe` | inspects shred-carried txs: top-level programs, ALT resolution, swap discriminators |
| `tickarray_probe` | pool-state reader + tick-array PDA derivation (verified on-chain) |
| `jito_probe` | Jito block-engine plumbing: getTipAccounts + sendBundle round-trip |
| `backrun_probe` | **the go/no-go measurement** — on each shred trigger, simulates the victim tx and measures the residual cross-venue gap a backrun could capture |

## Config

`.env` (see `.env.example`): `GRPC_ENDPOINT` / `GRPC_X_TOKEN` (Tatum
yellowstone gRPC), `SHREDSTREAM_PORT`, `RPC_ENDPOINT` (JSON-RPC for sims +
pool-state reads), `JITO_BLOCK_ENGINE`, `RUN_MS`.

## Status / findings

Every stage of the research (historical ceiling, competition census, Jito tip
economics, live spread parity, shadow runs, and finally `backrun_probe` on a
co-located box at 2 ms) points the same way:

**NO-GO on liquid SOL/USDC.** 600 s co-located run: 85 victim sims applied,
residual post-victim gap median 0.1 bp / max 3.5 bp, 0/85 cleared the 8 bp fee
floor. Routing bots arb these pools atomically — the edge is gone before it
exists for a shred-reactor, at any speed.

Untested frontier: thin/long-tail pairs (re-point the same tooling by swapping
pool addresses + decoders).

Pools (SOL/USDC): Orca Whirlpool `Czfq3xZZDmsdGdUyrNLtRhGc47cXcZtLG4crryfu44zE`,
Raydium CLMM `3ucNos4NbumPLZNWztqGHNFFgkHeRMBQAVemeeomsUxv` (both 4 bp fee →
8 bp threshold).

See `RUN.md` for the on-box runbook.
