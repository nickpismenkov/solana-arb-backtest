# Port plan: solana-arb-backtest (Rust) → TypeScript

Source repo: https://github.com/nickpismenkov/solana-arb-backtest (cloned to
`/tmp/solana-arb-backtest` during setup; re-clone if needed).

## Repo shape

Rust HFT research engine for Solana cross-venue arbitrage + lending-protocol
liquidation. Not a tiny repo — **97 `.rs` files, ~23,000 lines**:

- `src/*.rs` — 35 library modules, ~8,600 lines (shared engine logic).
- `src/bin/*.rs` — 62 standalone binaries, ~14,500 lines (probes, monitors,
  executors — mostly small `main()`s that wire library modules together for
  one specific experiment/venue).
- `Cargo.toml` — deps: tokio, yellowstone-grpc (gRPC pool feed), Jito
  shredstream, solana-* (pubkey/message/tx/ALT), bincode, serde, ureq.

This requires **parallel porting, split into 2 chunks** by domain, since the
codebase cleanly separates into two independent verticals that only share a
handful of low-level infra modules (`grpc.rs`, `lib.rs`, `main.rs`).

## Chunk A — DEX arbitrage & market-data infra (assigned to Worker A)

**Lib modules** (`src/*.rs`, ~4,540 lines):
`lib.rs`, `main.rs`, `grpc.rs`, `jito.rs`, `shredstream.rs`, `decode.rs`,
`detector.rs`, `signal.rs`, `execute.rs`, `observe.rs`, `pools.rs`, `swap.rs`,
`clmm.rs`, `arb.rs`, `jup.rs`, `jupiter.rs`, `jupiter_math.rs`,
`jupiter_fire.rs`, `flashloan.rs`, `pyth.rs`, `pyth_accumulator.rs`,
`pyth_crank.rs`, `lazer.rs`

**Binaries** (`src/bin/*.rs`, ~4,400 lines, 32 files):
`alt_deploy`, `arb_probe`, `backrun_probe`, `clmm_probe`, `decode_probe`,
`executor`, `flashloan_probe`, `flow_probe`, `grpc_latency_probe`,
`grpc_ping`, `history_probe`, `hotpath_bench`, `jito_probe`, `jup_alt_print`,
`jup_probe`, `jupiter_fire_probe`, `jupiter_probe`, `jupiter_seed_probe`,
`land_probe`, `lazer_probe`, `profit_watch`, `pyth_accum_probe`,
`pyth_crank_decode`, `pyth_crank_probe`, `pyth_probe`, `pyth_recv_decode`,
`report`, `shred_probe`, `swap_probe`, `tickarray_probe`, `verify_probe`,
`watcher`

Covers: gRPC/shredstream feeds, Jito bundle submission, Pyth/Lazer price
oracles, Orca/Raydium CLMM pool decode + cross-venue arb detection, Jupiter
swap/routing math. Output to `ts-port/src/lib/` (shared modules) and
`ts-port/src/bin/` (one entrypoint file per binary, prefixed to match source
name, e.g. `src/bin/shredProbe.ts`).

## Chunk B — Lending liquidation & pump.fun (assigned to Worker B)

**Lib modules** (`src/*.rs`, ~4,060 lines):
`kamino.rs`, `kamino_engine.rs`, `kamino_fire.rs`, `kamino_ix.rs`,
`marginfi.rs`, `save.rs`, `save_engine.rs`, `save_fire.rs`, `liquidation.rs`,
`liq_engine.rs`, `liq_fire.rs`, `pump.rs`

**Binaries** (`src/bin/*.rs`, ~10,100 lines, 30 files):
`kamino_alt_print`, `kamino_fire_probe`, `kamino_liq_decode`,
`kamino_liq_probe`, `liq_alt_print`, `liq_crank_probe`, `liq_executor`,
`liq_finder`, `liq_fire_probe`, `liq_jupiter_executor`, `liq_kamino`,
`liq_kamino_executor`, `liq_kamino_live`, `liq_kamino_monitor`,
`liq_marginfi_sim`, `liq_monitor`, `liq_probe`, `liq_race`, `liq_report`,
`liq_save_executor`, `liq_stream_executor`, `marginfi_probe`,
`mfi_health_debug`, `mfi_liq_census`, `mfi_multipos_probe`,
`mfi_oracle_census`, `mfi_pyth_decode`, `mfi_reject_audit`,
`mfi_stream_detect`, `mfi_watchset_value`, `pump_backtest`, `pump_census`,
`pump_collect`, `save_alt_print`, `save_fire_probe`, `save_liq_census`,
`save_liq_decode`, `save_overflag_probe`, `save_probe`

Covers: Kamino / MarginFi / Save (Solend) lending-account decode, health-ratio
computation, liquidation opportunity detection + execution, pump.fun
backtesting/census. Output to `ts-port/src/lib/` and `ts-port/src/bin/`
alongside Chunk A's files (different filenames, no collisions expected).

**Note:** Chunk B's binaries are line-heavy mostly because the five
`liq_*_executor` files (`liq_executor`, `liq_jupiter_executor`,
`liq_kamino_executor`, `liq_save_executor`, `liq_stream_executor`, ~3,800
lines combined) look like copy-pasted variants of the same executor loop
per-protocol/feed. Worker B should port the shared control flow once (e.g.
`src/lib/liqExecutor.ts`) and keep each binary as a thin per-variant
entrypoint, rather than porting all 5 line-for-line — this should cut actual
TS output well below the raw Rust line count.

## Shared conventions (both workers)

- One TS module per Rust file, same base name (snake_case → camelCase),
  under `ts-port/src/lib/` for `src/*.rs` and `ts-port/src/bin/` for
  `src/bin/*.rs`.
- Use `@solana/web3.js` for Pubkey/Transaction/Message/ALT types in place of
  the `solana-*` Rust crates; `@grpc/grpc-js` for the yellowstone gRPC feed;
  `bs58`, `dotenv` already scaffolded in `package.json`.
- `anyhow::Result` → throw/catch with plain `Error`; no custom Result type.
- Keep `.env` var names identical (see `.env.example` in the source repo) so
  `arb.config.example.json` / runbooks stay accurate.
- Run `npm install && npm run typecheck` from `ts-port/` after adding files.

## Status

- [x] Scaffolded `ts-port/` (`package.json`, `tsconfig.json`, `.gitignore`,
      `src/lib/`, `src/bin/`).
- [x] Chunk A ported (22 lib modules incl. `index.ts` barrel, 32 binaries +
      `shadow.ts`). `npm run typecheck` passes clean for the whole project.
- [x] Chunk B ported (13 lib modules incl. the consolidated `liqExecutor.ts`
      + 30 binaries). `npm run typecheck` / `npm run build` pass clean for
      the whole combined project.

### Chunk B notes

- The 5 near-duplicate `liq_executor.rs` / `liq_jupiter_executor.rs` /
  `liq_kamino_executor.rs` / `liq_save_executor.rs` / `liq_stream_executor.rs`
  binaries were consolidated: shared RPC/sim/logging/config/sign-submit
  plumbing now lives once in `src/lib/liqExecutor.ts`, and each Rust file
  became a thin(ner) per-protocol entrypoint in `src/bin/` that imports it.
  Protocol-specific decision logic (health/ratio math, sim-ladder sizing,
  fire-tx building, safety gates) was kept 1:1 per binary, not simplified.
- `src/bin/liqStreamExecutor.ts`'s gRPC push path is a documented
  `not implemented` stub, matching the same Yellowstone-`.proto`-unavailable
  precedent already established in Chunk A's `grpc.ts` / `mfiStreamDetect.ts`
  — the rest of its loop (reconnect, in-RAM loan book, pre-arm, Lazer-tick
  gating) is fully ported and runs at RPC-poll latency until a real gRPC
  client is wired in.

### Chunk A notes

- Added `ws`, `borsh`, `@grpc/proto-loader`, `@types/ws` to `package.json`
  (already present transitively in `node_modules`/`package-lock.json`; now
  explicit dependencies since Chunk A code imports them directly).
- **Known stubs** (no TS-portable equivalent available offline — no network to
  fetch the Yellowstone Geyser `.proto` or vendor the `shredstream` crate's
  wire format): `grpc.ts`'s low-level subscribe function, `shredstream.ts`'s
  `decodeShredDatagram`, and the equivalent inline stubs in `grpcPing.ts` /
  `grpcLatencyProbe.ts`. Everything else in those files (env parsing, tick
  decoding, heartbeat/report logic) is fully implemented; only the "read raw
  bytes off the wire" step throws `not implemented: ...` until a real
  `.proto`/SDK is vendored in.
- `clmm.ts` exposes `ClmmState` as a plain interface + free functions
  (`clmmFromOrca`, `clmmFromRay`, `applySwap`, `afterSwap`, `afterBaseSwap`,
  `uiPrice`) rather than a class, since nothing in Chunk A's own `arb.rs`
  imports `clmm.rs`. Chunk B's `liqFire.ts` (which depends on `clmm.rs` via
  `liq_fire.rs`) now correctly calls the free-function API
  (`clmmFromOrca`/`clmmApplySwap`) — reconciled on Chunk B's side.
- `npm run typecheck` passes with zero errors across the full combined
  project (Chunk A + Chunk B).
