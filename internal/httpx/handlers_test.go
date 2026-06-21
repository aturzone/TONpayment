package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aturzone/TONpayment/internal/config"
	"github.com/aturzone/TONpayment/internal/service"
	"github.com/aturzone/TONpayment/internal/store"
)

// paidVerifier always reports the invoice as paid, so a /status call settles it.
type paidVerifier struct{}

func (paidVerifier) Verify(_ store.Invoice) (bool, string, error) { return true, "tx-http", nil }

func newTestServer(t *testing.T) *http.Server {
	t.Helper()
	st, err := store.NewMemory("")
	if err != nil {
		t.Fatal(err)
	}
	svc := service.New(st, paidVerifier{}, "UQpay", 15*time.Minute, nil)
	return NewServer(Services{Cfg: &config.Config{Env: "dev"}, Service: svc})
}

// TestCreateThenStatusPaid walks the headline flow over real HTTP: create an
// invoice, then hit /status with a mock-paid verifier and expect "paid".
func TestCreateThenStatusPaid(t *testing.T) {
	srv := newTestServer(t)

	// create
	body := `{"amountNano":2500000000,"ttlSeconds":900,"metadata":{"orderId":"o1"}}`
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/invoices", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatalf("no id in create response: %v", created)
	}
	if dl, _ := created["deeplink"].(string); !strings.HasPrefix(dl, "ton://transfer/UQpay?") {
		t.Fatalf("bad deeplink: %q", created["deeplink"])
	}
	if created["status"] != store.StatusPending {
		t.Fatalf("new invoice status = %v, want pending", created["status"])
	}

	// status -> paid
	rec2 := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/v1/invoices/"+id+"/status", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("status code = %d", rec2.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec2.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["status"] != store.StatusPaid {
		t.Fatalf("status = %v, want paid", got["status"])
	}
	if got["txHash"] != "tx-http" {
		t.Fatalf("txHash = %v, want tx-http", got["txHash"])
	}
}

func TestGetUnknownInvoice404(t *testing.T) {
	srv := newTestServer(t)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/invoices/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", rec.Code)
	}
}

func TestCreateRejectsBadAmount(t *testing.T) {
	srv := newTestServer(t)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/invoices", strings.NewReader(`{"amountNano":0}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rec.Code)
	}
}

func TestCreateRejectsBadPayTo(t *testing.T) {
	srv := newTestServer(t)
	rec := httptest.NewRecorder()
	body := `{"amountNano":1000000000,"payTo":"not-a-ton-address"}`
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/invoices", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 for an invalid payTo", rec.Code)
	}
}

func TestCreateRequiresAPIKeyWhenConfigured(t *testing.T) {
	st, err := store.NewMemory("")
	if err != nil {
		t.Fatal(err)
	}
	svc := service.New(st, paidVerifier{}, "UQpay", 15*time.Minute, nil)
	srv := NewServer(Services{Cfg: &config.Config{Env: "dev", CreateAPIKey: "secret"}, Service: svc})
	body := `{"amountNano":1000000000}`

	// no key -> 401
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/invoices", strings.NewReader(body)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: code = %d, want 401", rec.Code)
	}

	// X-API-Key -> 201
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/invoices", strings.NewReader(body))
	req2.Header.Set("X-API-Key", "secret")
	srv.Handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusCreated {
		t.Fatalf("X-API-Key: code = %d, want 201", rec2.Code)
	}

	// Authorization: Bearer -> 201
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodPost, "/v1/invoices", strings.NewReader(body))
	req3.Header.Set("Authorization", "Bearer secret")
	srv.Handler.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusCreated {
		t.Fatalf("Bearer key: code = %d, want 201", rec3.Code)
	}
}

func TestHealthz(t *testing.T) {
	srv := newTestServer(t)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
}

// TestReadsRequireAPIKeyWhenConfigured locks in that an invoice id is not an
// authorization: with a key set, every read (get/status/qr) is gated (S-0001/S-0003).
func TestReadsRequireAPIKeyWhenConfigured(t *testing.T) {
	st, err := store.NewMemory("")
	if err != nil {
		t.Fatal(err)
	}
	svc := service.New(st, paidVerifier{}, "UQpay", 15*time.Minute, nil)
	srv := NewServer(Services{Cfg: &config.Config{Env: "dev", CreateAPIKey: "secret"}, Service: svc})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/invoices", strings.NewReader(`{"amountNano":1000000000}`))
	req.Header.Set("X-API-Key", "secret")
	srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: code = %d, body=%s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatal("no id in create response")
	}

	for _, path := range []string{"/v1/invoices/" + id, "/v1/invoices/" + id + "/status", "/v1/invoices/" + id + "/qr"} {
		r := httptest.NewRecorder()
		srv.Handler.ServeHTTP(r, httptest.NewRequest(http.MethodGet, path, nil))
		if r.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s without key: code = %d, want 401", path, r.Code)
		}
		r2 := httptest.NewRecorder()
		req2 := httptest.NewRequest(http.MethodGet, path, nil)
		req2.Header.Set("X-API-Key", "secret")
		srv.Handler.ServeHTTP(r2, req2)
		if r2.Code != http.StatusOK {
			t.Fatalf("GET %s with key: code = %d, want 200", path, r2.Code)
		}
	}
}

// TestRejectsUnknownAndTrailingJSON locks in strict body parsing for the money
// API: unknown fields, trailing junk, and multi-object bodies are 400 (S-0005).
func TestRejectsUnknownAndTrailingJSON(t *testing.T) {
	srv := newTestServer(t)
	for _, body := range []string{
		`{"amountNano":1000000000,"bogus":"x"}`,
		`{"amountNano":1000000000} trailing`,
		`{"amountNano":1000000000}{"amountNano":2}`,
	} {
		rec := httptest.NewRecorder()
		srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/invoices", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %q: code = %d, want 400", body, rec.Code)
		}
	}
}
