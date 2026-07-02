#!/usr/bin/env bash
# Phase 0 — network reality check. Measures TCP-connect RTT (network round-trip)
# to the Jito block-engine regions. Run this from a candidate VPS: if the closest
# region is single-digit ms, you're in the arena; ~30ms+ means you're a spectator.
#
# Usage: ./latency.sh [extra_host ...]   # pass your Geyser gRPC host to test it too
set -u
HOSTS=(
  mainnet.block-engine.jito.wtf
  amsterdam.mainnet.block-engine.jito.wtf
  frankfurt.mainnet.block-engine.jito.wtf
  london.mainnet.block-engine.jito.wtf
  ny.mainnet.block-engine.jito.wtf
  slc.mainnet.block-engine.jito.wtf
  tokyo.mainnet.block-engine.jito.wtf
  singapore.mainnet.block-engine.jito.wtf
  dublin.mainnet.block-engine.jito.wtf
  "$@"
)
echo "TCP-connect RTT (min of 5 samples, ms):"
for H in "${HOSTS[@]}"; do
  [ -z "$H" ] && continue
  vals=""
  for i in 1 2 3 4 5; do
    t=$(curl -sS -o /dev/null --connect-timeout 4 --max-time 6 -w "%{time_connect}" "https://$H/" 2>/dev/null)
    [ -n "$t" ] && vals="$vals $t"
  done
  if [ -z "$vals" ]; then printf "  %-48s unreachable\n" "$H"
  else printf "  %-48s " "$H"; python3 -c "import sys; v=[float(x)*1000 for x in '$vals'.split()]; print(f'{min(v):6.1f} ms')"; fi
done
