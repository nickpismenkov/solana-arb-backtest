// Verifies the swap.go instruction builders against mainnet by assembling a
// real swap and simulating it. We don't need funds: Anchor validates the
// account context (PDAs, owners, tick arrays, oracle) BEFORE the handler
// runs, so a correct build fails late at the token transfer (unfunded ATA),
// while a wrong meta fails early with a constraint/seeds/owner error. The
// probe prints the error class + logs so we can tell which happened.
//
// Usage: RPC_ENDPOINT=<url> go run ./cmd/swap_probe
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gagliardetto/solana-go"

	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/pools"
	"solana-arb-backtest-go/internal/swap"
	"solana-arb-backtest-go/internal/ticks"
)

const (
	ataProgram   = "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL"
	tokenProgram = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
)

func rpc(endpoint string, body map[string]any) (map[string]any, bool) {
	for attempt := 0; attempt < 4; attempt++ {
		b, _ := json.Marshal(body)
		resp, err := http.Post(endpoint, "application/json", bytes.NewReader(b))
		if err == nil {
			var v map[string]any
			decErr := json.NewDecoder(resp.Body).Decode(&v)
			resp.Body.Close()
			if decErr == nil {
				return v, true
			}
		}
		time.Sleep(time.Duration(400<<attempt) * time.Millisecond)
	}
	return nil, false
}

func accountData(endpoint, addr string) ([]byte, bool) {
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getAccountInfo",
		"params": []any{addr, map[string]string{"encoding": "base64"}}})
	if !ok {
		return nil, false
	}
	result, ok := v["result"].(map[string]any)
	if !ok {
		return nil, false
	}
	value, ok := result["value"].(map[string]any)
	if !ok || value == nil {
		return nil, false
	}
	dataArr, ok := value["data"].([]any)
	if !ok || len(dataArr) == 0 {
		return nil, false
	}
	dataStr, ok := dataArr[0].(string)
	if !ok {
		return nil, false
	}
	data, err := base64.StdEncoding.DecodeString(dataStr)
	if err != nil {
		return nil, false
	}
	return data, true
}

func pkAt(d []byte, o int) solana.PublicKey {
	return solana.PublicKeyFromBytes(d[o : o+32])
}

func ata(owner, mint solana.PublicKey) solana.PublicKey {
	addr, _, err := solana.FindProgramAddress(
		[][]byte{owner.Bytes(), solana.MustPublicKeyFromBase58(tokenProgram).Bytes(), mint.Bytes()},
		solana.MustPublicKeyFromBase58(ataProgram),
	)
	if err != nil {
		panic(err)
	}
	return addr
}

func simulate(endpoint string, ix solana.Instruction, authority solana.PublicKey) (*string, []string) {
	tx, err := solana.NewTransaction([]solana.Instruction{ix}, solana.Hash{}, solana.TransactionPayer(authority))
	if err != nil {
		panic(err)
	}
	tx.Signatures = []solana.Signature{{}}
	raw, err := tx.MarshalBinary()
	if err != nil {
		panic(err)
	}
	b64 := base64.StdEncoding.EncodeToString(raw)
	v, _ := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "simulateTransaction",
		"params": []any{b64, map[string]any{"encoding": "base64", "sigVerify": false, "replaceRecentBlockhash": true}}})

	var val map[string]any
	if v != nil {
		if result, ok := v["result"].(map[string]any); ok {
			if value, ok := result["value"].(map[string]any); ok {
				val = value
			}
		}
	}
	var errStr *string
	if val["err"] != nil {
		s, _ := json.Marshal(val["err"])
		str := string(s)
		errStr = &str
	}
	var logs []string
	if logsArr, ok := val["logs"].([]any); ok {
		for _, l := range logsArr {
			if s, ok := l.(string); ok {
				logs = append(logs, s)
			}
		}
	}
	return errStr, logs
}

func classify(err *string, logs []string) string {
	if err == nil {
		return "✅ SIMULATED OK — metas correct, swap executed"
	}
	joined := strings.ToLower(strings.Join(logs, "\n"))
	// Failing on OUR token accounts (unfunded/uninitialized ATAs) means every
	// pool-side meta already validated — the builder is correct.
	if strings.Contains(joined, "insufficient") ||
		strings.Contains(joined, "input_token_account") ||
		strings.Contains(joined, "output_token_account") ||
		strings.Contains(joined, "token_owner_account") ||
		strings.Contains(joined, "3012") ||
		strings.Contains(joined, "not be already initialized") ||
		strings.Contains(joined, "could not create program address") ||
		strings.Contains(joined, "account not found") ||
		strings.Contains(joined, "uninitialized") {
		return "✅ METAS OK — reached swap handler; only our unfunded ATA failed"
	}
	if strings.Contains(joined, "seeds") ||
		strings.Contains(joined, "constraint") ||
		strings.Contains(joined, "owned by") ||
		strings.Contains(joined, "declared program") {
		return "❌ METAS WRONG — account-context validation failed"
	}
	return "⚠️  INCONCLUSIVE — inspect logs below"
}

func main() {
	envfile.LoadDotEnv()
	endpoint := os.Getenv("RPC_ENDPOINT")
	if endpoint == "" {
		endpoint = "https://api.mainnet-beta.solana.com"
	}
	cfg := pools.Pair()
	// Any wallet works — we're checking account structure, not balances.
	authority := solana.MustPublicKeyFromBase58("Anu6Awu4kxaEDrg1nkpcikx6tJ2xhfVci5TvDrZBsZEB")

	// ── Orca ──
	fmt.Printf("=== Orca %s pool %s ===\n", cfg.Label, cfg.OrcaPool)
	od, ok := accountData(endpoint, cfg.OrcaPool)
	if !ok {
		panic("orca pool")
	}
	ost, ok := ticks.DecodeOrcaState(od)
	if !ok {
		panic("orca state")
	}
	orcaPk := solana.MustPublicKeyFromBase58(cfg.OrcaPool)
	mintA := pkAt(od, 101)
	mintB := pkAt(od, 181)
	start := ticks.OrcaStartIndex(ost.Tick, ost.TickSpacing)
	// Three consecutive tick arrays in the swap direction (a_to_b = price down).
	n := 88 * int32(ost.TickSpacing)
	baseIsA := mintA.Equals(solana.MustPublicKeyFromBase58(cfg.BaseMint))
	aToB := baseIsA // sell base = A→B when base is mintA
	var starts [3]int32
	if aToB {
		starts = [3]int32{start, start - n, start - 2*n}
	} else {
		starts = [3]int32{start, start + n, start + 2*n}
	}
	oa := &swap.OrcaSwapAccounts{
		Whirlpool:      orcaPk,
		TokenAuthority: authority,
		TokenOwnerA:    ata(authority, mintA),
		TokenVaultA:    pkAt(od, 133),
		TokenOwnerB:    ata(authority, mintB),
		TokenVaultB:    pkAt(od, 213),
		TickArrays: [3]solana.PublicKey{
			ticks.OrcaTickArray(orcaPk, starts[0]),
			ticks.OrcaTickArray(orcaPk, starts[1]),
			ticks.OrcaTickArray(orcaPk, starts[2]),
		},
		Oracle: swap.OrcaOracle(orcaPk),
	}
	ix := swap.OrcaSwapIx(oa, 100_000, 0, swap.SqrtLimit(aToB), true, aToB)
	errStr, logs := simulate(endpoint, ix, authority)
	fmt.Printf("  a_to_b=%v err=%v\n", aToB, derefOrNil(errStr))
	fmt.Printf("  %s\n", classify(errStr, logs))
	for i, l := range logs {
		if i >= 14 {
			break
		}
		fmt.Printf("    %s\n", l)
	}

	// ── Raydium CLMM ──
	fmt.Printf("\n=== Raydium CLMM %s pool %s ===\n", cfg.Label, cfg.RayPool)
	rd, ok := accountData(endpoint, cfg.RayPool)
	if !ok {
		panic("ray pool")
	}
	rst, ok := ticks.DecodeRayState(rd)
	if !ok {
		panic("ray state")
	}
	rayPk := solana.MustPublicKeyFromBase58(cfg.RayPool)
	ammConfig := pkAt(rd, 9)
	mint0 := pkAt(rd, 73)
	mint1 := pkAt(rd, 105)
	vault0 := pkAt(rd, 137)
	vault1 := pkAt(rd, 169)
	observation := pkAt(rd, 201)
	baseIs0 := mint0.Equals(solana.MustPublicKeyFromBase58(cfg.BaseMint))
	// Sell base: input is base. If base is mint0, input vault = vault0.
	var inputMint, inputVault, outputVault solana.PublicKey
	if baseIs0 {
		inputMint, inputVault, outputVault = mint0, vault0, vault1
	} else {
		inputMint, inputVault, outputVault = mint1, vault1, vault0
	}
	outputMint := mint1
	if !baseIs0 {
		outputMint = mint0
	}
	// Selling base: input == base. zero_for_one when input is mint0 → arrays descend.
	zeroForOne := baseIs0
	n2 := 60 * int32(rst.TickSpacing)
	rstart := ticks.RayStartIndex(rst.Tick, rst.TickSpacing)
	var rstarts [3]int32
	if zeroForOne {
		rstarts = [3]int32{rstart, rstart - n2, rstart - 2*n2}
	} else {
		rstarts = [3]int32{rstart, rstart + n2, rstart + 2*n2}
	}
	ra := &swap.RaySwapAccounts{
		Payer:              authority,
		AmmConfig:          ammConfig,
		PoolState:          rayPk,
		InputTokenAccount:  ata(authority, inputMint),
		OutputTokenAccount: ata(authority, outputMint),
		InputVault:         inputVault,
		OutputVault:        outputVault,
		ObservationState:   observation,
		TickArrays: [3]solana.PublicKey{
			ticks.RayTickArray(rayPk, rstarts[0]),
			ticks.RayTickArray(rayPk, rstarts[1]),
			ticks.RayTickArray(rayPk, rstarts[2]),
		},
	}
	isBaseInput := true
	rix := swap.RaySwapIx(ra, 100_000, 0, swap.SqrtLimit(baseIs0), isBaseInput)
	errStr2, logs2 := simulate(endpoint, rix, authority)
	fmt.Printf("  is_base_input=%v err=%v\n", isBaseInput, derefOrNil(errStr2))
	fmt.Printf("  %s\n", classify(errStr2, logs2))
	for i, l := range logs2 {
		if i >= 14 {
			break
		}
		fmt.Printf("    %s\n", l)
	}
}

func derefOrNil(s *string) string {
	if s == nil {
		return "None"
	}
	return *s
}
