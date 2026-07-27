# Launching the Go Port of arb-engine

This directory contains the complete Go port of the core arb-engine, including the `shadow` real-time gRPC spread tracker, the fee-adjusted `Detector`, and the Q64.64 pool price decode.

## Prerequisites

- Go 1.21 or higher installed on your system.

## Setup

1. Copy `.env.example` to `.env`:
   ```bash
   cp .env.example .env
   ```

2. Configure your Tatum gRPC endpoint, Jito credentials, and desired pair parameters in `.env`.

## Running the Shadow Harness

To run the real-time shadow harness in Go:

```bash
go run cmd/shadow/main.go
```

## Packages

- **`pkg/pools`**: Handles pool pair configuration and decoding of on-chain price states (Orca Whirlpool and Raydium CLMM) using Q64.64 fixed-point math.
- **`pkg/detector`**: Implements the core tick-by-tick fee-adjusted arbitrage detector, tracking reaction budgets (slots + milliseconds).
- **`pkg/grpc`**: Provides the real-time gRPC subscriber loop for account updates and high-fidelity simulated ticks.
