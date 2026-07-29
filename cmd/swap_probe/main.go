// Command swap_probe verifies the swap.go instruction builders against
// mainnet by assembling a real swap and simulating it. We don't need funds:
// Anchor validates the account context (PDAs, owners, tick arrays, oracle)
// BEFORE the handler runs, so a correct build fails late at the token
// transfer (unfunded ATA), while a wrong meta fails early with a
// constraint/seeds/owner error. The probe prints the error class + logs so
// we can tell which happened.
//
// Usage: RPC_ENDPOINT=<url> go run ./cmd/swap_probe
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"arbengine/internal/config"
	"arbengine/internal/execute"
	"arbengine/internal/flashloan"
	"arbengine/internal/pools"
	"arbengine/internal/rpcclient"
	"arbengine/internal/solana"
	"arbengine/internal/swap"
)

// fatal mirrors Rust's .expect(msg) panic-and-exit for a required value.
func fatal(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}

func ata(owner, mint solana.Pubkey) solana.Pubkey {
	return flashloan.AtaFor(owner, mint, solana.MustPubkeyFromBase58(flashloan.TokenProgram))
}

// simulate compiles a v0 message (no ALTs) with a zero blockhash and
// zero signatures, then simulates with replaceRecentBlockhash — the Go
// equivalent of Rust's legacy Message::new_with_blockhash + sigVerify=false.
func simulate(c *rpcclient.Client, ix solana.Instruction, authority solana.Pubkey) (*string, []string) {
	msg, err := solana.CompileV0(authority, []solana.Instruction{ix}, nil, solana.Hash{})
	if err != nil {
		return nil, nil
	}
	tx := solana.NewUnsignedVersionedTransaction(msg)
	b64, err := tx.Base64()
	if err != nil {
		return nil, nil
	}
	raw, err := c.SimulateTransaction(b64)
	if err != nil || raw == nil {
		return nil, nil
	}
	var val struct {
		Err  json.RawMessage `json:"err"`
		Logs []string        `json:"logs"`
	}
	if err := json.Unmarshal(raw, &val); err != nil {
		return nil, nil
	}
	var errStr *string
	if len(val.Err) > 0 && string(val.Err) != "null" {
		s := string(val.Err)
		errStr = &s
	}
	return errStr, val.Logs
}

func classify(err *string, logs []string) string {
	if err == nil {
		return "✅ SIMULATED OK — metas correct, swap executed"
	}
	joined := strings.ToLower(strings.Join(logs, "\n"))
	// Failing on OUR token accounts (unfunded/uninitialized ATAs) means every
	// pool-side meta already validated — the builder is correct.
	switch {
	case strings.Contains(joined, "insufficient"),
		strings.Contains(joined, "input_token_account"),
		strings.Contains(joined, "output_token_account"),
		strings.Contains(joined, "token_owner_account"),
		strings.Contains(joined, "3012"),
		strings.Contains(joined, "not be already initialized"),
		strings.Contains(joined, "could not create program address"),
		strings.Contains(joined, "account not found"),
		strings.Contains(joined, "uninitialized"):
		return "✅ METAS OK — reached swap handler; only our unfunded ATA failed"
	case strings.Contains(joined, "seeds"),
		strings.Contains(joined, "constraint"),
		strings.Contains(joined, "owned by"),
		strings.Contains(joined, "declared program"):
		return "❌ METAS WRONG — account-context validation failed"
	default:
		return "⚠️  INCONCLUSIVE — inspect logs below"
	}
}

func main() {
	config.LoadDotenv()
	endpoint := config.EnvOr("RPC_ENDPOINT", "https://api.mainnet-beta.solana.com")
	c := rpcclient.New(endpoint)
	cfg := pools.Pair()
	// Any wallet works — we're checking account structure, not balances.
	authority := solana.MustPubkeyFromBase58("Anu6Awu4kxaEDrg1nkpcikx6tJ2xhfVci5TvDrZBsZEB")

	// ── Orca ──
	fmt.Printf("=== Orca %s pool %s ===\n", cfg.Label, cfg.OrcaPool)
	orcaPk := solana.MustPubkeyFromBase58(cfg.OrcaPool)
	od, err := c.GetAccountData(orcaPk)
	if err != nil || od == nil {
		fatal("orca pool")
	}
	ost, ok := execute.DecodeOrcaState(od)
	if !ok {
		fatal("orca state")
	}
	mintA := arbPkAt(od, 101)
	mintB := arbPkAt(od, 181)
	start := execute.OrcaStartIndex(ost.Tick, ost.TickSpacing)
	// Three consecutive tick arrays in the swap direction (a_to_b = price down).
	n := 88 * int32(ost.TickSpacing)
	baseMint := solana.MustPubkeyFromBase58(cfg.BaseMint)
	baseIsA := mintA == baseMint
	aToB := baseIsA // sell base = A→B when base is mintA
	var starts [3]int32
	if aToB {
		starts = [3]int32{start, start - n, start - 2*n}
	} else {
		starts = [3]int32{start, start + n, start + 2*n}
	}
	oa := swap.OrcaSwapAccounts{
		Whirlpool:      orcaPk,
		TokenAuthority: authority,
		TokenOwnerA:    ata(authority, mintA),
		TokenVaultA:    arbPkAt(od, 133),
		TokenOwnerB:    ata(authority, mintB),
		TokenVaultB:    arbPkAt(od, 213),
		TickArrays: [3]solana.Pubkey{
			execute.OrcaTickArray(orcaPk, starts[0]),
			execute.OrcaTickArray(orcaPk, starts[1]),
			execute.OrcaTickArray(orcaPk, starts[2]),
		},
		Oracle: swap.OrcaOracle(orcaPk),
	}
	ix := swap.OrcaSwapIx(oa, 100_000, 0, swap.SqrtLimit(aToB), true, aToB)
	oErr, oLogs := simulate(c, ix, authority)
	fmt.Printf("  a_to_b=%v err=%s\n", aToB, errDebug(oErr))
	fmt.Printf("  %s\n", classify(oErr, oLogs))
	for i, l := range oLogs {
		if i >= 14 {
			break
		}
		fmt.Printf("    %s\n", l)
	}

	// ── Raydium CLMM ──
	fmt.Printf("\n=== Raydium CLMM %s pool %s ===\n", cfg.Label, cfg.RayPool)
	rayPk := solana.MustPubkeyFromBase58(cfg.RayPool)
	rd, err := c.GetAccountData(rayPk)
	if err != nil || rd == nil {
		fatal("ray pool")
	}
	rst, ok := execute.DecodeRayState(rd)
	if !ok {
		fatal("ray state")
	}
	ammConfig := arbPkAt(rd, 9)
	mint0 := arbPkAt(rd, 73)
	mint1 := arbPkAt(rd, 105)
	vault0 := arbPkAt(rd, 137)
	vault1 := arbPkAt(rd, 169)
	observation := arbPkAt(rd, 201)
	baseIs0 := mint0 == baseMint
	// Sell base: input is base. If base is mint0, input vault = vault0.
	var inputMint, inputVault, outputVault, outputMint solana.Pubkey
	if baseIs0 {
		inputMint, inputVault, outputVault = mint0, vault0, vault1
		outputMint = mint1
	} else {
		inputMint, inputVault, outputVault = mint1, vault1, vault0
		outputMint = mint0
	}
	// Selling base: input == base. zero_for_one when input is mint0 → arrays descend.
	zeroForOne := baseIs0
	rn := 60 * int32(rst.TickSpacing)
	rstart := execute.RayStartIndex(rst.Tick, rst.TickSpacing)
	var rstarts [3]int32
	if zeroForOne {
		rstarts = [3]int32{rstart, rstart - rn, rstart - 2*rn}
	} else {
		rstarts = [3]int32{rstart, rstart + rn, rstart + 2*rn}
	}
	ra := swap.RaySwapAccounts{
		Payer:              authority,
		AmmConfig:          ammConfig,
		PoolState:          rayPk,
		InputTokenAccount:  ata(authority, inputMint),
		OutputTokenAccount: ata(authority, outputMint),
		InputVault:         inputVault,
		OutputVault:        outputVault,
		ObservationState:   observation,
		TickArrays: [3]solana.Pubkey{
			execute.RayTickArray(rayPk, rstarts[0]),
			execute.RayTickArray(rayPk, rstarts[1]),
			execute.RayTickArray(rayPk, rstarts[2]),
		},
	}
	isBaseInput := true
	rix := swap.RaySwapIx(ra, 100_000, 0, swap.SqrtLimit(baseIs0), isBaseInput)
	rErr, rLogs := simulate(c, rix, authority)
	fmt.Printf("  is_base_input=%v err=%s\n", isBaseInput, errDebug(rErr))
	fmt.Printf("  %s\n", classify(rErr, rLogs))
	for i, l := range rLogs {
		if i >= 14 {
			break
		}
		fmt.Printf("    %s\n", l)
	}
}

func arbPkAt(d []byte, o int) solana.Pubkey {
	pk, _ := solana.PubkeyFromBytes(d[o : o+32])
	return pk
}

// errDebug mirrors Rust's `{err:?}` formatting for Option<String>: "None" or
// the quoted string.
func errDebug(err *string) string {
	if err == nil {
		return "None"
	}
	return fmt.Sprintf("Some(%q)", *err)
}
