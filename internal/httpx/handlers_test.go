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

func TestHealthz(t *testing.T) {
	srv := newTestServer(t)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
}
