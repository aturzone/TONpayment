package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// openTestPG connects to the disposable test database named by
// TON_TEST_DATABASE_URL, skipping the test when it is unset.
func openTestPG(t *testing.T) *Postgres {
	t.Helper()
	url := os.Getenv("TON_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TON_TEST_DATABASE_URL to run the Postgres integration test")
	}
	pg, err := NewPostgres(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	return pg
}

// Runs only when TON_TEST_DATABASE_URL points at a disposable Postgres (CI or a
// local throwaway cluster). Skipped otherwise so `go test ./...` stays green.
// Mirrors postgres_test.go's gating.
func TestTenantStore(t *testing.T) {
	pg := openTestPG(t) // skips if env unset
	defer pg.Close()
	ctx := context.Background()

	if err := pg.MigrateTenant(ctx); err != nil {
		t.Fatalf("MigrateTenant: %v", err)
	}
	// Idempotent: a second apply must not error.
	if err := pg.MigrateTenant(ctx); err != nil {
		t.Fatalf("MigrateTenant (second apply): %v", err)
	}
	cleanupTenant(t, pg)

	t.Run("merchants", func(t *testing.T) { testMerchants(t, ctx, pg) })
	t.Run("gateways", func(t *testing.T) { testGateways(t, ctx, pg) })
	t.Run("apiKeys", func(t *testing.T) { testAPIKeys(t, ctx, pg) })
	t.Run("walletUniqueness", func(t *testing.T) { testWalletUniqueness(t, ctx, pg) })
	t.Run("challenges", func(t *testing.T) { testChallenges(t, ctx, pg) })
	t.Run("platformConfig", func(t *testing.T) { testPlatformConfig(t, ctx, pg) })
	t.Run("webhooks", func(t *testing.T) { testWebhooks(t, ctx, pg) })
	t.Run("scopedInvoices", func(t *testing.T) { testScopedInvoices(t, ctx, pg) })
}

// cleanupTenant removes any rows from a previous run so the test is re-runnable.
// merchants cascade to gateways/api_keys/webhook_endpoints via ON DELETE CASCADE.
func cleanupTenant(t *testing.T, pg *Postgres) {
	t.Helper()
	ctx := context.Background()
	stmts := []string{
		`DELETE FROM merchants WHERE id LIKE 't-%'`,
		`DELETE FROM wallet_ownership WHERE owner_id LIKE 't-%' OR address LIKE 'tW-%'`,
		`DELETE FROM invoices WHERE id LIKE 't-%'`,
		`DELETE FROM auth_challenges WHERE nonce LIKE 't-%'`,
		`DELETE FROM webhook_deliveries WHERE invoice_id LIKE 't-%'`,
	}
	for _, s := range stmts {
		if _, err := pg.pool.Exec(ctx, s); err != nil {
			t.Fatalf("cleanup %q: %v", s, err)
		}
	}
}

func testMerchants(t *testing.T, ctx context.Context, pg *Postgres) {
	m := Merchant{ID: "t-mer-1", Wallet: "tW-merchant-1"}
	if err := pg.CreateMerchant(ctx, m); err != nil {
		t.Fatalf("CreateMerchant: %v", err)
	}
	got, ok, err := pg.GetMerchant(ctx, "t-mer-1")
	if err != nil || !ok {
		t.Fatalf("GetMerchant: ok=%v err=%v", ok, err)
	}
	if got.Wallet != "tW-merchant-1" || got.Status != MerchantActive || got.CreatedAt.IsZero() {
		t.Fatalf("merchant defaults wrong: %+v", got)
	}
	byWallet, ok, err := pg.GetMerchantByWallet(ctx, "tW-merchant-1")
	if err != nil || !ok || byWallet.ID != "t-mer-1" {
		t.Fatalf("GetMerchantByWallet: %+v ok=%v err=%v", byWallet, ok, err)
	}
	if _, ok, _ := pg.GetMerchantByWallet(ctx, "tW-nope"); ok {
		t.Fatal("GetMerchantByWallet for unknown wallet must be not-found")
	}
	if err := pg.SetMerchantStatus(ctx, "t-mer-1", MerchantSuspended); err != nil {
		t.Fatalf("SetMerchantStatus: %v", err)
	}
	if got, _, _ := pg.GetMerchant(ctx, "t-mer-1"); got.Status != MerchantSuspended {
		t.Fatalf("status not suspended: %+v", got)
	}
	// reactivate so downstream subtests see an active merchant
	_ = pg.SetMerchantStatus(ctx, "t-mer-1", MerchantActive)

	list, err := pg.ListMerchants(ctx, 10, 0)
	if err != nil || len(list) < 1 {
		t.Fatalf("ListMerchants: n=%d err=%v", len(list), err)
	}
}

func testGateways(t *testing.T, ctx context.Context, pg *Postgres) {
	g := Gateway{
		ID: "t-gw-1", MerchantID: "t-mer-1", Slug: "t-shop", DisplayName: "Test Shop",
		Branding: map[string]any{"primary": "#0098ea", "theme": "dark"},
		ReceivingAddress: "tW-merchant-1", Active: true,
	}
	if err := pg.CreateGateway(ctx, g); err != nil {
		t.Fatalf("CreateGateway: %v", err)
	}
	got, ok, err := pg.GetGateway(ctx, "t-gw-1")
	if err != nil || !ok {
		t.Fatalf("GetGateway: ok=%v err=%v", ok, err)
	}
	if got.Slug != "t-shop" || got.Branding["primary"] != "#0098ea" || got.Branding["theme"] != "dark" {
		t.Fatalf("gateway branding round-trip wrong: %+v", got)
	}
	bySlug, ok, err := pg.GetGatewayBySlug(ctx, "t-shop")
	if err != nil || !ok || bySlug.ID != "t-gw-1" {
		t.Fatalf("GetGatewayBySlug: %+v ok=%v err=%v", bySlug, ok, err)
	}
	// slug uniqueness
	dup := g
	dup.ID = "t-gw-dup"
	if err := pg.CreateGateway(ctx, dup); err == nil {
		t.Fatal("duplicate slug must violate the unique index")
	}
	// update branding + active
	got.DisplayName = "Renamed"
	got.Branding = map[string]any{"primary": "#ff0000"}
	got.Active = false
	if err := pg.UpdateGateway(ctx, got); err != nil {
		t.Fatalf("UpdateGateway: %v", err)
	}
	after, _, _ := pg.GetGateway(ctx, "t-gw-1")
	if after.DisplayName != "Renamed" || after.Active != false || after.Branding["primary"] != "#ff0000" {
		t.Fatalf("UpdateGateway not applied: %+v", after)
	}
	// reactivate for webhook subtest
	after.Active = true
	_ = pg.UpdateGateway(ctx, after)

	list, err := pg.ListGatewaysByMerchant(ctx, "t-mer-1")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListGatewaysByMerchant: n=%d err=%v", len(list), err)
	}
}

func testAPIKeys(t *testing.T, ctx context.Context, pg *Postgres) {
	hash := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	k := APIKey{ID: "t-ak-1", MerchantID: "t-mer-1", KeyHash: hash, KeyPrefix: "pk_live_aaaa", Mode: KeyModeLive, Label: "default"}
	if err := pg.CreateAPIKey(ctx, k); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	got, ok, err := pg.GetAPIKeyByHash(ctx, hash)
	if err != nil || !ok || got.ID != "t-ak-1" || got.MerchantID != "t-mer-1" || got.RevokedAt != nil {
		t.Fatalf("GetAPIKeyByHash: %+v ok=%v err=%v", got, ok, err)
	}
	if _, ok, _ := pg.GetAPIKeyByHash(ctx, []byte{9, 9, 9}); ok {
		t.Fatal("unknown hash must be not-found")
	}
	// hash uniqueness
	if err := pg.CreateAPIKey(ctx, APIKey{ID: "t-ak-dup", MerchantID: "t-mer-1", KeyHash: hash, KeyPrefix: "x", Mode: KeyModeLive}); err == nil {
		t.Fatal("duplicate key_hash must violate the unique index")
	}
	if err := pg.RevokeAPIKey(ctx, "t-ak-1", time.Now().UTC()); err != nil {
		t.Fatalf("RevokeAPIKey: %v", err)
	}
	if got, _, _ := pg.GetAPIKeyByHash(ctx, hash); got.RevokedAt == nil {
		t.Fatal("key should be revoked (RevokedAt set), still returned for the caller to reject")
	}
	list, err := pg.ListAPIKeysByMerchant(ctx, "t-mer-1")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListAPIKeysByMerchant: n=%d err=%v", len(list), err)
	}
}

func testWalletUniqueness(t *testing.T, ctx context.Context, pg *Postgres) {
	addr := "tW-shared-1"
	// A donation link claims it first.
	if err := pg.ClaimWallet(ctx, WalletOwner{Address: addr, Product: ProductDonation, OwnerID: "t-creator-1"}); err != nil {
		t.Fatalf("first ClaimWallet (donation): %v", err)
	}
	// A payment gateway must NOT be able to claim the same wallet.
	err := pg.ClaimWallet(ctx, WalletOwner{Address: addr, Product: ProductPayment, OwnerID: "t-mer-1"})
	if !errors.Is(err, ErrWalletTaken) {
		t.Fatalf("second ClaimWallet must return ErrWalletTaken, got %v", err)
	}
	// Even the same product/owner can't double-claim.
	if err := pg.ClaimWallet(ctx, WalletOwner{Address: addr, Product: ProductDonation, OwnerID: "t-creator-1"}); !errors.Is(err, ErrWalletTaken) {
		t.Fatalf("re-claim must return ErrWalletTaken, got %v", err)
	}
	owner, ok, err := pg.GetWalletOwner(ctx, addr)
	if err != nil || !ok || owner.Product != ProductDonation {
		t.Fatalf("GetWalletOwner: %+v ok=%v err=%v", owner, ok, err)
	}
	// Releasing frees it for the other product.
	if err := pg.ReleaseWallet(ctx, addr, "t-creator-1"); err != nil {
		t.Fatalf("ReleaseWallet: %v", err)
	}
	if err := pg.ClaimWallet(ctx, WalletOwner{Address: addr, Product: ProductPayment, OwnerID: "t-mer-1"}); err != nil {
		t.Fatalf("re-claim after release: %v", err)
	}
}

func testChallenges(t *testing.T, ctx context.Context, pg *Postgres) {
	if err := pg.PutChallenge(ctx, "t-nonce-1", 5*time.Minute); err != nil {
		t.Fatalf("PutChallenge: %v", err)
	}
	won, err := pg.ConsumeChallenge(ctx, "t-nonce-1")
	if err != nil || !won {
		t.Fatalf("first consume must win: won=%v err=%v", won, err)
	}
	// single-use: the second consume must lose (anti-replay).
	won2, err := pg.ConsumeChallenge(ctx, "t-nonce-1")
	if err != nil || won2 {
		t.Fatalf("second consume must lose: won=%v err=%v", won2, err)
	}
	// unknown nonce loses.
	if won, _ := pg.ConsumeChallenge(ctx, "t-nonce-unknown"); won {
		t.Fatal("unknown nonce must not be consumable")
	}
	// expired nonce loses.
	if err := pg.PutChallenge(ctx, "t-nonce-exp", -1*time.Second); err != nil {
		t.Fatalf("PutChallenge expired: %v", err)
	}
	if won, _ := pg.ConsumeChallenge(ctx, "t-nonce-exp"); won {
		t.Fatal("expired nonce must not be consumable")
	}
}

func testPlatformConfig(t *testing.T, ctx context.Context, pg *Postgres) {
	// seeded row defaults to zero fee
	c, err := pg.GetPlatformConfig(ctx)
	if err != nil {
		t.Fatalf("GetPlatformConfig: %v", err)
	}
	_ = c // value depends on prior runs; just assert set/get below
	if err := pg.SetPlatformConfig(ctx, 150, "tW-fee-wallet"); err != nil {
		t.Fatalf("SetPlatformConfig: %v", err)
	}
	got, err := pg.GetPlatformConfig(ctx)
	if err != nil || got.FeeBps != 150 || got.FeeWallet != "tW-fee-wallet" || got.UpdatedAt.IsZero() {
		t.Fatalf("platform config set/get: %+v err=%v", got, err)
	}
	// reset to zero so we don't leave a fee configured
	_ = pg.SetPlatformConfig(ctx, 0, "")
}

func testWebhooks(t *testing.T, ctx context.Context, pg *Postgres) {
	e := WebhookEndpoint{ID: "t-whk-1", GatewayID: "t-gw-1", URL: "https://example.com/hook", Secret: "shh", Active: true}
	if err := pg.CreateWebhookEndpoint(ctx, e); err != nil {
		t.Fatalf("CreateWebhookEndpoint: %v", err)
	}
	inactive := WebhookEndpoint{ID: "t-whk-2", GatewayID: "t-gw-1", URL: "https://example.com/old", Secret: "x", Active: false}
	if err := pg.CreateWebhookEndpoint(ctx, inactive); err != nil {
		t.Fatalf("CreateWebhookEndpoint inactive: %v", err)
	}
	all, err := pg.ListWebhookEndpoints(ctx, "t-gw-1")
	if err != nil || len(all) != 2 {
		t.Fatalf("ListWebhookEndpoints: n=%d err=%v", len(all), err)
	}
	active, err := pg.ListActiveWebhookEndpoints(ctx, "t-gw-1")
	if err != nil || len(active) != 1 || active[0].ID != "t-whk-1" {
		t.Fatalf("ListActiveWebhookEndpoints: %+v err=%v", active, err)
	}
	if err := pg.RecordWebhookDelivery(ctx, "t-inv-1", "t-whk-1", 1, 200, true); err != nil {
		t.Fatalf("RecordWebhookDelivery: %v", err)
	}
}

func testScopedInvoices(t *testing.T, ctx context.Context, pg *Postgres) {
	now := time.Now().UTC().Truncate(time.Second)
	// a multi-tenant invoice owned by t-mer-1
	mt := Invoice{
		ID: "t-inv-mt", PayTo: "tW-merchant-1", Memo: "TON-mt", AmountNano: 1_000_000_000,
		Currency: "TON", Status: StatusPending, MerchantID: "t-mer-1", GatewayID: "t-gw-1", CreatedAt: now,
	}
	if err := pg.CreateInvoice(mt); err != nil {
		t.Fatalf("CreateInvoice (mt): %v", err)
	}
	// a single-tenant invoice (no merchant) — must never be visible to a merchant scope
	st := Invoice{
		ID: "t-inv-st", PayTo: "tW-single", Memo: "TON-st", AmountNano: 1,
		Currency: "TON", Status: StatusPending, CreatedAt: now,
	}
	if err := pg.CreateInvoice(st); err != nil {
		t.Fatalf("CreateInvoice (st): %v", err)
	}

	// right merchant sees it, and the tenancy columns round-tripped
	got, ok, err := pg.GetInvoiceForMerchant(ctx, "t-inv-mt", "t-mer-1")
	if err != nil || !ok || got.MerchantID != "t-mer-1" || got.GatewayID != "t-gw-1" {
		t.Fatalf("GetInvoiceForMerchant (owner): %+v ok=%v err=%v", got, ok, err)
	}
	// wrong merchant: not found (no existence leak)
	if _, ok, _ := pg.GetInvoiceForMerchant(ctx, "t-inv-mt", "t-mer-OTHER"); ok {
		t.Fatal("cross-tenant read must be not-found")
	}
	// single-tenant invoice: not found for any merchant scope
	if _, ok, _ := pg.GetInvoiceForMerchant(ctx, "t-inv-st", "t-mer-1"); ok {
		t.Fatal("single-tenant invoice must not be visible to a merchant scope")
	}
	// and the single-tenant row reads back with empty tenancy fields via the base path
	if base, ok := pg.GetInvoice("t-inv-st"); !ok || base.MerchantID != "" || base.GatewayID != "" {
		t.Fatalf("single-tenant invoice tenancy fields must be empty: %+v ok=%v", base, ok)
	}

	list, err := pg.ListInvoicesByMerchant(ctx, "t-mer-1", 10, 0)
	if err != nil || len(list) != 1 || list[0].ID != "t-inv-mt" {
		t.Fatalf("ListInvoicesByMerchant: %+v err=%v", list, err)
	}
}
