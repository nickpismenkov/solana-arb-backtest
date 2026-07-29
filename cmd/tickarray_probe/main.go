// Command tickarray_probe verifies tick-array derivation against the chain
// (no wallet, no money). Reads each pool's live state, derives the current
// tick-array PDA, and checks that account actually exists and is owned by
// the DEX program. If both resolve to real program-owned accounts, the
// start-index math + PDA seeds are correct — the foundation the swap
// instructions stand on.
//
// Usage: RPC_ENDPOINT=<url> go run ./cmd/tickarray_probe
package main

import (
	"fmt"

	"arbengine/internal/config"
	"arbengine/internal/decode"
	"arbengine/internal/execute"
	"arbengine/internal/pools"
	"arbengine/internal/rpcclient"
	"arbengine/internal/solana"
)

func check(rpc *rpcclient.Client, label string, program string, state execute.PoolState, tickArray solana.Pubkey, start int32) {
	fmt.Printf("\n%s: tick=%d spacing=%d liquidity=%v\n", label, state.Tick, state.TickSpacing, state.Liquidity)
	fmt.Printf("  current tick-array start index: %d\n", start)
	fmt.Printf("  derived tick-array PDA: %s\n", tickArray)

	info, err := rpc.GetAccountInfo(tickArray)
	if err != nil || info == nil {
		fmt.Println("  on-chain: account not found ✗ (derivation wrong or empty array)")
		return
	}
	ok := info.Owner == program
	status := "✗ wrong owner"
	if ok {
		status = "✓ program-owned (derivation VALID)"
	}
	fmt.Printf("  on-chain: owner=%s len=%d → %s\n", info.Owner, len(info.Data), status)
}

func main() {
	config.LoadDotenv()

	endpoint := config.EnvOr("RPC_ENDPOINT", "https://api.mainnet-beta.solana.com")
	rpc := rpcclient.New(endpoint)

	cfg := pools.Pair()
	orcaPool := solana.MustPubkeyFromBase58(cfg.OrcaPool)
	rayPool := solana.MustPubkeyFromBase58(cfg.RayPool)

	if info, err := rpc.GetAccountInfo(orcaPool); err == nil && info != nil {
		if st, ok := execute.DecodeOrcaState(info.Data); ok {
			start := execute.OrcaStartIndex(st.Tick, st.TickSpacing)
			check(rpc, "Orca", decode.OrcaProgram, st, execute.OrcaTickArray(orcaPool, start), start)
		}
	}
	if info, err := rpc.GetAccountInfo(rayPool); err == nil && info != nil {
		if st, ok := execute.DecodeRayState(info.Data); ok {
			start := execute.RayStartIndex(st.Tick, st.TickSpacing)
			check(rpc, "Raydium CLMM", decode.RayClmmProgram, st, execute.RayTickArray(rayPool, start), start)
		}
	}
}
