// Package poller periodically re-checks pending invoices so callers don't have
// to poll: paid ones settle (and fire webhooks), overdue ones expire. The
// on-demand GET /status path still works too — both funnel through the same
// idempotent service.CheckStatus.
package poller

import (
	"context"
	"log"
	"time"

	"github.com/aturzone/TONpayment/internal/service"
)

type Poller struct {
	svc         *service.Service
	interval    time.Duration
	concurrency int
}

func New(svc *service.Service, interval time.Duration, concurrency int) *Poller {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	if concurrency < 1 {
		concurrency = 4
	}
	return &Poller{svc: svc, interval: interval, concurrency: concurrency}
}

// Run polls until ctx is cancelled.
func (p *Poller) Run(ctx context.Context) {
	t := time.NewTicker(p.interval)
	defer t.Stop()
	log.Printf("poller: every %s, concurrency %d", p.interval, p.concurrency)
	for {
		select {
		case <-ctx.Done():
			log.Printf("poller: stopped")
			return
		case <-t.C:
			p.tick(ctx)
		}
	}
}

// tick re-checks every pending invoice. The service batches the upstream reads by
// receiving address and paces them against the toncenter rate budget, so one tick
// costs O(distinct addresses) requests, not O(pending invoices); concurrency bounds
// how many addresses are settled in parallel.
func (p *Poller) tick(ctx context.Context) {
	p.svc.CheckPending(ctx, p.concurrency)
}
