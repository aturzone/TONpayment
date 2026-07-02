package telegrambot

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aturzone/TONpayment/internal/store"
)

// fakeAuthStore is an in-memory authStore for exercising the enrollment state
// machine and the fresh-TOTP gate without a database.
type fakeAuthStore struct {
	admins map[int64]*store.TelegramAdmin
	used   map[string]time.Time
	fail   bool // force every call to error (the fail-closed paths)
}

func newFakeAuthStore() *fakeAuthStore {
	return &fakeAuthStore{admins: map[int64]*store.TelegramAdmin{}, used: map[string]time.Time{}}
}

var errBoom = &fakeErr{"boom"}

type fakeErr struct{ s string }

func (e *fakeErr) Error() string { return e.s }

func (f *fakeAuthStore) GetTelegramAdmin(_ context.Context, id int64) (store.TelegramAdmin, bool, error) {
	if f.fail {
		return store.TelegramAdmin{}, false, errBoom
	}
	a, ok := f.admins[id]
	if !ok {
		return store.TelegramAdmin{}, false, nil
	}
	return *a, true, nil
}

func (f *fakeAuthStore) UpsertStagedTelegramAdmin(_ context.Context, id int64, phone, enc string) error {
	if f.fail {
		return errBoom
	}
	f.admins[id] = &store.TelegramAdmin{TelegramUserID: id, Phone: phone, TOTPSecretEnc: enc, UpdatedAt: time.Now()}
	return nil
}

func (f *fakeAuthStore) MarkTelegramAdminEnrolled(_ context.Context, id int64) error {
	if f.fail {
		return errBoom
	}
	f.admins[id].Enrolled = true
	return nil
}

func (f *fakeAuthStore) RecordTelegramTOTPFailure(_ context.Context, id int64, attempts int, lock time.Time) error {
	if f.fail {
		return errBoom
	}
	a := f.admins[id]
	a.FailedTOTPAttempts = attempts
	if !lock.IsZero() {
		a.TOTPLockedUntil = lock
	}
	return nil
}

func (f *fakeAuthStore) ResetTelegramTOTPFailures(_ context.Context, id int64) error {
	if f.fail {
		return errBoom
	}
	a := f.admins[id]
	a.FailedTOTPAttempts = 0
	a.TOTPLockedUntil = time.Time{}
	return nil
}

func (f *fakeAuthStore) ClaimTelegramTOTPCode(_ context.Context, id int64, code string, _ time.Duration) (bool, error) {
	if f.fail {
		return false, errBoom
	}
	key := string(rune(id)) + ":" + code
	if _, seen := f.used[key]; seen {
		return false, nil
	}
	f.used[key] = time.Now()
	return true, nil
}

func newTestAuth(t *testing.T, st authStore) *authService {
	t.Helper()
	cipher, err := newSecretCipher("unit-test-secret")
	if err != nil {
		t.Fatal(err)
	}
	return newAuthService(st, cipher, []string{"+989154869715"}, nil)
}

// currentCode computes the valid TOTP for a staged admin's secret.
func currentCode(t *testing.T, a *authService, st *fakeAuthStore, id int64) string {
	t.Helper()
	secret, err := a.cipher.decrypt(st.admins[id].TOTPSecretEnc)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := b32.DecodeString(secret)
	return hotp(key, uint64(time.Now().Unix())/30)
}

func TestEnrollmentHappyPath(t *testing.T) {
	st := newFakeAuthStore()
	a := newTestAuth(t, st)
	ctx := context.Background()
	const uid = 42

	// A stranger's first text → share-phone prompt.
	r := a.HandleDM(ctx, uid, "/start", "", 0)
	if !r.requestContact || r.text != replySharePhone {
		t.Fatalf("expected share-phone prompt, got %+v", r)
	}

	// Sharing their OWN allow-listed number stages the secret.
	r = a.HandleDM(ctx, uid, "", "+989154869715", uid)
	if !strings.Contains(r.text, "2FA secret") {
		t.Fatalf("expected enroll prompt, got %q", r.text)
	}
	if a.IsEnrolledAdmin(ctx, uid) {
		t.Fatal("must NOT be enrolled before code confirmation")
	}

	// The correct 6-digit code completes enrollment.
	r = a.HandleDM(ctx, uid, currentCode(t, a, st, uid), "", 0)
	if r.text != replyEnrolled {
		t.Fatalf("expected enrolled reply, got %q", r.text)
	}
	if !a.IsEnrolledAdmin(ctx, uid) {
		t.Fatal("must be enrolled after confirmation")
	}
}

func TestEnrollmentRejectsForwardedContactAndForeignPhone(t *testing.T) {
	st := newFakeAuthStore()
	a := newTestAuth(t, st)
	ctx := context.Background()

	// A FORWARDED contact (user_id != sender) with the allow-listed number.
	r := a.HandleDM(ctx, 7, "", "+989154869715", 999)
	if r.text != replyNotAuthorized {
		t.Fatalf("forwarded contact must be rejected, got %q", r.text)
	}
	// Their OWN contact but a non-allow-listed number.
	r = a.HandleDM(ctx, 7, "", "+15551234567", 7)
	if r.text != replyNotAuthorized {
		t.Fatalf("foreign phone must be rejected, got %q", r.text)
	}
	if len(st.admins) != 0 {
		t.Fatal("nothing may be staged on a rejected bind")
	}
	if a.IsEnrolledAdmin(ctx, 7) {
		t.Fatal("rejected user must not be admin")
	}
}

func TestFreshTOTPGateAndReplay(t *testing.T) {
	st := newFakeAuthStore()
	a := newTestAuth(t, st)
	ctx := context.Background()
	const uid = 42
	a.HandleDM(ctx, uid, "", "+989154869715", uid)
	enrollCode := currentCode(t, a, st, uid)
	a.HandleDM(ctx, uid, enrollCode, "", 0) // enroll (consumes enrollCode)

	// The exact code that finished enrollment was CONSUMED by enrollment and
	// must not authorize a sensitive command.
	if a.VerifyFreshTOTP(ctx, uid, enrollCode) {
		t.Fatal("the enrollment code must be consumed by enrollment")
	}

	// A different, still-valid code (the next 30s step — inside CheckTOTP's ±1
	// window) passes once, then its replay is rejected.
	secret, err := a.cipher.decrypt(st.admins[uid].TOTPSecretEnc)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := b32.DecodeString(secret)
	next := hotp(key, uint64(time.Now().Add(totpStep).Unix())/30)
	if next == enrollCode {
		t.Skip("adjacent TOTP steps collided (1-in-a-million); rerun")
	}
	if !a.VerifyFreshTOTP(ctx, uid, next) {
		t.Fatal("first use of a valid fresh code must pass")
	}
	if a.VerifyFreshTOTP(ctx, uid, next) {
		t.Fatal("REPLAYED code must be rejected")
	}
	if a.VerifyFreshTOTP(ctx, uid, "000000") {
		t.Fatal("wrong code must be rejected")
	}
	if a.VerifyFreshTOTP(ctx, 999, next) {
		t.Fatal("non-enrolled user must be rejected")
	}
}

func TestLockoutAfterRepeatedBadCodes(t *testing.T) {
	st := newFakeAuthStore()
	a := newTestAuth(t, st)
	ctx := context.Background()
	const uid = 42
	a.HandleDM(ctx, uid, "", "+989154869715", uid)
	code := currentCode(t, a, st, uid)
	a.HandleDM(ctx, uid, code, "", 0) // enroll

	for i := 0; i < a.lockoutThreshold; i++ {
		if a.VerifyFreshTOTP(ctx, uid, "000000") {
			t.Fatal("bad code accepted")
		}
	}
	if st.admins[uid].TOTPLockedUntil.IsZero() {
		t.Fatal("lockout must be armed after threshold bad codes")
	}
	// Even the CORRECT code is rejected while locked (no oracle).
	if a.VerifyFreshTOTP(ctx, uid, currentCode(t, a, st, uid)) {
		t.Fatal("locked admin must be rejected even with a valid code")
	}
}

func TestFailClosedOnStoreErrors(t *testing.T) {
	st := newFakeAuthStore()
	st.fail = true
	a := newTestAuth(t, st)
	ctx := context.Background()

	if a.IsEnrolledAdmin(ctx, 42) {
		t.Fatal("store error must read as NOT authorized")
	}
	if a.VerifyFreshTOTP(ctx, 42, "123456") {
		t.Fatal("store error must read as NOT verified")
	}
	r := a.HandleDM(ctx, 42, "hi", "", 0)
	if r.text != replyNotAuthorized {
		t.Fatalf("DM on store error must reply neutrally, got %q", r.text)
	}
}

func TestEnrolledUserGroupGateOnly(t *testing.T) {
	// IsEnrolledAdmin is keyed strictly on the numeric id.
	st := newFakeAuthStore()
	a := newTestAuth(t, st)
	if a.IsEnrolledAdmin(context.Background(), 0) {
		t.Fatal("id 0 must never be authorized")
	}
}
