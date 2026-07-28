// Package rpcclient is a minimal Solana JSON-RPC client — a Go stand-in for
// the ad-hoc `ureq::post(rpc).send_json(...)` calls scattered through the
// Rust probes/executors. It centralizes the request/response envelope and
// exposes typed helpers for the calls this codebase actually makes.
package rpcclient

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"arbengine/internal/solana"
)

type Client struct {
	Endpoint string
	HTTP     *http.Client
}

func New(endpoint string) *Client {
	return &Client{
		Endpoint: endpoint,
		HTTP: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// Call issues a JSON-RPC request and returns the raw `result` field.
func (c *Client) Call(method string, params any) (json.RawMessage, error) {
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: 1, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("rpcclient: decode %s response: %w", method, err)
	}
	if out.Error != nil {
		return nil, fmt.Errorf("rpcclient: %s error %d: %s", method, out.Error.Code, out.Error.Message)
	}
	return out.Result, nil
}

// AccountInfo mirrors the `value` payload of getAccountInfo (base64 encoding).
type AccountInfo struct {
	Lamports   uint64 `json:"lamports"`
	Owner      string `json:"owner"`
	Executable bool   `json:"executable"`
	RentEpoch  uint64 `json:"rentEpoch"`
	Data       []byte `json:"-"`
}

type accountInfoJSON struct {
	Lamports   uint64        `json:"lamports"`
	Owner      string        `json:"owner"`
	Executable bool          `json:"executable"`
	RentEpoch  json.Number   `json:"rentEpoch"`
	Data       []interface{} `json:"data"`
}

// GetAccountInfo fetches and base64-decodes an account's data. Returns
// (nil, nil) if the account doesn't exist (mirrors the Rust code's `Option`
// return on a null `value`).
func (c *Client) GetAccountInfo(pubkey solana.Pubkey) (*AccountInfo, error) {
	raw, err := c.Call("getAccountInfo", []any{pubkey.String(), map[string]string{"encoding": "base64"}})
	if err != nil {
		return nil, err
	}
	var withValue struct {
		Value *accountInfoJSON `json:"value"`
	}
	if err := json.Unmarshal(raw, &withValue); err != nil {
		return nil, err
	}
	if withValue.Value == nil {
		return nil, nil
	}
	v := withValue.Value
	var data []byte
	if len(v.Data) > 0 {
		if s, ok := v.Data[0].(string); ok {
			data, err = base64.StdEncoding.DecodeString(s)
			if err != nil {
				return nil, err
			}
		}
	}
	rentEpoch, _ := v.RentEpoch.Int64()
	return &AccountInfo{
		Lamports:   v.Lamports,
		Owner:      v.Owner,
		Executable: v.Executable,
		RentEpoch:  uint64(rentEpoch),
		Data:       data,
	}, nil
}

// GetAccountData is a convenience wrapper returning just the raw account
// bytes (the common case: pool/reserve/oracle account decoding).
func (c *Client) GetAccountData(pubkey solana.Pubkey) ([]byte, error) {
	info, err := c.GetAccountInfo(pubkey)
	if err != nil {
		return nil, err
	}
	if info == nil {
		return nil, nil
	}
	return info.Data, nil
}

// GetMultipleAccounts batches up to 100 pubkeys per Solana RPC limits.
func (c *Client) GetMultipleAccounts(pubkeys []solana.Pubkey) ([]*AccountInfo, error) {
	addrs := make([]string, len(pubkeys))
	for i, pk := range pubkeys {
		addrs[i] = pk.String()
	}
	raw, err := c.Call("getMultipleAccounts", []any{addrs, map[string]string{"encoding": "base64"}})
	if err != nil {
		return nil, err
	}
	var withValue struct {
		Value []*accountInfoJSON `json:"value"`
	}
	if err := json.Unmarshal(raw, &withValue); err != nil {
		return nil, err
	}
	out := make([]*AccountInfo, len(withValue.Value))
	for i, v := range withValue.Value {
		if v == nil {
			continue
		}
		var data []byte
		if len(v.Data) > 0 {
			if s, ok := v.Data[0].(string); ok {
				data, _ = base64.StdEncoding.DecodeString(s)
			}
		}
		rentEpoch, _ := v.RentEpoch.Int64()
		out[i] = &AccountInfo{Lamports: v.Lamports, Owner: v.Owner, Executable: v.Executable, RentEpoch: uint64(rentEpoch), Data: data}
	}
	return out, nil
}

// GetLatestBlockhash returns the current recent blockhash for tx building.
func (c *Client) GetLatestBlockhash() (solana.Hash, error) {
	raw, err := c.Call("getLatestBlockhash", []any{map[string]string{"commitment": "confirmed"}})
	if err != nil {
		return solana.Hash{}, err
	}
	var withValue struct {
		Value struct {
			Blockhash string `json:"blockhash"`
		} `json:"value"`
	}
	if err := json.Unmarshal(raw, &withValue); err != nil {
		return solana.Hash{}, err
	}
	return solana.HashFromBase58(withValue.Value.Blockhash)
}

// GetSlot returns the current slot.
func (c *Client) GetSlot() (uint64, error) {
	raw, err := c.Call("getSlot", []any{})
	if err != nil {
		return 0, err
	}
	var slot uint64
	if err := json.Unmarshal(raw, &slot); err != nil {
		return 0, err
	}
	return slot, nil
}

// GetBalance returns an account's lamport balance.
func (c *Client) GetBalance(pubkey solana.Pubkey) (uint64, error) {
	raw, err := c.Call("getBalance", []any{pubkey.String()})
	if err != nil {
		return 0, err
	}
	var withValue struct {
		Value uint64 `json:"value"`
	}
	if err := json.Unmarshal(raw, &withValue); err != nil {
		return 0, err
	}
	return withValue.Value, nil
}

// SendTransaction submits a base64-encoded signed transaction and returns
// its signature.
func (c *Client) SendTransaction(txBase64 string, skipPreflight bool) (string, error) {
	raw, err := c.Call("sendTransaction", []any{txBase64, map[string]any{
		"encoding":      "base64",
		"skipPreflight": skipPreflight,
		"maxRetries":    0,
	}})
	if err != nil {
		return "", err
	}
	var sig string
	if err := json.Unmarshal(raw, &sig); err != nil {
		return "", err
	}
	return sig, nil
}

// SimulateTransaction runs `simulateTransaction` and returns the raw result
// (callers decode the parts they need — logs, unitsConsumed, err).
func (c *Client) SimulateTransaction(txBase64 string) (json.RawMessage, error) {
	raw, err := c.Call("simulateTransaction", []any{txBase64, map[string]any{
		"encoding":               "base64",
		"sigVerify":              false,
		"replaceRecentBlockhash": true,
	}})
	if err != nil {
		return nil, err
	}
	var withValue struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(raw, &withValue); err != nil {
		return nil, err
	}
	return withValue.Value, nil
}

// GetTransaction fetches a confirmed transaction's jsonParsed representation.
func (c *Client) GetTransaction(signature string) (json.RawMessage, error) {
	raw, err := c.Call("getTransaction", []any{signature, map[string]any{
		"encoding":                       "jsonParsed",
		"maxSupportedTransactionVersion": 0,
		"commitment":                     "confirmed",
	}})
	if err != nil {
		return nil, err
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	return raw, nil
}

// GetTokenAccountBalance returns the UI (decimal-adjusted) balance of an SPL
// token account.
func (c *Client) GetTokenAccountBalance(pubkey solana.Pubkey) (float64, error) {
	raw, err := c.Call("getTokenAccountBalance", []any{pubkey.String()})
	if err != nil {
		return 0, err
	}
	var withValue struct {
		Value struct {
			UIAmount float64 `json:"uiAmount"`
		} `json:"value"`
	}
	if err := json.Unmarshal(raw, &withValue); err != nil {
		return 0, err
	}
	return withValue.Value.UIAmount, nil
}

// ProgramAccount is one entry of getProgramAccounts.
type ProgramAccount struct {
	Pubkey  solana.Pubkey
	Account AccountInfo
}

type programAccountJSON struct {
	Pubkey  string          `json:"pubkey"`
	Account accountInfoJSON `json:"account"`
}

// GetProgramAccountsOpts controls filters/dataSlice for getProgramAccounts.
type GetProgramAccountsOpts struct {
	Filters   []any
	DataSlice *struct {
		Offset int `json:"offset"`
		Length int `json:"length"`
	}
}

// GetProgramAccounts fetches all accounts owned by a program, optionally filtered.
func (c *Client) GetProgramAccounts(program solana.Pubkey, opts GetProgramAccountsOpts) ([]ProgramAccount, error) {
	cfg := map[string]any{"encoding": "base64"}
	if len(opts.Filters) > 0 {
		cfg["filters"] = opts.Filters
	}
	if opts.DataSlice != nil {
		cfg["dataSlice"] = opts.DataSlice
	}
	raw, err := c.Call("getProgramAccounts", []any{program.String(), cfg})
	if err != nil {
		return nil, err
	}
	var list []programAccountJSON
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	out := make([]ProgramAccount, 0, len(list))
	for _, e := range list {
		pk, err := solana.PubkeyFromBase58(e.Pubkey)
		if err != nil {
			continue
		}
		var data []byte
		if len(e.Account.Data) > 0 {
			if s, ok := e.Account.Data[0].(string); ok {
				data, _ = base64.StdEncoding.DecodeString(s)
			}
		}
		rentEpoch, _ := e.Account.RentEpoch.Int64()
		out = append(out, ProgramAccount{
			Pubkey: pk,
			Account: AccountInfo{
				Lamports:   e.Account.Lamports,
				Owner:      e.Account.Owner,
				Executable: e.Account.Executable,
				RentEpoch:  uint64(rentEpoch),
				Data:       data,
			},
		})
	}
	return out, nil
}

var ErrNotFound = errors.New("rpcclient: not found")
