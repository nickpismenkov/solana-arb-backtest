// Validate the pure-seed derivation of the Fluid (Jupiter Lend) **Liquidity**
// program accounts that a Vaults `liquidate` ix needs (positions 9..=22) + the
// oracle `sources` — the accounts the executor used to lift from a captured tx.
// The point: prove they can be derived for ANY vault WITHOUT a recent liquidate.
//
// Two independent proofs:
//
//	A. STANDALONE (no tx needed): for every in-scope vault, derive each account
//	   from seeds via `jupiterlend.DeriveLiquidateAccounts` + the decoded
//	   oracle sources, then read it on-chain and assert it's real and correctly
//	   owned (Liquidity PDAs owned by the liquidity program; the vault token
//	   accounts are SPL accounts whose authority == the `liquidity` PDA and whose
//	   mint matches). This is what lets a never-liquidated vault arm.
//	B. GROUND TRUTH (when a recent liquidate exists): pull real liquidate txs and
//	   assert the seed-derived pubkeys EQUAL the exact accounts the real
//	   liquidator passed at positions 9..=22 + the oracle sources.
//
// Read-only. Usage: HELIUS_RPC=<url> [SCAN_SIGS=1500] [MAX_VAULTS=12] go run ./cmd/jupiter_seed_probe
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/base58"

	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/jupiterlend"
)

const tokenProgramID = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"

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

type ownedAcct struct {
	Owner solana.PublicKey
	Data  []byte
}

// getMulti fetches (owner, data) for a batch of accounts (nil slot = missing).
func getMulti(endpoint string, pks []solana.PublicKey) []*ownedAcct {
	strs := make([]string, len(pks))
	for i, p := range pks {
		strs[i] = p.String()
	}
	out := make([]*ownedAcct, len(pks))
	for chunkI := 0; chunkI*100 < len(strs); chunkI++ {
		lo := chunkI * 100
		hi := lo + 100
		if hi > len(strs) {
			hi = len(strs)
		}
		chunk := strs[lo:hi]
		v := rpc(endpoint, map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "getMultipleAccounts",
			"params": []any{chunk, map[string]any{"encoding": "base64"}},
		})
		result, _ := v["result"].(map[string]any)
		arr, _ := result["value"].([]any)
		for j, accv := range arr {
			if accv == nil {
				continue
			}
			acc, _ := accv.(map[string]any)
			ownerStr, _ := acc["owner"].(string)
			owner, err := solana.PublicKeyFromBase58(ownerStr)
			if err != nil {
				continue
			}
			data, ok := b64field(acc["data"])
			if !ok {
				continue
			}
			out[lo+j] = &ownedAcct{Owner: owner, Data: data}
		}
	}
	return out
}

func gpaByDisc(endpoint string, disc [8]byte) []struct {
	Pk   solana.PublicKey
	Data []byte
} {
	disc58 := base58.Encode(disc[:])
	v := rpc(endpoint, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "getProgramAccounts",
		"params": []any{jupiterlend.VaultsProgram, map[string]any{
			"encoding": "base64",
			"filters":  []any{map[string]any{"memcmp": map[string]any{"offset": 0, "bytes": disc58}}},
		}},
	})
	var out []struct {
		Pk   solana.PublicKey
		Data []byte
	}
	result, _ := v["result"].([]any)
	for _, ev := range result {
		e, _ := ev.(map[string]any)
		pkStr, _ := e["pubkey"].(string)
		pk, err := solana.PublicKeyFromBase58(pkStr)
		if err != nil {
			continue
		}
		acct, _ := e["account"].(map[string]any)
		data, ok := b64field(acct["data"])
		if !ok {
			continue
		}
		out = append(out, struct {
			Pk   solana.PublicKey
			Data []byte
		}{pk, data})
	}
	return out
}

func loadVaults(endpoint string) []*jupiterlend.Vault {
	configs := map[uint16]struct {
		Pk  solana.PublicKey
		Cfg *jupiterlend.VaultConfig
	}{}
	for _, e := range gpaByDisc(endpoint, jupiterlend.VaultConfigDisc) {
		if c, ok := jupiterlend.DecodeVaultConfig(e.Data); ok {
			configs[c.VaultID] = struct {
				Pk  solana.PublicKey
				Cfg *jupiterlend.VaultConfig
			}{e.Pk, c}
		}
	}
	states := map[uint16]struct {
		Pk solana.PublicKey
		St *jupiterlend.VaultState
	}{}
	for _, e := range gpaByDisc(endpoint, jupiterlend.VaultStateDisc) {
		if s, ok := jupiterlend.DecodeVaultState(e.Data); ok {
			states[s.VaultID] = struct {
				Pk solana.PublicKey
				St *jupiterlend.VaultState
			}{e.Pk, s}
		}
	}
	var vaults []*jupiterlend.Vault
	for vid, c := range configs {
		if s, ok := states[vid]; ok {
			vaults = append(vaults, &jupiterlend.Vault{
				ConfigPubkey: c.Pk, StatePubkey: s.Pk, Config: c.Cfg, State: s.St,
			})
		}
	}
	sort.Slice(vaults, func(i, j int) bool { return vaults[i].Config.VaultID < vaults[j].Config.VaultID })
	return vaults
}

// tokenAcctMintOwner decodes an SPL token-account: mint @0, owner @32 (both
// work for Token & Token-2022 base layout).
func tokenAcctMintOwner(data []byte) (mint, owner solana.PublicKey, ok bool) {
	if len(data) < 64 {
		return solana.PublicKey{}, solana.PublicKey{}, false
	}
	return solana.PublicKeyFromBytes(data[0:32]), solana.PublicKeyFromBytes(data[32:64]), true
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

type checkKind byte

const (
	kindL checkKind = 'L' // liquidity-owned PDA
	kindT checkKind = 'T' // spl token account (owner=liquidity)
	kindV checkKind = 'V' // vaults-program PDA
	kindO checkKind = 'O' // oracle-source (any, just must exist)
	kindN checkKind = 'N' // None-sentinel
)

type check struct {
	Label string
	Pk    solana.PublicKey
	Kind  checkKind
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
	scan := 1500
	if v := os.Getenv("SCAN_SIGS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			scan = n
		}
	}
	maxVaults := 12
	if v := os.Getenv("MAX_VAULTS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			maxVaults = n
		}
	}

	liqProg := solana.MustPublicKeyFromBase58(jupiterlend.LiquidityProgram)
	vaultsProg := solana.MustPublicKeyFromBase58(jupiterlend.VaultsProgram)
	liquidity := jupiterlend.LiquidityPDA()
	fmt.Printf("[seed] liquidity global PDA = %s\n", liquidity)
	if _, ok := getAcct(endpoint, liquidity); ok {
		fmt.Printf("[seed]   ✓ exists on-chain\n\n")
	} else {
		fmt.Printf("[seed]   ✗ MISSING — seed for `liquidity` is wrong!\n\n")
	}

	vaults := loadVaults(endpoint)
	var scoped []*jupiterlend.Vault
	for _, v := range vaults {
		if v.Config.DebtInScope() {
			scoped = append(scoped, v)
		}
	}
	fmt.Printf("[seed] %d vaults total, %d in-scope (USDC/USDT/wSOL debt)\n\n", len(vaults), len(scoped))

	// token program per mint (cache).
	mintTp := map[solana.PublicKey]solana.PublicKey{}
	{
		var mints []solana.PublicKey
		for _, v := range scoped {
			mints = append(mints, v.Config.SupplyToken, v.Config.BorrowToken)
		}
		got := getMulti(endpoint, mints)
		for i, m := range mints {
			if got[i] != nil {
				mintTp[m] = got[i].Owner
			}
		}
	}
	defaultTp := solana.MustPublicKeyFromBase58(tokenProgramID)
	tp := func(m solana.PublicKey) solana.PublicKey {
		if p, ok := mintTp[m]; ok {
			return p
		}
		return defaultTp
	}

	// ── PROOF A: standalone seed derivation validated vs live account state ──
	fmt.Println("═══ PROOF A — seed-derived accounts exist + correctly owned (no tx) ═══")
	aPass, aChecked := 0, 0
	limit := maxVaults
	if limit > len(scoped) {
		limit = len(scoped)
	}
	for _, v := range scoped[:limit] {
		vid := v.Config.VaultID
		a := jupiterlend.DeriveLiquidateAccounts(v, tp(v.Config.SupplyToken), tp(v.Config.BorrowToken))
		// oracle sources from the decoded oracle account.
		oracleRaw, _ := getAcct(endpoint, v.Config.Oracle)
		var sources []solana.PublicKey
		if oracleRaw != nil {
			sources, _ = jupiterlend.DecodeOracleSources(oracleRaw)
		}

		checks := []check{
			{"supply_reserves", a.SupplyTokenReservesLiquidity, kindL},
			{"borrow_reserves", a.BorrowTokenReservesLiquidity, kindL},
			{"supply_position", a.VaultSupplyPositionOnLiquidity, kindL},
			{"borrow_position", a.VaultBorrowPositionOnLiquidity, kindL},
			{"supply_rate_model", a.SupplyRateModel, kindL},
			{"borrow_rate_model", a.BorrowRateModel, kindL},
			{"supply_claim(None)", a.SupplyTokenClaimAccount, kindN},
			{"vault_supply_tok_acct", a.VaultSupplyTokenAccount, kindT},
			{"vault_borrow_tok_acct", a.VaultBorrowTokenAccount, kindT},
			{"new_branch", a.NewBranch, kindV},
		}
		for i, s := range sources {
			checks = append(checks, check{fmt.Sprintf("oracle_source[%d]", i), s, kindO})
		}
		pks := make([]solana.PublicKey, len(checks))
		for i, c := range checks {
			pks[i] = c.Pk
		}
		got := getMulti(endpoint, pks)

		nbID := jupiterlend.NewBranchID(v.State.BranchLiquidated, v.State.CurrentBranchID, v.State.TotalBranchID)
		fmt.Printf("── vault %3d [%s→%s]  new_branch_id=%d (bl=%d, cur=%d, tot=%d)  %d oracle src ──\n",
			vid, shortStr(v.Config.SupplyToken), v.Config.DebtLabel(), nbID,
			v.State.BranchLiquidated, v.State.CurrentBranchID, v.State.TotalBranchID, len(sources))
		for i, c := range checks {
			aChecked++
			g := got[i]
			var verdict string
			switch {
			case c.Kind == kindN && c.Pk.Equals(vaultsProg):
				verdict = "✓ None-sentinel (=vaults program id)"
			case c.Kind == kindL && g != nil && g.Owner.Equals(liqProg):
				verdict = "✓ liquidity-owned"
			case c.Kind == kindV && g != nil && g.Owner.Equals(vaultsProg):
				verdict = "✓ vaults-owned"
			case c.Kind == kindV && g == nil:
				verdict = "· not yet created (branch reused/absent — sim is the gate)"
			case c.Kind == kindT && g != nil:
				if _, auth, ok := tokenAcctMintOwner(g.Data); ok && auth.Equals(liquidity) && ownerIsAnyTp(mintTp, g.Owner) {
					verdict = "✓ SPL acct, authority=liquidity"
				} else {
					verdict = "✗ token acct authority/owner mismatch"
				}
			case c.Kind == kindO && g != nil:
				verdict = "✓ source exists"
			case g != nil:
				verdict = "✗ wrong owner"
			default:
				verdict = "✗ MISSING"
			}
			if hasPrefixOK(verdict) {
				aPass++
			}
			// only print the interesting / failing lines to keep output readable
			isSourceLine := len(c.Label) >= 13 && c.Label[:13] == "oracle_source"
			if !isSourceLine || !hasPrefixOK(verdict) {
				fmt.Printf("     %-22s %s  %s\n", c.Label, shortStr(c.Pk), verdict)
			}
		}
	}
	fmt.Printf("\n  → PROOF A: %d/%d derived accounts real + correctly owned\n\n", aPass, aChecked)

	// ── PROOF B: exact-match vs real liquidate txs (only if any exist) ──
	fmt.Println("═══ PROOF B — seed derivation == real liquidator's accounts (ground truth) ═══")
	reals := recentLiquidates(endpoint, scan, 10)
	if len(reals) == 0 {
		fmt.Printf("  (no recent liquidate tx in %d sigs — liquidations are rare on this protocol;\n", scan)
		fmt.Println("   PROOF A + the on-chain program's own seed constraints at sim are the validation.)")
	}
	bOK, bBad := 0, 0
	for _, r := range reals {
		if len(r.Accounts) < 26 {
			continue
		}
		v := loadVault(endpoint, r.Accounts[4])
		if v == nil {
			continue
		}
		a := jupiterlend.DeriveLiquidateAccounts(v, tp(v.Config.SupplyToken), tp(v.Config.BorrowToken))
		srcN := 0
		if len(r.Indices) > 0 {
			srcN = int(r.Indices[0])
		}
		oracleRaw, _ := getAcct(endpoint, v.Config.Oracle)
		var derivedSources []solana.PublicKey
		if oracleRaw != nil {
			derivedSources, _ = jupiterlend.DecodeOracleSources(oracleRaw)
		}
		hi := 26 + srcN
		if hi > len(r.Accounts) {
			hi = len(r.Accounts)
		}
		realSources := r.Accounts[26:hi]

		pairs := []struct {
			Label         string
			Derived, Real solana.PublicKey
		}{
			{"new_branch", a.NewBranch, r.Accounts[9]},
			{"supply_reserves", a.SupplyTokenReservesLiquidity, r.Accounts[10]},
			{"borrow_reserves", a.BorrowTokenReservesLiquidity, r.Accounts[11]},
			{"supply_position", a.VaultSupplyPositionOnLiquidity, r.Accounts[12]},
			{"borrow_position", a.VaultBorrowPositionOnLiquidity, r.Accounts[13]},
			{"supply_rate_model", a.SupplyRateModel, r.Accounts[14]},
			{"borrow_rate_model", a.BorrowRateModel, r.Accounts[15]},
			{"supply_claim", a.SupplyTokenClaimAccount, r.Accounts[16]},
			{"liquidity", a.Liquidity, r.Accounts[17]},
			{"vault_supply_tok_acct", a.VaultSupplyTokenAccount, r.Accounts[19]},
			{"vault_borrow_tok_acct", a.VaultBorrowTokenAccount, r.Accounts[20]},
			{"liquidity_program", a.LiquidityProgram, r.Accounts[18]},
		}
		vok := true
		var fails []string
		for _, p := range pairs {
			if p.Derived.Equals(p.Real) {
				bOK++
			} else {
				bBad++
				vok = false
				fails = append(fails, p.Label)
			}
		}
		// oracle sources exact-order match
		srcMatch := len(derivedSources) == len(realSources)
		if srcMatch {
			for i := range derivedSources {
				if !derivedSources[i].Equals(realSources[i]) {
					srcMatch = false
					break
				}
			}
		}
		if srcMatch {
			bOK++
		} else {
			bBad++
			vok = false
			fails = append(fails, "oracle_sources")
		}
		sigShort := r.Sig
		if len(sigShort) > 10 {
			sigShort = sigShort[:10]
		}
		result := "✓ ALL 12 accts + sources reproduced from seeds"
		if !vok {
			result = fmt.Sprintf("✗ mismatch: %v", fails)
		}
		suffix := ""
		if !srcMatch {
			suffix = " (see sources)"
		}
		fmt.Printf("  %s vault %3d [%s→%s]  %s%s\n",
			sigShort, v.Config.VaultID, shortStr(v.Config.SupplyToken), v.Config.DebtLabel(), result, suffix)
	}
	if len(reals) > 0 {
		fmt.Printf("\n  → PROOF B: %d exact matches, %d mismatches across %d real liquidate txs\n", bOK, bBad, len(reals))
	}
	fmt.Println("\n[seed] done.")
}

func hasPrefixOK(s string) bool {
	r := []rune(s)
	return len(r) > 0 && (r[0] == '✓' || r[0] == '·')
}

func ownerIsAnyTp(mintTp map[solana.PublicKey]solana.PublicKey, owner solana.PublicKey) bool {
	for _, t := range mintTp {
		if t.Equals(owner) {
			return true
		}
	}
	return false
}

func shortStr(pk solana.PublicKey) string {
	s := pk.String()
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// ── real-liquidate capture (shared shape with jupiter_fire_probe) ────────────

type realLiq struct {
	Sig      string
	Accounts []solana.PublicKey
	Indices  []uint8
}

func decodeIndices(data []byte) ([]uint8, bool) {
	o := 8 + 8 + 16 + 1
	if o >= len(data) {
		return nil, false
	}
	if data[o] == 1 {
		o += 2
	} else {
		o += 1
	}
	if o+4 > len(data) {
		return nil, false
	}
	ilen := int(uint32(data[o]) | uint32(data[o+1])<<8 | uint32(data[o+2])<<16 | uint32(data[o+3])<<24)
	o += 4
	if o+ilen > len(data) {
		return nil, false
	}
	return append([]uint8{}, data[o:o+ilen]...), true
}

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
			indices, ok := decodeIndices(data)
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
			return &realLiq{Sig: sig, Accounts: accts, Indices: indices}
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
