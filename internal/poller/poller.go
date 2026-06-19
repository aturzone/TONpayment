// Package poller periodically re-checks pending invoices so callers don't have
// to poll: paid ones settle (and fire webhooks), overdue ones expire. The
// on-demand GET /status path still works too — both funnel through the same
// idempotent service.CheckStatus.
package poller

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/aturzone/TONpayment/internal/service"
	"github.com/aturzone/TONpayment/internal/store"
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

// tick re-checks every pending invoice with bounded concurrency.
func (p *Poller) tick(ctx context.Context) {
	pending := p.svc.ListPending()
	if len(pending) == 0 {
		return
	}
	sem := make(chan struct{}, p.concurrency)
	var wg sync.WaitGroup
	for _, inv := range pending {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(inv store.Invoice) {
			defer wg.Done()
			defer func() { <-sem }()
			if _, err := p.svc.CheckStatus(inv.ID); err != nil {
				log.Printf("poller: invoice %s: %v", inv.ID, err)
			}
		}(inv)
	}
	wg.Wait()
}
