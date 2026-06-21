// Package webhook delivers a signed POST to a caller-configured URL when an
// invoice settles, so callers don't have to poll. The body is the invoice JSON;
// the X-Signature header lets the receiver verify authenticity.
package webhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
		// Don't follow redirects on outbound delivery — a redirect could send the
		// signed invoice payload to an unintended (e.g. internal) host (SSRF).
		client = &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("webhook: redirects are not followed")
			},
		}
	}
	return &Sender{url: url, secret: []byte(secret), http: client, retries: 5, sem: make(chan struct{}, 32)}
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
		s.deliver(inv)
	}()
}

// DeliverSync delivers synchronously (with the same signing + retries) and reports
// whether it ultimately succeeded. The multi-tenant webhook router uses it to fan
// out to a gateway's endpoints and record each outcome. Safe on a nil *Sender.
func (s *Sender) DeliverSync(inv store.Invoice) bool {
	if s == nil {
		return false
	}
	return s.deliver(inv)
}

func (s *Sender) deliver(inv store.Invoice) bool {
	s.sem <- struct{}{} // cap concurrent outbound deliveries
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
		err = s.attempt(body, sig)
		if err == nil {
			return true // delivered
		}
		log.Printf("webhook: invoice %s attempt %d/%d failed: %v", inv.ID, attempt, s.retries, err)
		if attempt < s.retries {
			time.Sleep(backoff)
			if backoff *= 2; backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
	log.Printf("webhook: giving up on invoice %s after %d attempts", inv.ID, s.retries)
	return false
}

func (s *Sender) attempt(body []byte, sig string) error {
	req, err := http.NewRequest(http.MethodPost, s.url, bytes.NewReader(body))
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
