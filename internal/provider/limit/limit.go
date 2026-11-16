// Package limit keeps a run within a provider's rate limit and a request
// budget. It wraps the provider directly, inside any cache, so that cached
// answers are neither slowed down nor counted.
package limit

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quentinpopp/toolgauge/internal/provider"
)

// Provider wraps another provider with pacing and a budget.
type Provider struct {
	inner    provider.Provider
	interval time.Duration // 0 means no pacing
	budget   int64         // 0 means no budget
	sent     atomic.Int64

	mu        sync.Mutex
	nextStart time.Time // earliest start of the next request
}

// Wrap returns p limited to perMinute requests per minute (0 for no limit)
// and at most budget requests in total (0 for no limit). Retries count
// against both, because the API counts them too.
func Wrap(p provider.Provider, perMinute float64, budget int) *Provider {
	l := &Provider{inner: p, budget: int64(budget)}
	if perMinute > 0 {
		l.interval = time.Duration(float64(time.Minute) / perMinute)
	}
	return l
}

// Name implements provider.Provider.
func (l *Provider) Name() string { return l.inner.Name() }

// Sent returns the number of requests passed to the wrapped provider.
func (l *Provider) Sent() int64 { return l.sent.Load() }

// Complete implements provider.Provider.
func (l *Provider) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	if err := l.wait(ctx); err != nil {
		return provider.Response{}, err
	}
	if n := l.sent.Add(1); l.budget > 0 && n > l.budget {
		l.sent.Add(-1)
		return provider.Response{}, provider.ErrBudgetExhausted
	}
	return l.inner.Complete(ctx, req)
}

// wait blocks until this request may start. Requests are spaced evenly
// instead of sent in bursts, which keeps clear of per-minute limits that
// providers measure over sliding windows.
func (l *Provider) wait(ctx context.Context) error {
	if l.interval == 0 {
		return nil
	}
	l.mu.Lock()
	start := time.Now()
	if l.nextStart.After(start) {
		start = l.nextStart
	}
	l.nextStart = start.Add(l.interval)
	l.mu.Unlock()

	d := time.Until(start)
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
