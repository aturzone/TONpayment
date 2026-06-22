package wallet

import (
	"context"
	"sync"
	"time"
)

// evenLimiter paces calls to a steady maximum rate with NO burst: each wait()
// returns one fixed interval after the previous one. Even spacing — rather than a
// token bucket that lets a backlog fire in a burst — is exactly what an external
// API's per-second limit wants, because a burst is precisely what trips a 429. It
// is shared by the poller's batched reads and the on-demand status path so the
// whole process honours a single toncenter budget. rps <= 0 disables pacing.
type evenLimiter struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
}

func newEvenLimiter(rps float64) *evenLimiter {
	l := &evenLimiter{}
	if rps > 0 {
		l.interval = time.Duration(float64(time.Second) / rps)
	}
	return l
}

// wait blocks until this caller's slot in the schedule, or until ctx is done
// (returning ctx.Err()). With pacing disabled it only reflects ctx.
func (l *evenLimiter) wait(ctx context.Context) error {
	if l == nil || l.interval <= 0 {
		return ctx.Err()
	}
	l.mu.Lock()
	now := time.Now()
	if l.next.Before(now) {
		l.next = now // idle gap elapsed — the next slot is right now
	}
	at := l.next
	l.next = l.next.Add(l.interval)
	l.mu.Unlock()

	d := time.Until(at)
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
