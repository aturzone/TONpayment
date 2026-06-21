package wallet

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// PubKeyClient resolves a wallet's ed25519 public key by calling the standard
// get_public_key get-method on the deployed wallet contract (via toncenter v2
// runGetMethod). The public key is what ton_proof signatures verify against.
//
// This is the on-chain (trustworthy) source of the key. A wallet that has never
// been deployed (no outgoing transaction yet) has no get_public_key, so sign-in
// requires a deployed wallet — a reasonable bar for a payment merchant, and far
// stronger than trusting a client-supplied key. (A stateInit-hash fallback for
// brand-new wallets can be added later.)
//
// Its GetPublicKey method matches the auth.PubKeyResolver signature structurally,
// so this package does not import auth.
type PubKeyClient struct {
	apiBase string
	apiKey  string
	http    *http.Client
}

// NewPubKeyClient builds a resolver against the toncenter v2 API. A nil client gets
// a sane default with a request timeout.
func NewPubKeyClient(apiBase, apiKey string, client *http.Client) *PubKeyClient {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &PubKeyClient{apiBase: apiBase, apiKey: apiKey, http: client}
}

// GetPublicKey returns the 32-byte ed25519 public key for address, or an error if
// the contract is not deployed or the method is unavailable.
func (c *PubKeyClient) GetPublicKey(ctx context.Context, address string) (ed25519.PublicKey, error) {
	reqBody, _ := json.Marshal(map[string]any{
		"address": address,
		"method":  "get_public_key",
		"stack":   []any{},
	})
	u := strings.TrimRight(c.apiBase, "/") + "/runGetMethod"
	if c.apiKey != "" {
		u += "?api_key=" + url.QueryEscape(c.apiKey)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("toncenter runGetMethod status %d", resp.StatusCode)
	}
	var out struct {
		OK     bool `json:"ok"`
		Result struct {
			ExitCode int                 `json:"exit_code"`
			Stack    [][]json.RawMessage `json:"stack"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if !out.OK || out.Result.ExitCode != 0 {
		return nil, fmt.Errorf("get_public_key returned exit code %d (wallet not deployed?)", out.Result.ExitCode)
	}
	if len(out.Result.Stack) == 0 || len(out.Result.Stack[0]) < 2 {
		return nil, errors.New("get_public_key: empty result stack")
	}
	var typ, val string
	_ = json.Unmarshal(out.Result.Stack[0][0], &typ)
	_ = json.Unmarshal(out.Result.Stack[0][1], &val)
	if typ != "num" {
		return nil, fmt.Errorf("get_public_key: unexpected stack entry type %q", typ)
	}
	return parsePubKeyNum(val)
}

// parsePubKeyNum converts a toncenter "num" value (hex "0x..." or decimal) into a
// left-padded 32-byte big-endian ed25519 public key.
func parsePubKeyNum(val string) (ed25519.PublicKey, error) {
	val = strings.TrimSpace(val)
	base := 10
	if strings.HasPrefix(val, "0x") || strings.HasPrefix(val, "0X") {
		base, val = 16, val[2:]
	}
	n, ok := new(big.Int).SetString(val, base)
	if !ok {
		return nil, fmt.Errorf("get_public_key: unparseable number %q", val)
	}
	b := n.Bytes()
	if len(b) > ed25519.PublicKeySize {
		return nil, errors.New("get_public_key: oversized key")
	}
	key := make([]byte, ed25519.PublicKeySize)
	copy(key[ed25519.PublicKeySize-len(b):], b)
	return ed25519.PublicKey(key), nil
}
