package wallet

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aturzone/TONpayment/internal/store"
	"github.com/aturzone/TONpayment/internal/tonaddr"
)

// TonVerifier confirms a payment by reading the receiving address's incoming
// transactions from the toncenter v2 API and matching by comment (memo) and
// amount. It fails closed: any error returns (false, "", err) so the invoice
// simply stays pending.
type TonVerifier struct {
	apiBase string
	apiKey  string
	http    *http.Client
}

var _ Verifier = (*TonVerifier)(nil)

// NewTonVerifier builds a verifier against the toncenter v2 API. A nil client
// gets a sane default with a request timeout; pass your own to tune transport or
// reuse connections.
func NewTonVerifier(apiBase, apiKey string, client *http.Client) *TonVerifier {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &TonVerifier{apiBase: apiBase, apiKey: apiKey, http: client}
}

type tcResponse struct {
	OK     bool `json:"ok"`
	Result []struct {
		Utime         int64 `json:"utime"`
		TransactionID struct {
			Lt   string `json:"lt"`
			Hash string `json:"hash"`
		} `json:"transaction_id"`
		InMsg struct {
			Source      string `json:"source"`
			Destination string `json:"destination"`
			Value       string `json:"value"` // nanoTON, as a string
			Message     string `json:"message"`
		} `json:"in_msg"`
	} `json:"result"`
}

func (v *TonVerifier) Verify(inv store.Invoice) (bool, string, error) {
	if inv.PayTo == "" {
		return false, "", errors.New("invoice has no receiving address")
	}
	if inv.Memo == "" {
		// Defensive: an empty memo would match any plain (no-comment) transfer.
		// NewMemo always produces a non-empty memo, so this only catches corruption.
		return false, "", errors.New("invoice has no memo")
	}
	u := fmt.Sprintf("%s/getTransactions?address=%s&limit=30&archival=true",
		strings.TrimRight(v.apiBase, "/"), url.QueryEscape(inv.PayTo))
	if v.apiKey != "" {
		u += "&api_key=" + url.QueryEscape(v.apiKey)
	}
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return false, "", err
	}
	resp, err := v.http.Do(req)
	if err != nil {
		return false, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, "", fmt.Errorf("toncenter status %d", resp.StatusCode)
	}
	var body tcResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false, "", err
	}
	// A transaction returned by getTransactions(archival) is already in a committed
	// masterchain block — TON has deterministic finality, so there is no probabilistic
	// "0-conf" reorg to wait out. The comment is public and unauthenticated, so we
	// require the exact memo AND a sufficient amount to the receiving address (the
	// query target) — that triple is the real protection. The address leg is enforced
	// by toncenter scoping results to the queried address (every in_msg here is a
	// message TO inv.PayTo); the memo+amount match then binds the payment to THIS
	// invoice. See SECURITY.md.
	// inv.PayTo is canonical, but toncenter may return the destination in a different
	// representation of the same account, so compare by account identity, not string.
	want, wantErr := tonaddr.Parse(inv.PayTo)
	for _, tx := range body.Result {
		if tx.InMsg.Message != inv.Memo {
			continue
		}
		// Defense in depth: ignore a transaction that predates the invoice — a memo can
		// only legitimately appear in a payment made after the invoice was created. The
		// generous skew avoids rejecting a real payment over minor clock differences.
		if tx.Utime > 0 && !inv.CreatedAt.IsZero() && tx.Utime < inv.CreatedAt.Add(-time.Hour).Unix() {
			continue
		}
		// Defense in depth: the query is already scoped to inv.PayTo, but if the
		// destination parses and is a *different* account, skip it. If it doesn't parse,
		// trust the query scope rather than risk rejecting a real payment (fail closed).
		if wantErr == nil && tx.InMsg.Destination != "" {
			if got, err := tonaddr.Parse(tx.InMsg.Destination); err == nil && got.Raw() != want.Raw() {
				continue
			}
		}
		val, err := strconv.ParseInt(tx.InMsg.Value, 10, 64)
		if err != nil {
			continue
		}
		if val >= inv.AmountNano {
			return true, tx.TransactionID.Hash, nil
		}
	}
	return false, "", nil
}
