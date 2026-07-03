#!/bin/bash
# Arb executor v2 runner — blind-guarded fire. DRY_RUN=1 by default (logs, no submit).
# Flip live with DRY_RUN=0 (needs KEYPAIR_PATH funded).
#
# Control it live by editing arb.config.json (hot-reloaded ~3s): paused,
# borrow_usdc, tip_lamports, priority_micro_lamports. No price threshold.
#
# On the box:
#   cp arb.config.example.json arb.config.json
#   ./runs/executor.sh                 # dry run (safe, logs only)
#   DRY_RUN=0 ./runs/executor.sh       # LIVE (tips only on wins; guard reverts losers)
set -uo pipefail
cd "$(dirname "$0")/.."

: "${RPC_ENDPOINT:?set RPC_ENDPOINT}"
: "${ALT_ADDRESS:=EZbTiW3Rr6ShpdBTUcSrFDAhf9wiK4DHV8PG8bz2Up1D}"
: "${KEYPAIR_PATH:=$HOME/arb-keypair.json}"
: "${SHREDSTREAM_PORT:=20000}"
: "${RUN_DIR:=runs}"
: "${CONFIG_PATH:=arb.config.json}"
: "${DRY_RUN:=1}"
: "${WALLET_MIN_SOL:=0.02}"
: "${MAX_DAILY_TIP_SOL:=0.05}"
export RPC_ENDPOINT ALT_ADDRESS KEYPAIR_PATH SHREDSTREAM_PORT RUN_DIR CONFIG_PATH \
       DRY_RUN WALLET_MIN_SOL MAX_DAILY_TIP_SOL

[ -f "$CONFIG_PATH" ] || cp arb.config.example.json "$CONFIG_PATH"
echo "=== building executor + watcher ==="
cargo build --release --bin executor --bin watcher || exit 1
echo "=== starting executor (DRY_RUN=$DRY_RUN) — edit $CONFIG_PATH to control; Ctrl-C to stop ==="
exec ./target/release/executor
