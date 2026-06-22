package wallet

import (
	"context"
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
//
// It also implements BatchVerifier: many invoices that share a receiving address
// (a merchant's own wallet hosts all of its invoices) are resolved with a single
// upstream read, and every read is paced by a shared rate limiter — together these
// keep the poller from bursting past toncenter's per-second limit under a backlog.
type TonVerifier struct {
	apiBase string
	apiKey  string
	http    *http.Client
	limiter *evenLimiter
}

var (
	_ Verifier      = (*TonVerifier)(nil)
	_ BatchVerifier = (*TonVerifier)(nil)
)

// NewTonVerifier builds a verifier against the toncenter v2 API. A nil client
// gets a sane default with a request timeout; pass your own to tune transport or
// reuse connections. Rate pacing is OFF by default (the library makes no
// assumptions); the server enables it with SetRateLimit from configuration.
func NewTonVerifier(apiBase, apiKey string, client *http.Client) *TonVerifier {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &TonVerifier{apiBase: apiBase, apiKey: apiKey, http: client, limiter: newEvenLimiter(0)}
}

// SetRateLimit caps upstream reads to rps requests/second (even-paced, no burst),
// shared across every call this verifier makes. 0 disables pacing.
func (v *TonVerifier) SetRateLimit(rps float64) { v.limiter = newEvenLimiter(rps) }

type tcTx struct {
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
}

type tcResponse struct {
	OK     bool   `json:"ok"`
	Result []tcTx `json:"result"`
}

// fetchTransactions reads payTo's recent incoming transactions once, after waiting
// for a slot in the shared rate budget. ctx bounds both the wait and the request.
func (v *TonVerifier) fetchTransactions(ctx context.Context, payTo string) ([]tcTx, error) {
	if payTo == "" {
		return nil, errors.New("invoice has no receiving address")
	}
	if err := v.limiter.wait(ctx); err != nil {
		return nil, err
	}
	u := fmt.Sprintf("%s/getTransactions?address=%s&limit=30&archival=true",
		strings.TrimRight(v.apiBase, "/"), url.QueryEscape(payTo))
	if v.apiKey != "" {
		u += "&api_key=" + url.QueryEscape(v.apiKey)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := v.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 429 included: fail closed, the invoice(s) stay pending and retry next tick.
		return nil, fmt.Errorf("toncenter status %d", resp.StatusCode)
	}
	var body tcResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.Result, nil
}

// matchInvoice reports whether any tx in txs satisfies inv. It is a pure function
// over an already-fetched transaction list, so the single-invoice and batched
// per-address paths apply identical matching.
//
// A transaction returned by getTransactions(archival) is already in a committed
// masterchain block — TON has deterministic finality, so there is no probabilistic
// "0-conf" reorg to wait out. The comment is public and unauthenticated, so we
// require the exact memo AND a sufficient amount to the receiving address (the
// query target) — that triple is the real protection. The address leg is enforced
// by toncenter scoping results to the queried address (every in_msg here is a
// message TO inv.PayTo); the memo+amount match then binds the payment to THIS
// invoice. See SECURITY.md.
func matchInvoice(inv store.Invoice, txs []tcTx) (bool, string) {
	// inv.PayTo is canonical, but toncenter may return the destination in a different
	// representation of the same account, so compare by account identity, not string.
	want, wantErr := tonaddr.Parse(inv.PayTo)
	for _, tx := range txs {
		if tx.InMsg.Message != inv.Memo {
			continue
		}
		// Defense in depth: ignore a transaction that predates the invoice — a memo can
		// only legitimately appear in a payment made after the invoice was created. The
		// generous skew avoids rejecting a real payment over minor clock differences.
		if tx.Utime > 0 && !inv.CreatedAt.IsZero() && tx.Utime < inv.CreatedAt.Add(-time.Hour).Unix() {
			continue
		}
		// Defense in depth: the query is already scoped to inv.PayTo, but verify the
		// destination ourselves too. Fail closed: a present destination must parse
		// AND match our account — a different account OR an unparseable one is not
		// credited. (An absent destination falls back to toncenter's query scope.)
		if wantErr == nil && tx.InMsg.Destination != "" {
			got, err := tonaddr.Parse(tx.InMsg.Destination)
			if err != nil || got.Raw() != want.Raw() {
				continue
			}
		}
		val, err := strconv.ParseInt(tx.InMsg.Value, 10, 64)
		if err != nil {
			continue
		}
		if val >= inv.AmountNano {
			return true, tx.TransactionID.Hash
		}
	}
	return false, ""
}

// Verify confirms a single invoice (the on-demand GET /status path). It bounds the
// whole operation — including any wait for a rate-limit slot — so a status check
// can never hang indefinitely; a timeout simply leaves the invoice pending.
func (v *TonVerifier) Verify(inv store.Invoice) (bool, string, error) {
	if inv.PayTo == "" {
		return false, "", errors.New("invoice has no receiving address")
	}
	if inv.Memo == "" {
		// Defensive: an empty memo would match any plain (no-comment) transfer.
		// NewMemo always produces a non-empty memo, so this only catches corruption.
		return false, "", errors.New("invoice has no memo")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	txs, err := v.fetchTransactions(ctx, inv.PayTo)
	if err != nil {
		return false, "", err
	}
	paid, hash := matchInvoice(inv, txs)
	return paid, hash, nil
}

// VerifyAddress resolves every invoice that shares payTo with a single upstream
// read — the poller's batched hot path. One transaction list is matched against
// all invoices in memory, so N invoices on one merchant wallet cost ONE toncenter
// request instead of N. A read error is reported per-invoice (fail closed), so the
// whole batch simply stays pending and retries next tick.
func (v *TonVerifier) VerifyAddress(ctx context.Context, payTo string, invs []store.Invoice) []AddrResult {
	out := make([]AddrResult, len(invs))
	for i := range invs {
		out[i].Invoice = invs[i]
	}
	txs, err := v.fetchTransactions(ctx, payTo)
	if err != nil {
		for i := range out {
			out[i].Err = err
		}
		return out
	}
	for i := range invs {
		if invs[i].Memo == "" {
			out[i].Err = errors.New("invoice has no memo")
			continue
		}
		out[i].Paid, out[i].TxHash = matchInvoice(invs[i], txs)
	}
	return out
}
