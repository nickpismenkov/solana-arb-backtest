# Running the shadow test on the co-located box (Amsterdam)

Prereqs on the box: **Node ≥ 20**, **git**, a **public IP**, and **UDP 20000 open**.
Your box's public IP must be **registered in the shredstream.com dashboard** (Amsterdam), and the shredstream.com trial **active**.

```bash
# 1. get the code
git clone https://github.com/nickpismenkov/solana-arb-backtest.git
cd solana-arb-backtest
npm install                      # pulls shredstream (linux-x64 native), @solana/web3.js, tsx, etc.

# 2. open the UDP port (also allow it in the cloud provider's firewall/security group)
sudo ufw allow 20000/udp || true

# 3. config
cp .env.example .env
#   edit .env:
#     GRPC_ENDPOINT=https://solana-mainnet-grpc.gateway.tatum.io
#     GRPC_X_TOKEN=<your Tatum token>
#     SHREDSTREAM_PORT=20000

# 4. sanity: are shreds arriving?  (should see packets from shredstream once streaming)
sudo tcpdump -ni any udp port 20000 | head

# 5. sanity: latency to the co-located block engine
./scripts/latency.sh          # want amsterdam.mainnet.block-engine.jito.wtf in single-digit ms

# 6. RUN THE SHADOW TEST (10 min default; override with RUN_MS)
RUN_MS=600000 npm run shadow-live
```

## What the report means
- **ShredStream pool triggers seen** — confirms the fast feed is live and hitting our pools.
- **Real fee-adjusted arbs** — cross-venue gaps that exceed both fees (29 bps).
- **reaction budget (slots/ms)** — how long a real arb lasts = your window to act.
- **ShredStream head start (ms)** — how much sooner ShredStream delivered the swap vs the gRPC feed.
- **contendable %** — arbs where budget ≥ 1 slot *and* we had a ShredStream signal. The go/no-go.

## Notes
- The pool-hit filter uses `staticAccountKeys`; v0 txns that reference a pool only via an Address Lookup Table won't match yet (refinement if hit-rate looks low — watch the `[shredstream] pool-hits=` heartbeat).
- gRPC (Tatum) is Turbine-sourced (slower); ShredStream is ahead-of-Turbine. That gap is exactly the "head start" we measure. For production, replace Tatum with a shred-sourced price path.
