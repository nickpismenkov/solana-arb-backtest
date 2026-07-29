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

# Load .env so the guards below (and the binary) see it — but command-line
# overrides (e.g. `DRY_RUN=0 RUN_DIR=runs/liq ./runs/liq.sh`) must WIN over .env.
# Snapshot the pre-set environment, source .env (expands $HOME etc.), then
# re-apply the snapshot so anything already set on the command line takes back.
if [ -f .env ]; then
  _preset=$(export -p)
  set -a; . ./.env; set +a
  eval "$_preset"
fi

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
# Self-crank mode (marginfi stale-oracle edge) — needs PYTH_LAZER_TOKEN to
# actually trigger. CRANK=0 disables. PYTH_LAZER_TOKEN/ALERT_WEBHOOK/HERMES/
# JITO_BLOCK_ENGINE are read from the environment/.env directly.
: "${CRANK:=1}"
: "${MAX_BLOB_AGE_MS:=3000}"
# Observability + per-cycle sim caps (emode-aware health keeps the crossed set
# small; these bound it if a real crash flags many at once). HEARTBEAT_SECS=0
# silences the status line.
: "${HEARTBEAT_SECS:=30}"
: "${MAX_ARM_PER_CYCLE:=8}"
: "${MAX_FIRE_PER_CYCLE:=4}"
export HELIUS_RPC KEYPAIR_PATH RUN_DIR DRY_RUN MIN_COLLATERAL_USD MIN_PROFIT_USD \
       TIP_FRACTION_BPS MAX_DAILY_TIP_SOL WALLET_MIN_SOL POLL_MS RESCAN_SECS \
       CRANK MAX_BLOB_AGE_MS HEARTBEAT_SECS MAX_ARM_PER_CYCLE MAX_FIRE_PER_CYCLE

echo "=== building liq_executor ==="
cargo build --release --bin liq_executor || exit 1
echo "=== starting liquidation executor (DRY_RUN=$DRY_RUN) — Ctrl-C to stop ==="
exec ./target/release/liq_executor
