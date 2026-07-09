#!/bin/bash
# Launch ALL liquidation executors in parallel — marginfi, Kamino, Save — as
# separate background processes sharing one wallet. Each gets its own RUN_DIR
# (so ledgers don't collide) and an EQUAL SLICE of the shared daily tip budget
# (per-process caps otherwise multiply: N executors = N× the cap).
#
#   ./runs/liq_all.sh                 # all three, DRY_RUN=1 (safe)
#   DRY_RUN=0 ./runs/liq_all.sh       # all three LIVE
#   TOTAL_DAILY_TIP_SOL=0.09 DRY_RUN=0 ./runs/liq_all.sh
#
# Logs: runs/<proto>/exec.log   ·   PIDs: runs/liq_all.pids
# Stop everything:  ./runs/liq_all.sh stop   (or: pkill -f liq_.*executor)
set -uo pipefail
cd "$(dirname "$0")/.."

if [ "${1:-}" = "stop" ]; then
  echo "stopping all liquidation executors …"
  pkill -f 'target/release/liq_executor' 2>/dev/null || true
  pkill -f 'target/release/liq_kamino_executor' 2>/dev/null || true
  pkill -f 'target/release/liq_save_executor' 2>/dev/null || true
  pkill -f 'target/release/liq_jupiter_executor' 2>/dev/null || true
  rm -f runs/liq_all.pids
  echo "done."
  exit 0
fi

# Load .env (command-line overrides win).
if [ -f .env ]; then
  _preset=$(export -p); set -a; . ./.env; set +a; eval "$_preset"
fi
: "${HELIUS_RPC:?set HELIUS_RPC}"
: "${KEYPAIR_PATH:=$HOME/arb-keypair.json}"
: "${DRY_RUN:=1}"
: "${WALLET_MIN_SOL:=0.02}"
# Shared daily tip budget, split equally across the 3 executors.
: "${TOTAL_DAILY_TIP_SOL:=0.06}"
PER=$(awk "BEGIN{printf \"%.4f\", $TOTAL_DAILY_TIP_SOL/3}")

echo "=== building all executors ==="
cargo build --release --bin liq_executor --bin liq_kamino_executor --bin liq_save_executor --bin liq_jupiter_executor || exit 1

mkdir -p runs/liq runs/kamino runs/save runs/jupiter
: > runs/liq_all.pids
echo "=== launching (DRY_RUN=$DRY_RUN, per-executor daily tip cap ${PER} SOL) ==="

# SPEED PROTECTION for marginfi's latency-critical crank/fire path:
#  - marginfi runs at NORMAL priority + on the primary (low-latency) RPC.
#  - Kamino/Save are NOT latency-critical (periodic-scan triggers), so they run
#    at LOWER cpu priority (nice) and, if you set SCAN_RPC, on a SEPARATE RPC
#    endpoint — so their heavy getProgramAccounts scans can't contend for CPU or
#    RPC with marginfi's hot path. marginfi's actual fire uses only pre-cached
#    state + one Sender call, so it's isolated regardless; this protects the
#    arm-phase + blockhash RPC too.
SCAN_RPC=${SCAN_RPC:-$HELIUS_RPC}

# marginfi (uses Lazer + self-crank if PYTH_LAZER_TOKEN is set) — normal priority, primary RPC.
RUN_DIR=runs/liq   HELIUS_RPC=$HELIUS_RPC DRY_RUN=$DRY_RUN KEYPAIR_PATH=$KEYPAIR_PATH MAX_DAILY_TIP_SOL=$PER WALLET_MIN_SOL=$WALLET_MIN_SOL \
  nohup ./target/release/liq_executor        > runs/liq/exec.log    2>&1 & echo "marginfi $!" | tee -a runs/liq_all.pids
# Kamino — lower priority, scan RPC.
RUN_DIR=runs/kamino HELIUS_RPC=$SCAN_RPC DRY_RUN=$DRY_RUN KEYPAIR_PATH=$KEYPAIR_PATH MAX_DAILY_TIP_SOL=$PER WALLET_MIN_SOL=$WALLET_MIN_SOL \
  nohup nice -n 10 ./target/release/liq_kamino_executor > runs/kamino/exec.log 2>&1 & echo "kamino $!"   | tee -a runs/liq_all.pids
# Save (Solend) — heaviest scan; lower priority, scan RPC, slower rescan.
RUN_DIR=runs/save  HELIUS_RPC=$SCAN_RPC DRY_RUN=$DRY_RUN KEYPAIR_PATH=$KEYPAIR_PATH MAX_DAILY_TIP_SOL=$PER WALLET_MIN_SOL=$WALLET_MIN_SOL \
  RESCAN_SECS=${SAVE_RESCAN_SECS:-30} \
  nohup nice -n 10 ./target/release/liq_save_executor   > runs/save/exec.log   2>&1 & echo "save $!"      | tee -a runs/liq_all.pids
# Jupiter (Fluid) — OBSERVE-ONLY: its loop never submits, and live single-packet
# fire is still gated on a deployed JUP_ALT, so it detects/arms for observation
# regardless of DRY_RUN. Lower priority, scan RPC.
RUN_DIR=runs/jupiter HELIUS_RPC=$SCAN_RPC DRY_RUN=$DRY_RUN \
  nohup nice -n 10 ./target/release/liq_jupiter_executor > runs/jupiter/exec.log 2>&1 & echo "jupiter $!" | tee -a runs/liq_all.pids

echo
echo "launched. tail a log:   tail -F runs/liq/exec.log runs/kamino/exec.log runs/save/exec.log runs/jupiter/exec.log"
echo "digest per protocol:    RUN_DIR=runs/save cargo run --release --bin liq_report"
echo "stop all:               ./runs/liq_all.sh stop"
