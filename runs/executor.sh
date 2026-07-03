#!/bin/bash
# Arb executor runner. DRY_RUN=1 by default — evaluates + logs, never submits.
# Flip to live by exporting DRY_RUN=0 (and having KEYPAIR_PATH funded).
#
# On the box:
#   cp .env.example .env   # fill GRPC/RPC/etc
#   ./runs/executor.sh                 # dry run (safe)
#   DRY_RUN=0 ./runs/executor.sh       # LIVE (spends tips on wins only)
set -uo pipefail
cd "$(dirname "$0")/.."

: "${RPC_ENDPOINT:?set RPC_ENDPOINT}"
: "${ALT_ADDRESS:=EZbTiW3Rr6ShpdBTUcSrFDAhf9wiK4DHV8PG8bz2Up1D}"
: "${KEYPAIR_PATH:=$HOME/arb-keypair.json}"
: "${SHREDSTREAM_PORT:=20000}"
: "${BORROW_USDC:=500}"
: "${TIP_LAMPORTS:=10000}"
: "${PRIORITY_MICRO_LAMPORTS:=10000}"
: "${RUN_DIR:=runs}"
: "${DRY_RUN:=1}"
: "${WALLET_MIN_SOL:=0.02}"
: "${MAX_DAILY_TIP_SOL:=0.05}"
export RPC_ENDPOINT ALT_ADDRESS KEYPAIR_PATH SHREDSTREAM_PORT BORROW_USDC \
       TIP_LAMPORTS PRIORITY_MICRO_LAMPORTS RUN_DIR DRY_RUN WALLET_MIN_SOL MAX_DAILY_TIP_SOL

echo "=== building executor ==="
cargo build --release --bin executor || exit 1
echo "=== starting (DRY_RUN=$DRY_RUN) — Ctrl-C to stop ==="
exec ./target/release/executor
