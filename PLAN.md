# Porting Plan — solana-arb-backtest (Rust → Go)

## Module path / import prefix (READ THIS FIRST)

- Go module path (from `port/go.mod`): **`solana-arb-backtest`**
- Every package MUST be imported using this prefix, e.g.:
  - `solana-arb-backtest/shared`
  - `solana-arb-backtest/detector`
  - `solana-arb-backtest/pools`
  - `solana-arb-backtest/signal`
  - `solana-arb-backtest/grpc`
  - `solana-arb-backtest/pump`
- **Do NOT** import as `solana-arb-backtest/port/...` — `port/` is only the
  on-disk directory containing `go.mod`; it is NOT part of the import path,
  the same way `go.mod`'s directory never appears in its own module's import
  paths.
- All source lives under `port/` in this repo (sibling to `go.mod`). Chunk
  target directories below are given relative to `port/`.

## Source repo

Cloned fresh from `https://github.com/nickpismenkov/solana-arb-backtest.git`
into `temp_repo/` (Cargo package `arb-engine`, lib name `arb_engine`).
Rust module list per `temp_repo/src/lib.rs`. Binaries per
`temp_repo/Cargo.toml` (`[[bin]] name = "shadow", path = "src/main.rs"`) and
`temp_repo/src/bin/*.rs`.

## Scope

Only the core engine + the two backtest/shadow entry points are ported.
Everything in `src/bin/` other than `pump_backtest.rs` is a probe/debug/
census/decode/liquidation-live tool and is **out of scope** (not ported, not
stubbed): `alt_deploy`, `arb_probe`, `backrun_probe`, `clmm_probe`,
`decode_probe`, `executor`, `flashloan_probe`, `flow_probe`,
`grpc_latency_probe`, `grpc_ping`, `history_probe`, `hotpath_bench`,
`jito_probe`, `jup_alt_print`, `jup_probe`, `jupiter_fire_probe`,
`jupiter_probe`, `jupiter_seed_probe`, `kamino_alt_print`,
`kamino_fire_probe`, `kamino_liq_decode`, `kamino_liq_probe`, `land_probe`,
`lazer_probe`, `liq_alt_print`, `liq_crank_probe`, `liq_executor`,
`liq_finder`, `liq_fire_probe`, `liq_jupiter_executor`, `liq_kamino`,
`liq_kamino_executor`, `liq_kamino_live`, `liq_kamino_monitor`,
`liq_marginfi_sim`, `liq_monitor`, `liq_probe`, `liq_race`, `liq_report`,
`liq_save_executor`, `liq_stream_executor`, `marginfi_probe`,
`mfi_health_debug`, `mfi_liq_census`, `mfi_multipos_probe`,
`mfi_oracle_census`, `mfi_pyth_decode`, `mfi_reject_audit`,
`mfi_stream_detect`, `mfi_watchset_value`, `profit_watch`, `pump_census`,
`pump_collect`, `pyth_accum_probe`, `pyth_crank_decode`, `pyth_crank_probe`,
`pyth_probe`, `pyth_recv_decode`, `report`, `save_alt_print`,
`save_fire_probe`, `save_liq_census`, `save_liq_decode`,
`save_overflag_probe`, `save_probe`, `shred_probe`, `swap_probe`,
`tickarray_probe`, `verify_probe`, `watcher`.

Likewise, `src/*.rs` modules never reached by the two in-scope binaries
(`arb.rs`, `clmm.rs`, `decode.rs`, `execute.rs`, `flashloan.rs`, `jito.rs`,
`jup.rs`, `jupiter.rs`, `jupiter_fire.rs`, `jupiter_math.rs`, `kamino.rs`,
`kamino_engine.rs`, `kamino_fire.rs`, `kamino_ix.rs`, `lazer.rs`,
`liq_engine.rs`, `liq_fire.rs`, `liquidation.rs`, `marginfi.rs`, `observe.rs`,
`pyth.rs`, `pyth_accumulator.rs`, `pyth_crank.rs`, `save.rs`,
`save_engine.rs`, `save_fire.rs`, `shredstream.rs`, `swap.rs`) are out of
scope.

`signal.rs` has zero consumers anywhere in the current Rust source (dead
code / not yet wired up), but is explicitly requested as a core-engine file
to port, so it is included in Chunk 1 as its own package.

## Dependency graph (from `use crate::…` / `use arb_engine::…` greps)

- `detector.rs` — no internal deps (only `std`).
- `pools.rs` — no internal deps (only `std::sync::OnceLock`).
- `signal.rs` — no internal deps (only `std::sync::atomic`).
- `pump.rs` — no internal deps; external `solana_pubkey::Pubkey` → ported via
  `solana-arb-backtest/shared.Pubkey`.
- `grpc.rs` — imports `crate::detector::Tick` and
  `crate::pools::{orca_price, pair, ray_clmm_price}`.
- `main.rs` (the `shadow` binary) — imports `arb_engine::detector`
  (`median_f64`, `median_u128`, `ArbEvent`, `Detector`, `Tick`, `TickResult`),
  `arb_engine::grpc`, `arb_engine::pools::pair`.
- `src/bin/pump_backtest.rs` — imports only
  `arb_engine::pump::{curve_buy_tokens_out, curve_sell_sol_out}`.

Chunk 2 packages import Chunk 1 packages (one-directional: grpc/main →
detector/pools). Chunk 1 has no dependency on Chunk 2, so the two chunks can
be ported independently and in parallel without circular imports.

## `shared/` package (already created in this setup step)

`port/shared/pubkey.go` — package `solana-arb-backtest/shared`. Defines
`Pubkey` (32-byte array type mirroring `solana_pubkey::Pubkey`), with
`String()` (base58), `Bytes()`, and `PubkeyFromBytes`. This is the only
cross-cutting type needed: `detector`/`pools`/`signal` use nothing but Go
primitives, and `Tick`/`ArbEvent`/`TickResult`/`Detector` (owned by the
`detector` package) are consumed directly by Chunk 2 via a normal package
import — no sharing required for those. `Pubkey` is genuinely cross-cutting
(used throughout `pump.rs` for bonding-curve PDA derivation and event
decoding, and would be needed by any future Solana-touching chunk), so it
lives in `shared` rather than being owned by the `pump` package.

Dependency added to `go.mod`: `github.com/mr-tron/base58 v1.3.0` (base58
encode, matches Rust's `bs58` usage pattern for `Pubkey::to_string()`).

## Chunks

### Chunk 1 — Core engine

| Source file | Lines | Target |
|---|---|---|
| `temp_repo/src/detector.rs` | 136 | `port/detector/detector.go` (package `detector`) |
| `temp_repo/src/pools.rs` | 112 | `port/pools/pools.go` (package `pools`) |
| `temp_repo/src/signal.rs` | 106 | `port/signal/signal.go` (package `signal`) |

Import prefix for this chunk's packages: `solana-arb-backtest/detector`,
`solana-arb-backtest/pools`, `solana-arb-backtest/signal`. No dependency on
Chunk 2. May import `solana-arb-backtest/shared` if needed (currently none
of these three files need it).

### Chunk 2 — Main backtest / shadow binaries

| Source file | Lines | Target |
|---|---|---|
| `temp_repo/src/grpc.rs` | 75 | `port/grpc/grpc.go` (package `grpc`) — runner dependency of `main.rs` |
| `temp_repo/src/pump.rs` | 594 | `port/pump/pump.go` (package `pump`) — dependency of `pump_backtest.rs` |
| `temp_repo/src/main.rs` | 104 | `port/cmd/shadow/main.go` (package `main`, binary `shadow`) |
| `temp_repo/src/bin/pump_backtest.rs` | 997 | `port/cmd/pumpbacktest/main.go` (package `main`, binary `pumpbacktest`) |

Import prefix for this chunk's packages: `solana-arb-backtest/grpc`,
`solana-arb-backtest/pump`, plus the two `main` packages at
`solana-arb-backtest/cmd/shadow` and `solana-arb-backtest/cmd/pumpbacktest`.

This chunk imports Chunk 1's `solana-arb-backtest/detector` and
`solana-arb-backtest/pools` packages (from `grpc.go` and `cmd/shadow/main.go`),
and imports `solana-arb-backtest/shared` for `Pubkey` (from `pump.go`).

## Notes for both chunk workers

- Do not create placeholder/stub files for out-of-scope modules — they are
  simply not ported.
- Each chunk worker creates its own files under its target directory; this
  setup step only created `go.mod` and `shared/`.
- `main.rs`'s inline `rustls`/TLS crypto-provider install and the
  `yellowstone-grpc-client`/`yellowstone-grpc-proto` streaming calls in
  `grpc.rs` will need Go equivalents (e.g. a gRPC client against the same
  Yellowstone Geyser endpoint) — left to the Chunk 2 worker to resolve, this
  plan only assigns file scope, not implementation approach.
