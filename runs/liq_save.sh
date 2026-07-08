#!/bin/bash
# Save (Solend) liquidation executor runner. DRY_RUN=1 by default (detects,
# sizes, quotes, full-simulates, logs "would fire" — never submits).
# Flip live with DRY_RUN=0 (needs KEYPAIR_PATH funded ≥ WALLET_MIN_SOL).
#
#   ./runs/liq_save.sh                 # dry run (safe, ledgers only)
#   DRY_RUN=0 ./runs/liq_save.sh       # LIVE (profit-or-revert; tip pays only on land)
#
# Runs in parallel with liq.sh (marginfi) — they're separate processes sharing
# the wallet. SHARED-WALLET RISK BUDGET: split the daily tip cap across the N
# executors you run, e.g. with 2 live set MAX_DAILY_TIP_SOL=0.025 on each.
set -uo pipefail
cd "$(dirname "$0")/.."

# Load .env (command-line overrides still win — snapshot/restore).
if [ -f .env ]; then
  _preset=$(export -p)
  set -a; . ./.env; set +a
  eval "$_preset"
fi

: "${HELIUS_RPC:?set HELIUS_RPC (scan + simulate + submit readback)}"
: "${KEYPAIR_PATH:=$HOME/arb-keypair.json}"
: "${RUN_DIR:=runs/save}"
: "${DRY_RUN:=1}"
: "${MIN_DEBT_USD:=100}"
: "${MIN_PROFIT_USD:=0.5}"
: "${RESCAN_SECS:=20}"
: "${MAX_FIRE_PER_CYCLE:=4}"
: "${RATIO_CAP:=3.0}"
: "${SLIPPAGE_BPS:=100}"
: "${MAX_SWAP_ACCOUNTS:=18}"
: "${MAX_DAILY_TIP_SOL:=0.025}"   # half the default, assuming it runs beside marginfi
: "${WALLET_MIN_SOL:=0.02}"
export HELIUS_RPC KEYPAIR_PATH RUN_DIR DRY_RUN MIN_DEBT_USD MIN_PROFIT_USD RESCAN_SECS \
       MAX_FIRE_PER_CYCLE RATIO_CAP SLIPPAGE_BPS MAX_SWAP_ACCOUNTS MAX_DAILY_TIP_SOL WALLET_MIN_SOL

echo "=== building liq_save_executor ==="
cargo build --release --bin liq_save_executor || exit 1
echo "=== starting Save liquidation executor (DRY_RUN=$DRY_RUN) — Ctrl-C to stop ==="
exec ./target/release/liq_save_executor
