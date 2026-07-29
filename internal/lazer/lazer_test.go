package lazer

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"arbengine/internal/liquidation"
	"arbengine/internal/pyth"
	"arbengine/internal/solana"
)

func testBank(mint string) *liquidation.Bank {
	return &liquidation.Bank{
		Mint:                 solana.MustPubkeyFromBase58(mint),
		MintDecimals:         9,
		AssetShareValue:      1.0,
		LiabilityShareValue:  1.0,
		AssetWeightInit:      0.8,
		AssetWeightMaint:     0.9,
		LiabilityWeightInit:  1.2,
		LiabilityWeightMaint: 1.1,
		OracleSetup:          3,
		OracleKey:            solana.ZeroPubkey,
		OracleMaxAge:         0,
		EmodeTag:             0,
		EmodeEntries:         nil,
	}
}

// uniquePubkey builds a distinct, deterministic test pubkey (stand-in for
// Rust's Pubkey::new_unique()).
func uniquePubkey(b byte) solana.Pubkey {
	var raw [32]byte
	raw[31] = b
	pk, _ := solana.PubkeyFromBytes(raw[:])
	return pk
}

// priceTableWith spins up a throwaway local WS server that immediately
// pushes one price update in the same shape the real Lazer server sends,
// then drives it through pyth.SpawnLazer (the only way to populate a
// pyth.PriceTable, since its internals are private to the pyth package) and
// waits for the price to land. This mirrors the Rust test's direct
// `table.write().unwrap().insert(...)` but goes through the public API.
func priceTableWith(t *testing.T, feedID uint32, price float64) *pyth.PriceTable {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := context.Background()
		// Drain the subscribe request.
		if _, _, err := c.Read(ctx); err != nil {
			return
		}
		// Encode price as an exact integer with exponent -1 (one decimal
		// digit) so float round-tripping through the wire format is exact
		// for the .5-precision prices these tests use.
		raw := int64(price * 10)
		msg := fmt.Sprintf(`{"type":"streamUpdated","parsed":{"timestampUs":"1","priceFeeds":[{"priceFeedId":%d,"price":"%d","exponent":-1}]}}`,
			feedID, raw)
		_ = c.Write(ctx, websocket.MessageText, []byte(msg))
		// Keep the connection open briefly so the client has time to read.
		time.Sleep(500 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)

	wsURL := "ws" + srv.URL[len("http"):]
	t.Setenv("LAZER_URL", wsURL)

	table := pyth.NewTable()
	pyth.SpawnLazer("test-token", []uint32{feedID}, table)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if p, ok := pyth.Get(table, feedID); ok && p.Price == price {
			return table
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for feed %d to reach %v via Lazer stream", feedID, price)
	return nil
}

func TestLazerOverridesOnChainForMappedMint(t *testing.T) {
	solBank := uniquePubkey(1)
	banks := liquidation.BankMap{solBank: testBank("So11111111111111111111111111111111111111112")}
	onChain := liquidation.PriceMap{solBank: 100.0} // stale on-chain price

	table := priceTableWith(t, LazerSOL, 92.5)

	blended, led := Blend(banks, onChain, table, MintFeedMap())
	if led != 1 {
		t.Fatalf("led = %d, want 1", led)
	}
	if blended[solBank] != 92.5 {
		t.Fatalf("blended price = %v, want 92.5 (Lazer should lead)", blended[solBank])
	}
}

func TestLSTBankKeepsOnChainBaseline(t *testing.T) {
	// mSOL is mapped to the SOL feed but is NOT 1:1 — blending the raw SOL
	// price would undervalue it by the exchange rate. It must keep baseline.
	msolBank := uniquePubkey(2)
	banks := liquidation.BankMap{msolBank: testBank("mSoLzYCxHdYgdzU16g5QSh3i5K3z3KZK7ytfqcJm7So")}
	onChain := liquidation.PriceMap{msolBank: 123.0} // oracle values the LST above raw SOL

	table := priceTableWith(t, LazerSOL, 92.5)

	blended, led := Blend(banks, onChain, table, MintFeedMap())
	if led != 0 {
		t.Fatalf("led = %d, want 0", led)
	}
	if blended[msolBank] != 123.0 {
		t.Fatalf("blended price = %v, want 123.0 (baseline)", blended[msolBank])
	}
}

func TestUnmappedBankKeepsOnChain(t *testing.T) {
	odd := uniquePubkey(3)
	banks := liquidation.BankMap{odd: testBank("2b1kV6Dku6WsWyBxJSKR14pC7sURHfWGrChjjBWY6vfz")} // not mapped
	onChain := liquidation.PriceMap{odd: 3.5}

	table := pyth.NewTable() // no Lazer ticks at all
	blended, led := Blend(banks, onChain, table, MintFeedMap())
	if led != 0 {
		t.Fatalf("led = %d, want 0", led)
	}
	if blended[odd] != 3.5 {
		t.Fatalf("blended price = %v, want 3.5", blended[odd])
	}
}
