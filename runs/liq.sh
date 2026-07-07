#!/bin/bash
# marginfi liquidation executor runner. DRY_RUN=1 by default (detects, sizes,
# quotes, full-simulates, logs "would fire" — never submits).
# Flip live with DRY_RUN=0 (needs KEYPAIR_PATH funded ≥ WALLET_MIN_SOL).
#
#   ./runs/liq.sh                 # dry run (safe, ledgers only)
#   DRY_RUN=0 ./runs/liq.sh       # LIVE (tip pays only on landing; repay_all
#                                 #  guard reverts unprofitable fires)
set -uo pipefail
cd "$(dirname "$0")/.."

: "${HELIUS_RPC:?set HELIUS_RPC (scan + simulate + submit readback)}"
: "${KEYPAIR_PATH:=$HOME/arb-keypair.json}"
: "${RUN_DIR:=runs}"
: "${DRY_RUN:=1}"
: "${MIN_COLLATERAL_USD:=100}"
: "${MIN_PROFIT_USD:=0.5}"
: "${TIP_FRACTION_BPS:=3000}"
: "${MAX_DAILY_TIP_SOL:=0.05}"
: "${WALLET_MIN_SOL:=0.02}"
: "${POLL_MS:=5000}"
: "${RESCAN_SECS:=300}"
export HELIUS_RPC KEYPAIR_PATH RUN_DIR DRY_RUN MIN_COLLATERAL_USD MIN_PROFIT_USD \
       TIP_FRACTION_BPS MAX_DAILY_TIP_SOL WALLET_MIN_SOL POLL_MS RESCAN_SECS

echo "=== building liq_executor ==="
cargo build --release --bin liq_executor || exit 1
echo "=== starting liquidation executor (DRY_RUN=$DRY_RUN) — Ctrl-C to stop ==="
exec ./target/release/liq_executor
