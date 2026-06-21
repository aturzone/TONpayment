package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aturzone/TONpayment/internal/auth"
	"github.com/aturzone/TONpayment/internal/config"
	"github.com/aturzone/TONpayment/internal/service"
	"github.com/aturzone/TONpayment/internal/store"
	"github.com/aturzone/TONpayment/internal/tenant"
	"github.com/aturzone/TONpayment/internal/tonaddr"
)

const testSessionSecret = "test-session-secret-key-1234"

// newMTServer builds a fully wired multi-tenant server against the disposable test
// Postgres (skips when TON_TEST_DATABASE_URL is unset). ton_proof's crypto is unit
// tested in the auth package; here we mint session tokens directly to exercise the
// control plane, data plane, scoping and checkout over real HTTP.
func newMTServer(t *testing.T) (*http.Server, *store.Postgres) {
	t.Helper()
	url := os.Getenv("TON_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TON_TEST_DATABASE_URL to run the multi-tenant HTTP test")
	}
	pg, err := store.NewPostgres(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	if err := pg.MigrateTenant(context.Background()); err != nil {
		t.Fatal(err)
	}
	secret := []byte(testSessionSecret)
	svc := service.New(pg, paidVerifier{}, "", 15*time.Minute, nil)
	svcs := Services{
		Cfg:           &config.Config{Env: "dev", Multitenant: true},
		Service:       svc,
		Auth:          auth.TenantKeyAuth{Store: pg},
		AuthSvc:       &auth.Service{Store: pg, SessionSecret: secret, Domains: map[string]bool{"tonpayment.net": true}},
		Tenant:        pg,
		TenantSvc:     tenant.New(pg, false),
		SessionSecret: secret,
	}
	return NewServer(svcs), pg
}

// makeMerchant creates a merchant with a fresh random wallet and returns its id,
// wallet, and a valid session token.
func makeMerchant(t *testing.T, pg *store.Postgres) (id, wallet, token string) {
	t.Helper()
	var h [32]byte
	if _, err := rand.Read(h[:]); err != nil {
		t.Fatal(err)
	}
	wallet = tonaddr.Address{Workchain: 0, Hash: h, Bounceable: false}.Friendly(false)
	id = "mer_t_" + hex.EncodeToString(h[:6])
	m := store.Merchant{ID: id, Wallet: wallet, Status: store.MerchantActive, CreatedAt: time.Now().UTC()}
	if err := pg.CreateMerchant(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	token = auth.SignSession([]byte(testSessionSecret), auth.Claims{
		MerchantID: id, Wallet: wallet, IssuedAt: time.Now().Unix(), ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	return id, wallet, token
}

func req(srv *http.Server, method, path, token, apiKey, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if apiKey != "" {
		r.Header.Set("X-API-Key", apiKey)
	}
	srv.Handler.ServeHTTP(rec, r)
	return rec
}

func jsonField(t *testing.T, rec *httptest.ResponseRecorder, key string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode %s: %v (body=%s)", key, err, rec.Body.String())
	}
	s, _ := m[key].(string)
	return s
}

func TestMultiTenantFlow(t *testing.T) {
	srv, pg := newMTServer(t)
	defer pg.Close()
	slug := "t-shop-" + randHex(t)

	_, wallet, token := makeMerchant(t, pg)

	// sign-in endpoints exist: challenge issues a payload
	if rec := req(srv, "POST", "/v1/auth/challenge", "", "", ""); rec.Code != http.StatusOK || jsonField(t, rec, "payload") == "" {
		t.Fatalf("challenge: code=%d body=%s", rec.Code, rec.Body.String())
	}
	// verify with an unknown nonce / bad proof is rejected (401)
	badVerify := `{"address":"` + wallet + `","proof":{"timestamp":` + nowStr() + `,"domain":{"value":"tonpayment.net"},"payload":"nope","signature":"AAAA"}}`
	if rec := req(srv, "POST", "/v1/auth/verify", "", "", badVerify); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad verify: code=%d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}

	// create a gateway (receiving address defaults to the login wallet)
	gwBody := `{"slug":"` + slug + `","displayName":"Test Shop","branding":{"primary":"#0098ea"}}`
	rec := req(srv, "POST", "/v1/gateways", token, "", gwBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create gateway: code=%d body=%s", rec.Code, rec.Body.String())
	}
	gatewayID := jsonField(t, rec, "id")
	if gatewayID == "" {
		t.Fatal("no gateway id")
	}

	// the same wallet cannot host a second gateway (cross-product/duplicate -> 409)
	if rec := req(srv, "POST", "/v1/gateways", token, "", `{"slug":"`+slug+`-2","displayName":"x"}`); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate-wallet gateway: code=%d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}

	// mint an API key
	rec = req(srv, "POST", "/v1/keys", token, "", `{"mode":"live","label":"default"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create key: code=%d body=%s", rec.Code, rec.Body.String())
	}
	apiKey := jsonField(t, rec, "key")
	if !strings.HasPrefix(apiKey, "tpk_live_") {
		t.Fatalf("unexpected key format: %q", apiKey)
	}

	// create an invoice for the gateway using the API key
	rec = req(srv, "POST", "/v1/invoices", "", apiKey, `{"gatewayId":"`+gatewayID+`","amountNano":2500000000}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create invoice: code=%d body=%s", rec.Code, rec.Body.String())
	}
	invID := jsonField(t, rec, "id")
	if invID == "" {
		t.Fatal("no invoice id")
	}

	// creating an invoice without a gatewayId is a 400 in multi-tenant mode
	if rec := req(srv, "POST", "/v1/invoices", "", apiKey, `{"amountNano":1000000000}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invoice without gateway: code=%d, want 400", rec.Code)
	}

	// scoped read with the owning key
	if rec := req(srv, "GET", "/v1/invoices/"+invID, "", apiKey, ""); rec.Code != http.StatusOK {
		t.Fatalf("scoped get: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// public checkout returns the invoice + gateway branding + fee
	rec = req(srv, "GET", "/v1/checkout/"+invID, "", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("checkout: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var co map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &co)
	if co["invoice"] == nil || co["gateway"] == nil || co["fee"] == nil {
		t.Fatalf("checkout missing fields: %s", rec.Body.String())
	}

	// a DIFFERENT merchant's key cannot read the first merchant's invoice (404, no leak)
	_, _, token2 := makeMerchant(t, pg)
	rec = req(srv, "POST", "/v1/keys", token2, "", `{}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create key 2: code=%d body=%s", rec.Code, rec.Body.String())
	}
	apiKey2 := jsonField(t, rec, "key")
	if rec := req(srv, "GET", "/v1/invoices/"+invID, "", apiKey2, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant read: code=%d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}

	// session-protected route rejects a missing token
	if rec := req(srv, "GET", "/v1/me", "", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no session: code=%d, want 401", rec.Code)
	}
	// and accepts a valid one
	if rec := req(srv, "GET", "/v1/me", token, "", ""); rec.Code != http.StatusOK {
		t.Fatalf("me: code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func randHex(t *testing.T) string {
	t.Helper()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b[:])
}

func nowStr() string {
	return strconv.FormatInt(time.Now().Unix(), 10)
}
