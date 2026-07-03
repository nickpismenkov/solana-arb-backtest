# On-box runbook (co-located box, Amsterdam)

Prereqs on the box: **Rust toolchain** (rustup, stable), **git**, a **public
IP**, and **UDP 20000 open**. The box's public IP must be registered with the
shred provider (shredstream.com dashboard, Amsterdam region — or self-hosted
Jito ShredStream proxy once whitelisted).

```bash
# 1. get the code + toolchain
git clone https://github.com/nickpismenkov/solana-arb-backtest.git
cd solana-arb-backtest
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y && . "$HOME/.cargo/env"

# 2. open the UDP port (also allow it in the provider's firewall/security group)
sudo ufw allow 20000/udp || true

# 3. config
cp .env.example .env      # fill in GRPC_X_TOKEN and RPC_ENDPOINT (never commit)

# 4. sanity: are shreds arriving?
sudo tcpdump -ni any udp port 20000 | head

# 5. sanity: latency to the co-located block engine (want single-digit ms)
./scripts/latency.sh

# 6. build once
cargo build --release

# 7. feed check: txn/s + pool-hit heartbeats from real shreds
cargo run --release --bin shred_probe

# 8. THE measurement — shred-triggered victim sim → residual backrunnable gap
RUN_MS=600000 cargo run --release --bin backrun_probe
```

## What backrun_probe reports

- **pool triggers seen** — shred-carried txs touching our pools (ALT-resolved).
- **sim success rate** — victims that apply pre-landing (reverts are skipped).
- **fee-clearing %** — post-victim cross-venue gaps > 8 bp (Orca 4 + Ray CLMM 4):
  the residual edge a guarded backrun could capture. **This is the go/no-go.**
- **net-edge distribution** — median/max residual gap in bps.

Result on liquid SOL/USDC (2026-07-02): 0/85 cleared — NO-GO (see README).

## Other probes

- `shadow` — gRPC (Turbine) price feed + detector; standing-edge check.
- `decode_probe` — what programs/instructions shreds actually carry.
- `jito_probe` — block-engine round-trip (`getTipAccounts`, `sendBundle`).
- `tickarray_probe` — pool-state + tick-array PDA derivation checks.

## Notes

- gRPC (Tatum) is Turbine-sourced and undersamples (~1 tick/2 s) — it cannot
  see sub-slot transients; ShredStream is the fast leg.
- `backrun_probe` needs `RPC_ENDPOINT` set; simulation pacing is RPC-bound,
  which is fine — it's a measurement, not the hot path.
