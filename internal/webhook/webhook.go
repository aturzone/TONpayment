// Package webhook delivers a signed POST to a caller-configured URL when an
// invoice settles, so callers don't have to poll. The body is the invoice JSON;
// the X-Signature header lets the receiver verify authenticity.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/aturzone/TONpayment/internal/store"
)

// Sender POSTs invoice JSON to a callback URL, signed with HMAC-SHA256 so the
// receiver can verify it, retrying a few times with exponential backoff.
// Delivery is asynchronous: Fire returns immediately and the attempts run in a
// goroutine tracked by an internal WaitGroup so shutdown can drain them.
type Sender struct {
	url     string
	secret  []byte
	http    *http.Client
	retries int
	sem     chan struct{} // bounds concurrent deliveries
	wg      sync.WaitGroup
}

// New returns a Sender, or nil if url is empty (webhook disabled). A nil client
// gets a default with a request timeout.
func New(url, secret string, client *http.Client) *Sender {
	if url == "" {
		return nil
	}
	if client == nil {
		// Trusted-URL default: refuse redirects only. The multi-tenant router passes a
		// SecureClient explicitly for untrusted merchant URLs (adds the private-IP block).
		client = redirectSafeClient(10 * time.Second)
	}
	return &Sender{url: url, secret: []byte(secret), http: client, retries: 4, sem: make(chan struct{}, 32)}
}

// Fire delivers the invoice asynchronously. Safe to call on a hot path and safe
// to call on a nil *Sender (no-op).
func (s *Sender) Fire(inv store.Invoice) {
	if s == nil {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.deliverCtx(context.Background(), inv)
	}()
}

// DeliverSync delivers synchronously (with the same signing + retries) and reports
// whether it ultimately succeeded. The multi-tenant webhook router uses it to fan
// out to a gateway's endpoints and record each outcome. Safe on a nil *Sender.
func (s *Sender) DeliverSync(inv store.Invoice) bool {
	if s == nil {
		return false
	}
	return s.deliverCtx(context.Background(), inv)
}

// DeliverSyncContext is DeliverSync bounded by ctx: the retry backoff and every HTTP
// attempt stop the moment ctx is done. A fan-out passes a deadline so one slow or
// black-holing endpoint can't tie up a delivery slot indefinitely.
func (s *Sender) DeliverSyncContext(ctx context.Context, inv store.Invoice) bool {
	if s == nil {
		return false
	}
	return s.deliverCtx(ctx, inv)
}

func (s *Sender) deliverCtx(ctx context.Context, inv store.Invoice) bool {
	select {
	case s.sem <- struct{}{}: // cap concurrent outbound deliveries
	case <-ctx.Done():
		return false
	}
	defer func() { <-s.sem }()

	body, err := json.Marshal(inv)
	if err != nil {
		log.Printf("webhook: marshal invoice %s: %v", inv.ID, err)
		return false
	}
	var sig string
	if len(s.secret) > 0 {
		mac := hmac.New(sha256.New, s.secret)
		mac.Write(body)
		sig = "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}

	backoff := 500 * time.Millisecond
	for attempt := 1; attempt <= s.retries; attempt++ {
		err = s.attempt(ctx, body, sig)
		if err == nil {
			return true // delivered
		}
		log.Printf("webhook: invoice %s attempt %d/%d failed: %v", inv.ID, attempt, s.retries, err)
		if attempt < s.retries {
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return false
			}
			if backoff *= 2; backoff > 8*time.Second {
				backoff = 8 * time.Second
			}
		}
	}
	log.Printf("webhook: giving up on invoice %s after %d attempts", inv.ID, s.retries)
	return false
}

func (s *Sender) attempt(ctx context.Context, body []byte, sig string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "TONpayment-webhook/1")
	if sig != "" {
		req.Header.Set("X-Signature", sig)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body) // drain so the connection can be reused
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

// Wait blocks until in-flight deliveries finish. Call once on shutdown.
func (s *Sender) Wait() {
	if s != nil {
		s.wg.Wait()
	}
}
