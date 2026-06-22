//go:build loadtest

// Self-contained throughput probe for the HTTP/service hot paths. It is build-
// tagged so it never runs in CI or `go test ./...`; run it explicitly:
//
//	go test -tags loadtest -run TestLoad -v ./internal/httpx/
//
// It drives the REAL router + middleware + service against the in-memory store,
// using a distinct client IP per request (unique X-Forwarded-For, TrustProxy on) so
// the per-IP rate limiter does not cap aggregate throughput. This measures the
// application ceiling (no database); production adds Postgres, whose hot paths are
// single indexed point reads behind a now-properly-sized pool.
package httpx

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aturzone/TONpayment/internal/config"
	"github.com/aturzone/TONpayment/internal/service"
	"github.com/aturzone/TONpayment/internal/store"
	"github.com/aturzone/TONpayment/internal/wallet"
)

func TestLoad(t *testing.T) {
	// Silence the per-request access log: it isn't what we're measuring and its I/O
	// would both flood the output and depress the numbers.
	log.SetOutput(io.Discard)
	defer log.SetOutput(os.Stderr)

	mem, err := store.NewMemory("")
	if err != nil {
		t.Fatal(err)
	}
	// Mock verifier that never confirms (poller isn't started here); unbounded pending
	// so the create path isn't throttled by the pending cap during the run.
	svc := service.New(mem, wallet.NewMockVerifier(1<<30), "UQAfMHHa0mXA3EuvtkNw1FzNjULnthGnzrW15A_5HfNIiNru", 15*time.Minute, nil)
	svc.SetLimits(24*time.Hour, 0, 0)

	cfg := &config.Config{Env: "dev", TrustProxy: true, Addr: ":0"}
	srv := NewServer(Services{Cfg: cfg, Service: svc})
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	// Warm up the process (allocator, connection pool, JIT of hot paths) so the first
	// measured scenario isn't penalized by cold start.
	warm := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 64}}
	var wwg sync.WaitGroup
	warmDeadline := time.Now().Add(2 * time.Second)
	for w := 0; w < 32; w++ {
		wwg.Add(1)
		go func(id int) {
			defer wwg.Done()
			for time.Now().Before(warmDeadline) {
				req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/invoices", strings.NewReader(`{"amountNano":1000000000}`))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Forwarded-For", fmt.Sprintf("172.16.%d.%d", id, id))
				if resp, err := warm.Do(req); err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			}
		}(w)
	}
	wwg.Wait()

	t.Logf("scenario %38s %10s %8s %10s %6s %8s", "", "req/s", "reqs", "avg", "errs", "non-2xx")

	// Pure HTTP stack (router + middleware), no store, no rate limit.
	load(t, "GET /healthz (HTTP stack only)", 64, 6*time.Second, func(c *http.Client, _ int64) (int, error) {
		resp, err := c.Get(ts.URL + "/healthz")
		if err != nil {
			return 0, err
		}
		_, _ = io.Copy(io.Discard, resp.Body) // drain so the connection can be reused
		resp.Body.Close()
		return resp.StatusCode, nil
	})

	// Create path: decode + validate + memo alloc + store insert, across concurrency
	// levels to show how it scales (and where the knee is).
	for _, conc := range []int{16, 64, 128} {
		load(t, fmt.Sprintf("POST /v1/invoices (create, c=%d)", conc), conc, 6*time.Second, func(c *http.Client, i int64) (int, error) {
			req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/invoices", strings.NewReader(`{"amountNano":1000000000}`))
			req.Header.Set("Content-Type", "application/json")
			// Unique client IP per request -> fresh rate-limit bucket -> never throttled.
			req.Header.Set("X-Forwarded-For", fmt.Sprintf("10.%d.%d.%d", (i>>16)&255, (i>>8)&255, i&255))
			resp, err := c.Do(req)
			if err != nil {
				return 0, err
			}
			_, _ = io.Copy(io.Discard, resp.Body) // drain so the connection can be reused
			resp.Body.Close()
			return resp.StatusCode, nil
		})
	}
}

// load runs `do` from `conc` goroutines for `dur` and logs sustained throughput.
func load(t *testing.T, name string, conc int, dur time.Duration, do func(*http.Client, int64) (int, error)) {
	tr := &http.Transport{MaxIdleConns: conc * 2, MaxIdleConnsPerHost: conc * 2, IdleConnTimeout: 30 * time.Second}
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	defer tr.CloseIdleConnections()

	var ops, errs, non2xx, latNs, counter int64
	start := time.Now()
	deadline := start.Add(dur)
	var wg sync.WaitGroup
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				i := atomic.AddInt64(&counter, 1)
				t0 := time.Now()
				code, err := do(client, i)
				atomic.AddInt64(&latNs, time.Since(t0).Nanoseconds())
				atomic.AddInt64(&ops, 1)
				if err != nil {
					atomic.AddInt64(&errs, 1)
				} else if code < 200 || code >= 300 {
					atomic.AddInt64(&non2xx, 1)
				}
			}
		}()
	}
	wg.Wait()

	elapsed := time.Since(start)
	o := atomic.LoadInt64(&ops)
	var avg time.Duration
	if o > 0 {
		avg = time.Duration(latNs / o)
	}
	t.Logf("%-40s %10.0f %8d %10s %6d %8d", name, float64(o)/elapsed.Seconds(), o, avg.Round(time.Microsecond), errs, non2xx)
}
