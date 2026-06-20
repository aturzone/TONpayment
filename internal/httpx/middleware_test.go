package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRateLimiterAllowsBurstThenBlocks(t *testing.T) {
	rl := newRateLimiter(1, 3, false) // 1 token/s, burst 3
	allowed := 0
	for i := 0; i < 5; i++ {
		if rl.allow("1.2.3.4") {
			allowed++
		}
	}
	if allowed != 3 {
		t.Fatalf("allowed %d of 5 rapid requests, want 3 (burst)", allowed)
	}
	if !rl.allow("5.6.7.8") {
		t.Fatal("a different client key should have its own bucket")
	}
}

func TestClientIPHonorsTrustProxy(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "1.1.1.1, 2.2.2.2")
	if got := clientIP(r, false); got != "10.0.0.1" {
		t.Fatalf("trustProxy=false: got %q, want 10.0.0.1 (connection remote addr)", got)
	}
	if got := clientIP(r, true); got != "1.1.1.1" {
		t.Fatalf("trustProxy=true: got %q, want 1.1.1.1 (first X-Forwarded-For)", got)
	}
}

func TestCORSOnlyEchoesAllowedOrigins(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := cors([]string{"https://app.example"})(ok)

	// allowed origin is echoed
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://app.example")
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example" {
		t.Fatalf("allowed origin: ACAO = %q", got)
	}

	// unknown origin gets no ACAO header
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.Header.Set("Origin", "https://evil.example")
	h.ServeHTTP(rec2, req2)
	if got := rec2.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("disallowed origin should get no ACAO header, got %q", got)
	}

	// preflight short-circuits with 204
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodOptions, "/", nil)
	req3.Header.Set("Origin", "https://app.example")
	h.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusNoContent {
		t.Fatalf("OPTIONS preflight code = %d, want 204", rec3.Code)
	}
}
