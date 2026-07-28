// Verify the Jupiter Lend (Fluid) liquidate reversal end-to-end. Three stages,
// all read-only (never submits):
//
//  1. GROUND-TRUTH PDA CHECK — pull recent real `liquidate` txs off the Vaults
//     program, split each `remaining` list by `remaining_accounts_indices`
//     ([sources, branches, ticks, tick_has_debt]), read every branch/tick/
//     tick_has_debt account, decode its id, and re-derive the PDA from our seeds.
//     A 100% match proves the seed + layout reversal against real liquidators.
//
//     2+3. LIVE SELECTION + SIM-VERIFY — for each vault with a recent liquidate (so
//     its Liquidity PDAs + oracle sources are liftable), derive the
//     `remaining_accounts` FRESH from current on-chain state via
//     `build_remaining_accounts`, build a `liquidate` ix (captured liquidator-side
//     accounts + our fresh remaining, col_per_unit_debt = 0), and
//     simulateTransaction (sigVerify=false, replaceRecentBlockhash=true). Success
//     bar: a CLEAN sim, or a revert at the protocol's OWN liquidation gate
//     (VaultInvalidLiquidation etc.) — either proves every upstream leg composes
//     (oracle CPI via sources, exchange prices, branch/tick/tick_has_debt wiring).
//
//  4. FULL FIRE TX — for a USDC-debt vault, build the marginfi-flash-loan-wrapped
//     liquidate+swap+repay tx and report composition + byte size (a single-packet
//     fire needs a deployment ALT, like Save's SAVE_ALT).
//
// Usage: HELIUS_RPC=<url> [SCAN_SIGS=1000] [SIM_VAULT=<id>] go run ./cmd/jupiter_fire_probe
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/base58"

	"solana-arb-backtest-go/internal/arb"
	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/flashloan"
	"solana-arb-backtest-go/internal/jupiterlend"
)

func rpc(endpoint string, body map[string]any) map[string]any {
	b, err := json.Marshal(body)
	if err != nil {
		return nil
	}
	for attempt := 0; attempt < 5; attempt++ {
		resp, err := http.Post(endpoint, "application/json", bytes.NewReader(b))
		if err == nil {
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var v map[string]any
			if json.Unmarshal(raw, &v) == nil {
				return v
			}
		}
		time.Sleep(time.Duration(400<<attempt) * time.Millisecond)
	}
	return nil
}

func b64field(d any) ([]byte, bool) {
	arr, ok := d.([]any)
	if !ok || len(arr) == 0 {
		return nil, false
	}
	s, ok := arr[0].(string)
	if !ok {
		return nil, false
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, false
	}
	return raw, true
}

func getAcct(endpoint string, pk solana.PublicKey) ([]byte, bool) {
	v := rpc(endpoint, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "getAccountInfo",
		"params": []any{pk.String(), map[string]any{"encoding": "base64"}},
	})
	if v == nil {
		return nil, false
	}
	result, _ := v["result"].(map[string]any)
	value, _ := result["value"].(map[string]any)
	if value == nil {
		return nil, false
	}
	return b64field(value["data"])
}

func mintOwner(endpoint string, mint solana.PublicKey) (solana.PublicKey, bool) {
	v := rpc(endpoint, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "getAccountInfo",
		"params": []any{mint.String(), map[string]any{"encoding": "base64"}},
	})
	if v == nil {
		return solana.PublicKey{}, false
	}
	result, _ := v["result"].(map[string]any)
	value, _ := result["value"].(map[string]any)
	if value == nil {
		return solana.PublicKey{}, false
	}
	ownerStr, _ := value["owner"].(string)
	pk, err := solana.PublicKeyFromBase58(ownerStr)
	if err != nil {
		return solana.PublicKey{}, false
	}
	return pk, true
}

// realLiq is a decoded real liquidate ix: args + full ordered account list.
type realLiq struct {
	Sig            string
	DebtAmt        uint64
	ColPerUnitDebt *[2]uint64
	Indices        []uint8
	Accounts       []solana.PublicKey
}

func decodeLiqArgs(data []byte) (debt uint64, col *[2]uint64, idx []uint8, ok bool) {
	if len(data) < 16 {
		return 0, nil, nil, false
	}
	debt = uint64(data[8]) | uint64(data[9])<<8 | uint64(data[10])<<16 | uint64(data[11])<<24 |
		uint64(data[12])<<32 | uint64(data[13])<<40 | uint64(data[14])<<48 | uint64(data[15])<<56
	col, ok = jupiterlend.DecodeU128LE(data, 16)
	if !ok {
		return 0, nil, nil, false
	}
	o := 32
	o++ // absorb
	if o >= len(data) {
		return 0, nil, nil, false
	}
	if data[o] == 1 {
		o += 2 // transfer_type: Some(u8)
	} else {
		o += 1
	}
	if o+4 > len(data) {
		return 0, nil, nil, false
	}
	ilen := int(uint32(data[o]) | uint32(data[o+1])<<8 | uint32(data[o+2])<<16 | uint32(data[o+3])<<24)
	o += 4
	if o+ilen > len(data) {
		return 0, nil, nil, false
	}
	idx = append([]uint8{}, data[o:o+ilen]...)
	return debt, col, idx, true
}

// recentLiquidates pulls recent liquidate ixs off the program (named + loaded
// addresses resolved).
func recentLiquidates(endpoint string, scan, want int) []*realLiq {
	prog := solana.MustPublicKeyFromBase58(jupiterlend.VaultsProgram)
	sigs := rpc(endpoint, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "getSignaturesForAddress",
		"params": []any{jupiterlend.VaultsProgram, map[string]any{"limit": scan}},
	})
	var out []*realLiq
	result, _ := sigs["result"].([]any)
	for _, ev := range result {
		e, _ := ev.(map[string]any)
		if e["err"] != nil {
			continue
		}
		sig, _ := e["signature"].(string)
		if sig == "" {
			continue
		}
		tx := rpc(endpoint, map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "getTransaction",
			"params": []any{sig, map[string]any{"encoding": "json", "maxSupportedTransactionVersion": 0, "commitment": "confirmed"}},
		})
		if tx == nil {
			continue
		}
		txResult, _ := tx["result"].(map[string]any)
		if txResult == nil {
			continue
		}
		txn, _ := txResult["transaction"].(map[string]any)
		msg, _ := txn["message"].(map[string]any)
		if msg == nil {
			continue
		}
		baseArr, _ := msg["accountKeys"].([]any)
		if baseArr == nil {
			continue
		}
		var keys []solana.PublicKey
		for _, k := range baseArr {
			s, _ := k.(string)
			if pk, err := solana.PublicKeyFromBase58(s); err == nil {
				keys = append(keys, pk)
			}
		}
		meta, _ := txResult["meta"].(map[string]any)
		if la, ok := meta["loadedAddresses"].(map[string]any); ok {
			for _, side := range []string{"writable", "readonly"} {
				arr, _ := la[side].([]any)
				for _, k := range arr {
					s, _ := k.(string)
					if pk, err := solana.PublicKeyFromBase58(s); err == nil {
						keys = append(keys, pk)
					}
				}
			}
		}
		check := func(ix map[string]any) *realLiq {
			pidxF, ok := ix["programIdIndex"].(float64)
			if !ok {
				return nil
			}
			pidx := int(pidxF)
			if pidx < 0 || pidx >= len(keys) || !keys[pidx].Equals(prog) {
				return nil
			}
			dataStr, _ := ix["data"].(string)
			data, err := base58.Decode(dataStr)
			if err != nil || len(data) < 8 {
				return nil
			}
			var got [8]byte
			copy(got[:], data[:8])
			if got != jupiterlend.LiquidateDisc {
				return nil
			}
			debt, col, idx, ok := decodeLiqArgs(data)
			if !ok {
				return nil
			}
			accIdxs, _ := ix["accounts"].([]any)
			var accts []solana.PublicKey
			for _, iv := range accIdxs {
				fi, ok := iv.(float64)
				if !ok {
					continue
				}
				i := int(fi)
				if i >= 0 && i < len(keys) {
					accts = append(accts, keys[i])
				}
			}
			return &realLiq{Sig: sig, DebtAmt: debt, ColPerUnitDebt: col, Indices: idx, Accounts: accts}
		}
		var found *realLiq
		insArr, _ := msg["instructions"].([]any)
		for _, ixv := range insArr {
			ix, _ := ixv.(map[string]any)
			if r := check(ix); r != nil {
				found = r
				break
			}
		}
		if found == nil {
			innerArr, _ := meta["innerInstructions"].([]any)
			for _, innerV := range innerArr {
				inner, _ := innerV.(map[string]any)
				insArr2, _ := inner["instructions"].([]any)
				for _, ixv := range insArr2 {
					ix, _ := ixv.(map[string]any)
					if r := check(ix); r != nil {
						found = r
						break
					}
				}
				if found != nil {
					break
				}
			}
		}
		if found != nil {
			out = append(out, found)
			if len(out) >= want {
				break
			}
		}
	}
	return out
}

// loadVault loads one vault (config+state) by its vault_config pubkey.
func loadVault(endpoint string, configPk solana.PublicKey) *jupiterlend.Vault {
	cfgRaw, ok := getAcct(endpoint, configPk)
	if !ok {
		return nil
	}
	cfg, ok := jupiterlend.DecodeVaultConfig(cfgRaw)
	if !ok {
		return nil
	}
	statePk := jupiterlend.VaultStatePDA(cfg.VaultID)
	stRaw, ok := getAcct(endpoint, statePk)
	if !ok {
		return nil
	}
	st, ok := jupiterlend.DecodeVaultState(stRaw)
	if !ok {
		return nil
	}
	return &jupiterlend.Vault{ConfigPubkey: configPk, StatePubkey: statePk, Config: cfg, State: st}
}

func simulate(endpoint string, tx *solana.Transaction) map[string]any {
	b64, err := tx.ToBase64()
	if err != nil {
		return nil
	}
	return rpc(endpoint, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "simulateTransaction",
		"params": []any{b64, map[string]any{"sigVerify": false, "replaceRecentBlockhash": true, "commitment": "processed", "encoding": "base64"}},
	})
}

func simValue(raw map[string]any) map[string]any {
	if raw == nil {
		return nil
	}
	result, _ := raw["result"].(map[string]any)
	if result == nil {
		return nil
	}
	value, _ := result["value"].(map[string]any)
	return value
}

func logStrings(v map[string]any, key string) []string {
	arr, _ := v[key].([]any)
	var out []string
	for _, l := range arr {
		if s, ok := l.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// jupAltDeployed is the deployed JUP_ALT (mainnet) — the fixed-liquidate-accounts
// lookup table. Used as the "WITH ALT" A/B input when JUP_ALT isn't exported in
// the environment (it IS on-chain, so the effect is provable either way).
const jupAltDeployed = "DtGiu3mSRTyxypMjwgFLqWqp2rcpPQDHCaC8Rfaf2cyA"

// buildFireWithAlt builds the full flash-loan fire tx with a chosen JUP_ALT
// setting by toggling the env var BuildFireTx reads (same path save_fire uses
// for SAVE_ALT). alt = nil strips JUP_ALT + LIQ_ALT so only Jupiter's own swap
// ALTs apply (the A/B baseline); non-nil folds that table in. Restores the
// prior env afterward.
func buildFireWithAlt(
	endpoint string, cand *jupiterlend.FireCandidate, liquidatorMA, authority solana.PublicKey, alt *solana.PublicKey,
) (*jupiterlend.FireTx, error) {
	savedJup, hadJup := os.LookupEnv("JUP_ALT")
	savedLiq, hadLiq := os.LookupEnv("LIQ_ALT")
	os.Unsetenv("LIQ_ALT")
	if alt != nil {
		os.Setenv("JUP_ALT", alt.String())
	} else {
		os.Unsetenv("JUP_ALT")
	}
	r, err := jupiterlend.BuildFireTx(endpoint, cand, liquidatorMA, authority, nil, 0, 50_000, 100, 16, solana.Hash{})
	if hadJup {
		os.Setenv("JUP_ALT", savedJup)
	} else {
		os.Unsetenv("JUP_ALT")
	}
	if hadLiq {
		os.Setenv("LIQ_ALT", savedLiq)
	} else {
		os.Unsetenv("LIQ_ALT")
	}
	return r, err
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func shortStr(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func defaultTokenProgram() solana.PublicKey {
	return solana.MustPublicKeyFromBase58("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA")
}

func main() {
	envfile.LoadDotEnv()

	endpoint := os.Getenv("HELIUS_RPC")
	if endpoint == "" {
		endpoint = os.Getenv("RPC_HTTP")
	}
	if endpoint == "" {
		fail("HELIUS_RPC (or RPC_HTTP) must be set")
	}
	scan := 1000
	if v := os.Getenv("SCAN_SIGS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			scan = n
		}
	}
	authorityStr := os.Getenv("AUTHORITY")
	if authorityStr == "" {
		authorityStr = "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak"
	}
	authority, err := solana.PublicKeyFromBase58(authorityStr)
	if err != nil {
		fail("bad AUTHORITY: %v", err)
	}
	liquidatorMAStr := os.Getenv("LIQUIDATOR_MA")
	if liquidatorMAStr == "" {
		liquidatorMAStr = "B6e37TbC5n56tWbcgC3RRafUXSuEwRz9ZbhL8Ksro6vD"
	}
	liquidatorMA, err := solana.PublicKeyFromBase58(liquidatorMAStr)
	if err != nil {
		fail("bad LIQUIDATOR_MA: %v", err)
	}

	fmt.Printf("[jup-fire] pulling recent real liquidates (scan %d sigs)…\n", scan)
	reals := recentLiquidates(endpoint, scan, 12)
	fmt.Printf("[jup-fire] captured %d real liquidate ixs\n\n", len(reals))

	// ── STAGE 1: PDA-seed + layout ground-truth check ──────────────────────────
	fmt.Println("═══ STAGE 1: PDA-seed reversal vs real txs ═══")
	ok, bad, checked := 0, 0, 0
	for _, r := range reals {
		if len(r.Indices) != 4 {
			fmt.Printf("  %s : indices %v not len-4 ?!\n", shortStr(r.Sig, 12), r.Indices)
			continue
		}
		if len(r.Accounts) < 26 {
			continue
		}
		vault := loadVault(endpoint, r.Accounts[4])
		if vault == nil {
			continue
		}
		vid := vault.Config.VaultID
		rem := r.Accounts[26:]
		ns, nb, nt, nh := int(r.Indices[0]), int(r.Indices[1]), int(r.Indices[2]), int(r.Indices[3])
		if ns+nb+nt+nh != len(rem) {
			fmt.Printf("  %s vault %d: indices %v sum != %d remaining\n", shortStr(r.Sig, 12), vid, r.Indices, len(rem))
			continue
		}
		branches := rem[ns : ns+nb]
		ticksArr := rem[ns+nb : ns+nb+nt]
		thd := rem[ns+nb+nt:]
		vok := true
		// branches: read → branch_id → re-derive
		for _, pk := range branches {
			checked++
			raw, ok2 := getAcct(endpoint, pk)
			b, ok3 := (*jupiterlend.BranchLite)(nil), false
			if ok2 {
				b, ok3 = jupiterlend.DecodeBranchLite(raw)
			}
			if ok3 && jupiterlend.BranchPDA(vid, b.BranchID).Equals(pk) {
				ok++
			} else {
				bad++
				vok = false
			}
		}
		// ticks: read → tick → re-derive (Tick.tick @ offset 10, i32)
		for _, pk := range ticksArr {
			checked++
			raw, ok2 := getAcct(endpoint, pk)
			matched := false
			if ok2 && len(raw) >= 14 {
				t := int32(uint32(raw[10]) | uint32(raw[11])<<8 | uint32(raw[12])<<16 | uint32(raw[13])<<24)
				matched = jupiterlend.TickPDA(vid, t).Equals(pk)
			}
			if matched {
				ok++
			} else {
				bad++
				vok = false
			}
		}
		// tick_has_debt: read → index (u8 @ offset 10) → re-derive
		for _, pk := range thd {
			checked++
			raw, ok2 := getAcct(endpoint, pk)
			matched := false
			if ok2 && len(raw) > 10 {
				matched = jupiterlend.TickHasDebtPDA(vid, raw[10]).Equals(pk)
			}
			if matched {
				ok++
			} else {
				bad++
				vok = false
			}
		}
		verdict := "✗ mismatch"
		if vok {
			verdict = "✓ all PDAs reproduce"
		}
		fmt.Printf("  %s vault %3d [%s→%s] idx %v  branches %d ticks %d thd %d  %s\n",
			shortStr(r.Sig, 12), vid, shortStr(vault.Config.SupplyToken.String(), 4), vault.Config.DebtLabel(),
			r.Indices, nb, nt, nh, verdict)
	}
	fmt.Printf("  → %d/%d tick/branch/tick_has_debt PDAs reproduced from seeds (%d mismatch)\n\n", ok, checked, bad)

	// ── STAGE 2+3: for EACH captured candidate, derive remaining accounts FRESH
	// from current state and sim the resolver liquidate. The first that composes
	// (VaultLiquidationResult / gated revert, not a size/RPC error) is the proof.
	var simVaultID *uint16
	if v := os.Getenv("SIM_VAULT"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 16); err == nil {
			id := uint16(n)
			simVaultID = &id
		}
	}
	usable := func(r *realLiq) bool {
		return len(r.Accounts) >= 26 && len(r.Indices) == 4 && r.ColPerUnitDebt != nil && !isZeroU128(r.ColPerUnitDebt)
	}
	fetch := func(pk solana.PublicKey) ([]byte, bool) { return getAcct(endpoint, pk) }

	fmt.Println("═══ STAGE 2+3: live selection + resolver sim (per candidate) ═══")
	type proved struct {
		Vault     *jupiterlend.Vault
		Remaining []solana.PublicKey
		Indices   [4]uint8
	}
	var resolverProved *proved
	for _, r := range reals {
		if !usable(r) {
			continue
		}
		vault := loadVault(endpoint, r.Accounts[4])
		if vault == nil {
			continue
		}
		if simVaultID != nil && vault.Config.VaultID != *simVaultID {
			continue
		}
		vid := vault.Config.VaultID
		s := vault.State
		ns := int(r.Indices[0])
		oracleSources := append([]solana.PublicKey{}, r.Accounts[26:26+ns]...)
		// liquidation_tick reconstructed from the captured col_per_unit_debt
		// (production derives it live from the Lazer price).
		liqTick, ok2 := jupiterlend.LiquidationTickFromColPerDebt(
			jupiterlend.U128ToBigInt(r.ColPerUnitDebt), vault.Config.LiquidationPenalty, vault.Config.LiquidationThreshold)
		if !ok2 {
			liqTick = s.TopmostTick - 1
		}
		remaining, indices := jupiterlend.BuildRemainingAccounts(vid, s.TopmostTick, s.CurrentBranchID, liqTick, oracleSources, fetch)

		// Keep the CAPTURED liquidator-side accounts (they satisfy the program's
		// token-owner constraints, as in the real tx); only the remaining/tick
		// accounts are our fresh derivation. col_per_unit_debt=0 = accept oracle
		// price (no false slippage revert). to != DEAD, so this exercises the real
		// liquidate path (not the resolver, which needs to's ATA to exist).
		a, ok3 := jupiterlend.AccountsFromCaptured(vault, r.Accounts)
		if !ok3 {
			continue
		}
		a.Remaining = remaining
		transferTypeOne := uint8(1)
		resolverIx := jupiterlend.BuildLiquidateIx(a, r.DebtAmt, &[2]uint64{0, 0}, false, &transferTypeOne, indices[:])
		tx, err := arb.CompileV0(r.Accounts[0], []solana.Instruction{resolverIx}, nil, solana.Hash{})
		if err != nil {
			continue
		}
		txBytes, err := tx.MarshalBinary()
		if err != nil {
			continue
		}
		fmt.Printf("  vault %3d [%s→%s] liq_tick=%d derived idx %v → %dB: ",
			vid, shortStr(vault.Config.SupplyToken.String(), 4), vault.Config.DebtLabel(), liqTick, indices, len(txBytes))
		if len(txBytes) > 1232 {
			fmt.Println("resolver tx > 1232 (needs ALT for a single-packet fire) — skip sim")
			continue
		}
		raw := simulate(endpoint, tx)
		val := simValue(raw)
		if val == nil {
			errStr := ""
			if raw != nil {
				if e, ok := raw["error"]; ok {
					b, _ := json.Marshal(e)
					errStr = string(b)
				}
			}
			fmt.Printf("RPC error: %s\n", errStr)
			continue
		}
		logs := logStrings(val, "logs")
		// "Composes" = the ix passed account validation and reached the
		// liquidation logic (a Vault* liquidation/slippage gate), or ran clean.
		var gate string
		for _, l := range logs {
			if containsStr(l, "Vault") && (containsStr(l, "Liquidat") || containsStr(l, "Slippage") || containsStr(l, "TopTick") || containsStr(l, "Tick")) {
				gate = l
				break
			}
		}
		if val["err"] == nil {
			fmt.Println("★★ liquidate SIMULATES CLEAN — composes end-to-end; vault liquidatable at live price")
			resolverProved = &proved{Vault: vault, Remaining: remaining, Indices: indices}
			break
		} else if gate != "" {
			fmt.Println("★ composes → gated at the protocol's own liquidation gate")
			fmt.Printf("     %s\n", trimSpace(gate))
			resolverProved = &proved{Vault: vault, Remaining: remaining, Indices: indices}
			break
		} else {
			errB, _ := json.Marshal(val["err"])
			fmt.Printf("upstream revert: %s\n", string(errB))
			tail := logs
			if len(tail) > 4 {
				tail = tail[len(tail)-4:]
			}
			for _, l := range tail {
				fmt.Printf("       %s\n", l)
			}
		}
	}
	if resolverProved == nil {
		fmt.Println("  (no candidate composed cleanly — see per-candidate reasons above)")
	}

	// Full flash-loan-wrapped fire tx (USDC debt only) — compose + size + sim.
	if resolverProved != nil {
		vault, remaining, indices := resolverProved.Vault, resolverProved.Remaining, resolverProved.Indices
		var r *realLiq
		for _, cand := range reals {
			if !usable(cand) {
				continue
			}
			lv := loadVault(endpoint, cand.Accounts[4])
			if lv != nil && lv.Config.VaultID == vault.Config.VaultID {
				r = cand
				break
			}
		}
		fmt.Printf("\n═══ STAGE 4: flash-loan-wrapped fire tx (vault %d) ═══\n", vault.Config.VaultID)
		if r != nil && vault.Config.DebtLabel() == "USDC" {
			fmt.Println("\n  ── full flash-loan-wrapped fire tx ──")
			collatMint := vault.Config.SupplyToken
			ctp, ok2 := mintOwner(endpoint, collatMint)
			if !ok2 {
				ctp = defaultTokenProgram()
			}
			fa, ok3 := jupiterlend.AccountsFromCaptured(vault, r.Accounts)
			if ok3 {
				fa.Remaining = remaining
				// size the seize by the resolver-implied col if available, else a nominal
				colBig := jupiterlend.U128ToBigInt(r.ColPerUnitDebt)
				minFloor := new(big.Int).Exp(big.NewInt(10), big.NewInt(13), nil)
				if colBig.Cmp(minFloor) < 0 {
					colBig = minFloor
				}
				debtBig := new(big.Int).SetUint64(r.DebtAmt)
				seizeBig := new(big.Int).Mul(debtBig, colBig)
				seizeBig.Div(seizeBig, new(big.Int).Exp(big.NewInt(10), big.NewInt(15), nil))
				seize := seizeBig.Uint64()
				if seize < 1 {
					seize = 1
				}
				cand := &jupiterlend.FireCandidate{
					Accts: fa, DebtAmt: r.DebtAmt, ColPerUnitDebt: &[2]uint64{0, 0},
					Remaining: remaining, RemainingIndices: indices,
					SeizeUnderlying: seize, CollateralMint: collatMint, CollateralTokenProgram: ctp,
				}
				jupAltStr := os.Getenv("JUP_ALT")
				if jupAltStr == "" {
					jupAltStr = jupAltDeployed
				}
				jupAlt, err := solana.PublicKeyFromBase58(jupAltStr)
				if err != nil {
					jupAlt = solana.MustPublicKeyFromBase58(jupAltDeployed)
				}
				without, errW := buildFireWithAlt(endpoint, cand, liquidatorMA, authority, nil)
				with, errWi := buildFireWithAlt(endpoint, cand, liquidatorMA, authority, &jupAlt)
				if errW == nil && errWi == nil {
					fmt.Printf("     A/B  without JUP_ALT: %dB   with JUP_ALT: %dB   (Δ −%dB; quoted USDC out %d)\n",
						without.TxBytes, with.TxBytes, without.TxBytes-with.TxBytes, with.QuotedUSDCOut)
					if with.TxBytes <= 1232 {
						val := simValue(simulate(endpoint, with.Tx))
						if val == nil {
							fmt.Println("     fire sim returned nothing")
						} else if val["err"] == nil {
							fmt.Printf("     ★★ FIRE TX SIMULATES CLEAN — would liquidate profitably now (%v CU)\n", val["unitsConsumed"])
						} else {
							errB, _ := json.Marshal(val["err"])
							fmt.Printf("     fire tx gated/other: %s\n", string(errB))
							logs := logStrings(val, "logs")
							tail := logs
							if len(tail) > 6 {
								tail = tail[len(tail)-6:]
							}
							for _, l := range tail {
								fmt.Printf("        %s\n", l)
							}
						}
					} else {
						fmt.Printf("     ⓘ WITH JUP_ALT still %dB > 1232 for this vault — size-gates off (never cached/submitted).\n", with.TxBytes)
					}
				} else {
					e := errW
					if e == nil {
						e = errWi
					}
					fmt.Printf("     fire build failed (often: Jupiter quote for tiny/odd size): %v\n", e)
				}
			}
		} else {
			fmt.Printf("  (vault debt is %s, not USDC — flash-loan wrap is USDC-only; resolver sim already proved the liquidate leg composes.)\n", vault.Config.DebtLabel())
		}
	}

	// ── STAGE 5: PURE-SEED derivation on a NO-RECENT-TX vault ──────────────────
	// The crux: derive the FULL liquidate account set from seeds + on-chain state
	// (NO captured tx), for a vault that has no recent liquidate to lift from, and
	// sim it. Success = resolver revert (VaultLiquidationResult) / a protocol
	// liquidation gate (VaultInvalidLiquidation 6027 = composition proven).
	fmt.Println("\n═══ STAGE 5: pure-seed account set on a NO-RECENT-TX vault ═══")
	authorityUSDC := func() solana.PublicKey {
		usdc := solana.MustPublicKeyFromBase58("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v")
		tp := defaultTokenProgram()
		return flashloan.AtaFor(authority, usdc, tp)
	}()
	// vaults that appeared as a config in a real liquidate → "has recent tx"; the
	// rest are our standalone targets.
	withTx := map[solana.PublicKey]bool{}
	for _, r := range reals {
		if len(r.Accounts) > 4 {
			withTx[r.Accounts[4]] = true
		}
	}
	allVaults := loadAllInscopeUSDC(endpoint)
	fmt.Printf("  %d in-scope USDC vaults; %d of the captured liquidates map to a vault\n", len(allVaults), len(withTx))
	provedStandalone := false
	var provedVault *jupiterlend.Vault
	for _, v := range allVaults {
		if simVaultID != nil && *simVaultID != v.Config.VaultID {
			continue
		}
		hasTx := withTx[v.ConfigPubkey] || resolveRecentLiquidateExists(endpoint, v.ConfigPubkey)
		vid := v.Config.VaultID
		oracleRaw, ok2 := getAcct(endpoint, v.Config.Oracle)
		if !ok2 {
			fmt.Printf("  vault %d: oracle decode failed — skip\n", vid)
			continue
		}
		sources, ok3 := jupiterlend.DecodeOracleSources(oracleRaw)
		if !ok3 {
			fmt.Printf("  vault %d: oracle decode failed — skip\n", vid)
			continue
		}
		stp, ok4 := mintOwner(endpoint, v.Config.SupplyToken)
		if !ok4 {
			stp = defaultTokenProgram()
		}
		btp, ok5 := mintOwner(endpoint, v.Config.BorrowToken)
		if !ok5 {
			btp = defaultTokenProgram()
		}
		a := jupiterlend.DeriveLiquidateAccounts(v, stp, btp)
		// resolver: to = ADDRESS_DEAD (program computes + reverts with the exact
		// liquidation result); signer = authority (its USDC ATA exists as required).
		jupiterlend.SetLiquidatorSide(a, authority, authorityUSDC, jupiterlend.AddressDead,
			flashloan.AtaFor(jupiterlend.AddressDead, v.Config.SupplyToken, stp))
		liqTick := v.State.TopmostTick - 1 // minimal band: include topmost only
		remaining, indices := jupiterlend.BuildRemainingAccounts(vid, v.State.TopmostTick, v.State.CurrentBranchID, liqTick, sources, fetch)
		a.Remaining = remaining
		debt := v.State.TotalBorrow / 50
		if debt < 1_000_000 {
			debt = 1_000_000
		}
		transferTypeOne := uint8(1)
		ix := jupiterlend.BuildLiquidateIx(a, debt, &[2]uint64{0, 0}, false, &transferTypeOne, indices[:])
		tx, err := arb.CompileV0(authority, []solana.Instruction{ix}, nil, solana.Hash{})
		if err != nil {
			continue
		}
		bs, err := tx.MarshalBinary()
		if err != nil {
			continue
		}
		fmt.Printf("  vault %3d [%s→%s] recent_tx=%v src=%d idx=%v liquidate-only %dB: ",
			vid, shortStr(v.Config.SupplyToken.String(), 4), v.Config.DebtLabel(), hasTx, len(sources), indices, len(bs))
		if len(bs) > 1232 {
			fmt.Println("(> 1232, needs ALT to sim standalone) — deriving-only")
			continue
		}
		val := simValue(simulate(endpoint, tx))
		if val == nil {
			fmt.Println("RPC/sim error")
			continue
		}
		logs := logStrings(val, "logs")
		var gate string
		for _, l := range logs {
			if containsStr(l, "VaultLiquidationResult") || containsStr(l, "VaultInvalidLiquidation") ||
				(containsStr(l, "Vault") && (containsStr(l, "Liquidat") || containsStr(l, "Slippage") || containsStr(l, "Tick"))) {
				gate = l
				break
			}
		}
		if val["err"] == nil {
			fmt.Println("★★ SIMULATES CLEAN — full seed-derived set composes, vault liquidatable now")
			provedStandalone = true
			if provedVault == nil {
				provedVault = v
			}
		} else if gate != "" {
			fmt.Println("★ composes → protocol liquidation gate (seed set validated on-chain)")
			fmt.Printf("      %s\n", trimSpace(gate))
			provedStandalone = true
			if provedVault == nil {
				provedVault = v
			}
		} else {
			errB, _ := json.Marshal(val["err"])
			fmt.Printf("revert: %s\n", string(errB))
			tail := logs
			if len(tail) > 5 {
				tail = tail[len(tail)-5:]
			}
			for _, l := range tail {
				fmt.Printf("        %s\n", l)
			}
		}
		if provedStandalone && simVaultID == nil {
			break
		}
	}
	if !provedStandalone {
		fmt.Println("  (no standalone vault reached a clean/gated sim under 1232B without an ALT — the")
		fmt.Println("   seed derivation is validated by jupiter_seed_probe PROOF A; the full fire tx below")
		fmt.Println("   needs the JUP_ALT to fit a single packet, then sims through the liquidity CPI.)")
	}

	// ── STAGE 6: full flash-loan fire tx — JUP_ALT A/B (undeniable size proof) ──
	// Build the SAME wrapped fire twice: once with only Jupiter's own swap ALTs
	// (baseline), once with JUP_ALT folded in. Print both sizes + the delta; when
	// the WITH-ALT packet is ≤ 1232, actually simulate it (sigVerify=false,
	// replaceRecentBlockhash=true, processed). Mirrors the Save A/B (1804→1274).
	if provedVault != nil {
		v := provedVault
		fmt.Printf("\n═══ STAGE 6: full flash-loan fire tx — JUP_ALT A/B (vault %d) ═══\n", v.Config.VaultID)
		jupAltStr := os.Getenv("JUP_ALT")
		if jupAltStr == "" {
			jupAltStr = jupAltDeployed
		}
		jupAlt, err := solana.PublicKeyFromBase58(jupAltStr)
		if err != nil {
			jupAlt = solana.MustPublicKeyFromBase58(jupAltDeployed)
		}
		stp, ok2 := mintOwner(endpoint, v.Config.SupplyToken)
		if !ok2 {
			stp = defaultTokenProgram()
		}
		btp, ok3 := mintOwner(endpoint, v.Config.BorrowToken)
		if !ok3 {
			btp = defaultTokenProgram()
		}
		var sources []solana.PublicKey
		if oracleRaw, ok4 := getAcct(endpoint, v.Config.Oracle); ok4 {
			sources, _ = jupiterlend.DecodeOracleSources(oracleRaw)
		}
		a := jupiterlend.DeriveLiquidateAccounts(v, stp, btp)
		liqTick := v.State.TopmostTick - 1
		remaining, indices := jupiterlend.BuildRemainingAccounts(v.Config.VaultID, v.State.TopmostTick, v.State.CurrentBranchID, liqTick, sources, fetch)
		a.Remaining = remaining
		// Nominal repay for the sim: a fraction of total_borrow, but CAPPED so the
		// marginfi flash-borrow leg doesn't trip the USDC bank's utilization gate
		// (IllegalUtilizationRatio 6026) — that would mask the downstream liquidate
		// gate we want to reach. The real executor sim-gates on a CLEAN sim anyway.
		debt := v.State.TotalBorrow / 50
		if debt < 1_000_000 {
			debt = 1_000_000
		}
		if debt > 25_000_000 {
			debt = 25_000_000
		}
		cand := &jupiterlend.FireCandidate{
			Accts: a, DebtAmt: debt, ColPerUnitDebt: &[2]uint64{0, 0},
			Remaining: remaining, RemainingIndices: indices,
			SeizeUnderlying: maxU64(debt, 1), CollateralMint: v.Config.SupplyToken, CollateralTokenProgram: stp,
		}
		fmt.Printf("     using JUP_ALT %s  (nominal repay %d native)\n", jupAlt, debt)
		without, errW := buildFireWithAlt(endpoint, cand, liquidatorMA, authority, nil)
		with, errWi := buildFireWithAlt(endpoint, cand, liquidatorMA, authority, &jupAlt)
		if errW == nil && errWi == nil {
			delta := without.TxBytes - with.TxBytes
			fmt.Printf("     A/B  without JUP_ALT: %dB   with JUP_ALT: %dB   (Δ −%dB; limit 1232)\n",
				without.TxBytes, with.TxBytes, delta)
			if with.TxBytes <= 1232 {
				fmt.Println("     ✓ WITH JUP_ALT the full wrapped fire fits a single packet — simulating it:")
				val := simValue(simulate(endpoint, with.Tx))
				if val == nil {
					errStr := ""
					raw2 := simulate(endpoint, with.Tx)
					if raw2 != nil {
						if e, ok := raw2["error"].(map[string]any); ok {
							if m, ok := e["message"]; ok {
								b, _ := json.Marshal(m)
								errStr = string(b)
							}
						}
					}
					fmt.Printf("     full fire sim not returned (RPC error): %s\n", errStr)
				} else if val["err"] == nil {
					fmt.Printf("     ★★ FULL FIRE TX SIMULATES CLEAN (%v CU) — seed liquidate + flash-loan + swap composes end-to-end; would liquidate now\n", val["unitsConsumed"])
				} else {
					// A revert at the protocol's own liquidation gate (6027 /
					// VaultInvalidLiquidation) = composition proven; the vault
					// just isn't underwater at the live price.
					logs := logStrings(val, "logs")
					var gate string
					for _, l := range logs {
						if containsStr(l, "6027") || containsStr(l, "VaultInvalidLiquidation") ||
							(containsStr(l, "Vault") && (containsStr(l, "Liquidat") || containsStr(l, "Slippage") || containsStr(l, "Tick"))) {
							gate = l
							break
						}
					}
					if gate != "" {
						fmt.Println("     ★ FULL FIRE composes → gated at the protocol's OWN liquidation gate (fireable wiring, vault healthy now)")
						fmt.Printf("        %s\n", trimSpace(gate))
					} else {
						errB, _ := json.Marshal(val["err"])
						fmt.Printf("     full fire tx reverted upstream: %s\n", string(errB))
					}
					tail := logs
					if len(tail) > 6 {
						tail = tail[len(tail)-6:]
					}
					for _, l := range tail {
						fmt.Printf("        %s\n", l)
					}
				}
			} else {
				fmt.Printf("     ⓘ WITH JUP_ALT still %dB > 1232 for this vault — it SIZE-GATES OFF (the executor never\n", with.TxBytes)
				fmt.Println("       caches/submits a >1232 tx). Other in-scope USDC vaults with fewer tick/branch remaining")
				fmt.Println("       accounts fit; the liquidate LEG is sim-proven above (6027 gate, sub-1232).")
			}
		} else {
			e := errW
			if e == nil {
				e = errWi
			}
			fmt.Printf("     fire build failed (often a Jupiter quote hiccup for the nominal size): %v\n", e)
		}
	}

	fmt.Println("\n[jup-fire] done.")
}

// loadAllInscopeUSDC loads every in-scope USDC-debt vault straight from
// getProgramAccounts (no tx).
func loadAllInscopeUSDC(endpoint string) []*jupiterlend.Vault {
	disc58 := base58.Encode(jupiterlend.VaultConfigDisc[:])
	v := rpc(endpoint, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "getProgramAccounts",
		"params": []any{jupiterlend.VaultsProgram, map[string]any{
			"encoding": "base64",
			"filters":  []any{map[string]any{"memcmp": map[string]any{"offset": 0, "bytes": disc58}}},
		}},
	})
	var out []*jupiterlend.Vault
	result, _ := v["result"].([]any)
	for _, ev := range result {
		e, _ := ev.(map[string]any)
		pkStr, _ := e["pubkey"].(string)
		cpk, err := solana.PublicKeyFromBase58(pkStr)
		if err != nil {
			continue
		}
		acct, _ := e["account"].(map[string]any)
		data, ok := b64field(acct["data"])
		if !ok {
			continue
		}
		cfg, ok := jupiterlend.DecodeVaultConfig(data)
		if !ok || cfg.DebtLabel() != "USDC" {
			continue
		}
		if vault := loadVault(endpoint, cpk); vault != nil {
			out = append(out, vault)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Config.VaultID < out[j].Config.VaultID })
	return out
}

// resolveRecentLiquidateExists is a cheap check: does any recent tx on this
// vault_config carry a liquidate ix?
func resolveRecentLiquidateExists(endpoint string, vaultConfig solana.PublicKey) bool {
	sigs := rpc(endpoint, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "getSignaturesForAddress",
		"params": []any{vaultConfig.String(), map[string]any{"limit": 30}},
	})
	prog := solana.MustPublicKeyFromBase58(jupiterlend.VaultsProgram)
	result, _ := sigs["result"].([]any)
	for _, ev := range result {
		e, _ := ev.(map[string]any)
		if e["err"] != nil {
			continue
		}
		sig, _ := e["signature"].(string)
		if sig == "" {
			continue
		}
		tx := rpc(endpoint, map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "getTransaction",
			"params": []any{sig, map[string]any{"encoding": "json", "maxSupportedTransactionVersion": 0, "commitment": "confirmed"}},
		})
		if tx == nil {
			continue
		}
		txResult, _ := tx["result"].(map[string]any)
		txn, _ := txResult["transaction"].(map[string]any)
		msg, _ := txn["message"].(map[string]any)
		if msg == nil {
			continue
		}
		keysArr, _ := msg["accountKeys"].([]any)
		var keys []solana.PublicKey
		for _, k := range keysArr {
			s, _ := k.(string)
			if pk, err := solana.PublicKeyFromBase58(s); err == nil {
				keys = append(keys, pk)
			}
		}
		insArr, _ := msg["instructions"].([]any)
		for _, ixv := range insArr {
			ix, _ := ixv.(map[string]any)
			pidxF, ok := ix["programIdIndex"].(float64)
			if !ok {
				continue
			}
			pidx := int(pidxF)
			if pidx < 0 || pidx >= len(keys) || !keys[pidx].Equals(prog) {
				continue
			}
			dataStr, _ := ix["data"].(string)
			d, err := base58.Decode(dataStr)
			if err != nil || len(d) < 8 {
				continue
			}
			var got [8]byte
			copy(got[:], d[:8])
			if got == jupiterlend.LiquidateDisc {
				return true
			}
		}
	}
	return false
}

func isZeroU128(v *[2]uint64) bool { return v != nil && v[0] == 0 && v[1] == 0 }

func containsStr(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	n, m := len(s), len(sub)
	for i := 0; i+m <= n; i++ {
		if s[i:i+m] == sub {
			return i
		}
	}
	return -1
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && isSpace(s[start]) {
		start++
	}
	for end > start && isSpace(s[end-1]) {
		end--
	}
	return s[start:end]
}

func isSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }

func maxU64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}
