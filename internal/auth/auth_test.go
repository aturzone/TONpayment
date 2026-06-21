package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aturzone/TONpayment/internal/store"
	"github.com/aturzone/TONpayment/internal/tonaddr"
)

// signProof produces a valid ton_proof signature for (addr, p), used to drive the
// verifier from the wallet side in tests.
func signProof(priv ed25519.PrivateKey, addr tonaddr.Address, p Proof) []byte {
	inner := sha256.Sum256(proofMessage(addr, p))
	full := append([]byte{0xff, 0xff}, "ton-connect"...)
	full = append(full, inner[:]...)
	digest := sha256.Sum256(full)
	return ed25519.Sign(priv, digest[:])
}

func testAddr(t *testing.T) tonaddr.Address {
	t.Helper()
	var h [32]byte
	if _, err := rand.Read(h[:]); err != nil {
		t.Fatal(err)
	}
	return tonaddr.Address{Workchain: 0, Hash: h, Bounceable: false}
}

func TestProofMessageShape(t *testing.T) {
	addr := testAddr(t)
	p := Proof{Timestamp: 1700000000, Domain: "tonpayment.net", Payload: "nonce123"}
	msg := proofMessage(addr, p)
	prefix := "ton-proof-item-v2/"
	if string(msg[:len(prefix)]) != prefix {
		t.Fatalf("missing prefix: %q", msg[:len(prefix)])
	}
	want := len(prefix) + 4 + 32 + 4 + len(p.Domain) + 8 + len(p.Payload)
	if len(msg) != want {
		t.Fatalf("message length = %d, want %d", len(msg), want)
	}
}

func TestVerifyProof(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	addr := testAddr(t)
	p := Proof{Timestamp: time.Now().Unix(), Domain: "tonpayment.net", Payload: "nonce-abc"}
	p.Signature = signProof(priv, addr, p)

	if !VerifyProof(pub, addr, p) {
		t.Fatal("valid proof must verify")
	}
	// tamper: domain
	bad := p
	bad.Domain = "evil.example"
	if VerifyProof(pub, addr, bad) {
		t.Fatal("domain tamper must fail")
	}
	// tamper: payload
	bad = p
	bad.Payload = "other"
	if VerifyProof(pub, addr, bad) {
		t.Fatal("payload tamper must fail")
	}
	// tamper: signature
	bad = p
	bad.Signature = append([]byte{}, p.Signature...)
	bad.Signature[0] ^= 0xff
	if VerifyProof(pub, addr, bad) {
		t.Fatal("signature tamper must fail")
	}
	// wrong key
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if VerifyProof(otherPub, addr, p) {
		t.Fatal("wrong public key must fail")
	}
	// malformed sizes
	if VerifyProof(ed25519.PublicKey{1, 2, 3}, addr, p) {
		t.Fatal("short pubkey must fail")
	}
}

func TestSessionRoundTrip(t *testing.T) {
	secret := []byte("test-secret")
	now := time.Unix(1700000000, 0)
	c := Claims{MerchantID: "mer_1", Wallet: "UQabc", Admin: true, IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Hour).Unix()}
	tok := SignSession(secret, c)

	got, ok := ParseSession(secret, tok, now)
	if !ok || got.MerchantID != "mer_1" || got.Wallet != "UQabc" || !got.Admin {
		t.Fatalf("round-trip: %+v ok=%v", got, ok)
	}
	// expired
	if _, ok := ParseSession(secret, tok, now.Add(2*time.Hour)); ok {
		t.Fatal("expired token must not parse")
	}
	// wrong secret
	if _, ok := ParseSession([]byte("other"), tok, now); ok {
		t.Fatal("wrong secret must not parse")
	}
	// tampered payload
	if _, ok := ParseSession(secret, "x"+tok, now); ok {
		t.Fatal("tampered token must not parse")
	}
	// malformed
	if _, ok := ParseSession(secret, "no-dot", now); ok {
		t.Fatal("malformed token must not parse")
	}
}

func TestSingleKeyAuth(t *testing.T) {
	// open mode
	open := SingleKeyAuth{Key: ""}
	if _, ok := open.Authenticate(httptest.NewRequest("POST", "/", nil)); !ok {
		t.Fatal("empty key must leave endpoint open")
	}
	// configured key
	a := SingleKeyAuth{Key: "secret"}
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("X-API-Key", "secret")
	if _, ok := a.Authenticate(r); !ok {
		t.Fatal("matching X-API-Key must authorize")
	}
	r = httptest.NewRequest("POST", "/", nil)
	r.Header.Set("Authorization", "Bearer secret")
	if _, ok := a.Authenticate(r); !ok {
		t.Fatal("matching Bearer must authorize")
	}
	r = httptest.NewRequest("POST", "/", nil)
	r.Header.Set("X-API-Key", "wrong")
	if _, ok := a.Authenticate(r); ok {
		t.Fatal("wrong key must be denied")
	}
}

// fakeTS implements only the TenantStore methods the auth package calls; the rest
// come from the embedded nil interface (never invoked here).
type fakeTS struct {
	store.TenantStore
	consumeOK  bool
	byWallet   map[string]store.Merchant
	byID       map[string]store.Merchant
	byHash     map[string]store.APIKey
	createdIDs []string
}

func newFakeTS() *fakeTS {
	return &fakeTS{byWallet: map[string]store.Merchant{}, byID: map[string]store.Merchant{}, byHash: map[string]store.APIKey{}}
}
func (f *fakeTS) PutChallenge(ctx context.Context, nonce string, ttl time.Duration) error { return nil }
func (f *fakeTS) ConsumeChallenge(ctx context.Context, nonce string) (bool, error) {
	return f.consumeOK, nil
}
func (f *fakeTS) GetMerchantByWallet(ctx context.Context, wallet string) (store.Merchant, bool, error) {
	m, ok := f.byWallet[wallet]
	return m, ok, nil
}
func (f *fakeTS) GetMerchant(ctx context.Context, id string) (store.Merchant, bool, error) {
	m, ok := f.byID[id]
	return m, ok, nil
}
func (f *fakeTS) CreateMerchant(ctx context.Context, m store.Merchant) error {
	f.byWallet[m.Wallet] = m
	f.byID[m.ID] = m
	f.createdIDs = append(f.createdIDs, m.ID)
	return nil
}
func (f *fakeTS) GetAPIKeyByHash(ctx context.Context, hash []byte) (store.APIKey, bool, error) {
	k, ok := f.byHash[string(hash)]
	return k, ok, nil
}

func TestServiceVerify(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	addr := testAddr(t)
	fake := newFakeTS()
	fake.consumeOK = true
	now := time.Unix(1700000000, 0)

	svc := &Service{
		Store:         fake,
		Resolve:       func(ctx context.Context, address string) (ed25519.PublicKey, error) { return pub, nil },
		SessionSecret: []byte("sek"),
		Domains:       map[string]bool{"tonpayment.net": true},
		AdminWallets:  map[string]bool{},
		Now:           func() time.Time { return now },
	}

	p := Proof{Timestamp: now.Unix(), Domain: "tonpayment.net", Payload: "nonce-xyz"}
	p.Signature = signProof(priv, addr, p)

	tok, m, err := svc.Verify(context.Background(), VerifyInput{Address: addr.Friendly(false), Proof: p})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if m.Wallet != addr.Friendly(false) || len(fake.createdIDs) != 1 {
		t.Fatalf("merchant upsert wrong: %+v created=%v", m, fake.createdIDs)
	}
	claims, ok := ParseSession(svc.SessionSecret, tok, now)
	if !ok || claims.MerchantID != m.ID || claims.Admin {
		t.Fatalf("session claims wrong: %+v ok=%v", claims, ok)
	}

	// second sign-in for the same wallet reuses the merchant (no new create)
	p2 := Proof{Timestamp: now.Unix(), Domain: "tonpayment.net", Payload: "nonce-2"}
	p2.Signature = signProof(priv, addr, p2)
	if _, m2, err := svc.Verify(context.Background(), VerifyInput{Address: addr.Friendly(false), Proof: p2}); err != nil || m2.ID != m.ID || len(fake.createdIDs) != 1 {
		t.Fatalf("second verify: m2=%+v created=%v err=%v", m2, fake.createdIDs, err)
	}

	// wrong domain
	if _, _, err := svc.Verify(context.Background(), VerifyInput{Address: addr.Friendly(false), Proof: Proof{Timestamp: now.Unix(), Domain: "evil", Payload: "n", Signature: p.Signature}}); err != ErrBadDomain {
		t.Fatalf("bad domain must be rejected, got %v", err)
	}
	// stale timestamp
	stale := Proof{Timestamp: now.Add(-10 * time.Minute).Unix(), Domain: "tonpayment.net", Payload: "n"}
	stale.Signature = signProof(priv, addr, stale)
	if _, _, err := svc.Verify(context.Background(), VerifyInput{Address: addr.Friendly(false), Proof: stale}); err != ErrStaleProof {
		t.Fatalf("stale ts must be rejected, got %v", err)
	}
	// replayed/unknown nonce
	fake.consumeOK = false
	if _, _, err := svc.Verify(context.Background(), VerifyInput{Address: addr.Friendly(false), Proof: p}); err != ErrUsedNonce {
		t.Fatalf("used nonce must be rejected, got %v", err)
	}
}

func TestTenantKeyAuth(t *testing.T) {
	fake := newFakeTS()
	raw := "pk_live_secret"
	hash := HashKey(raw)
	fake.byHash[string(hash)] = store.APIKey{ID: "ak_1", MerchantID: "mer_1", KeyHash: hash, Mode: store.KeyModeLive}
	fake.byID["mer_1"] = store.Merchant{ID: "mer_1", Status: store.MerchantActive}

	a := TenantKeyAuth{Store: fake}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-API-Key", raw)
	p, ok := a.Authenticate(r)
	if !ok || p.MerchantID != "mer_1" || p.Mode != store.KeyModeLive {
		t.Fatalf("valid key: %+v ok=%v", p, ok)
	}
	// unknown key
	r = httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-API-Key", "nope")
	if _, ok := a.Authenticate(r); ok {
		t.Fatal("unknown key must be denied")
	}
	// suspended merchant
	fake.byID["mer_1"] = store.Merchant{ID: "mer_1", Status: store.MerchantSuspended}
	r = httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-API-Key", raw)
	if _, ok := a.Authenticate(r); ok {
		t.Fatal("suspended merchant must be denied")
	}
	// revoked key
	now := time.Now()
	fake.byHash[string(hash)] = store.APIKey{ID: "ak_1", MerchantID: "mer_1", KeyHash: hash, RevokedAt: &now}
	fake.byID["mer_1"] = store.Merchant{ID: "mer_1", Status: store.MerchantActive}
	r = httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-API-Key", raw)
	if _, ok := a.Authenticate(r); ok {
		t.Fatal("revoked key must be denied")
	}
}
