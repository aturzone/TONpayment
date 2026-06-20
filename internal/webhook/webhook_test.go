package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/aturzone/TONpayment/internal/store"
)

func TestSenderDeliversSignedWebhook(t *testing.T) {
	var (
		mu      sync.Mutex
		gotSig  string
		gotBody []byte
		once    sync.Once
	)
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotSig, gotBody = r.Header.Get("X-Signature"), b
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		once.Do(func() { close(done) })
	}))
	defer srv.Close()

	s := New(srv.URL, "shhh", nil)
	s.Fire(store.Invoice{ID: "inv_1", Memo: "TON-x"})
	<-done
	s.Wait()

	mu.Lock()
	defer mu.Unlock()
	mac := hmac.New(sha256.New, []byte("shhh"))
	mac.Write(gotBody)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if gotSig != want {
		t.Fatalf("X-Signature = %q, want %q", gotSig, want)
	}
}

func TestNewReturnsNilWhenDisabled(t *testing.T) {
	if New("", "secret", nil) != nil {
		t.Fatal("an empty URL must disable the webhook (nil Sender)")
	}
	// Fire and Wait on a nil Sender must be safe no-ops.
	var s *Sender
	s.Fire(store.Invoice{ID: "x"})
	s.Wait()
}
