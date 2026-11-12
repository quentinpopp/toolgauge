package limit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/quentinpopp/toolgauge/internal/provider"
)

type recorder struct {
	mu    sync.Mutex
	times []time.Time
}

func (r *recorder) Name() string { return "rec" }

func (r *recorder) Complete(context.Context, provider.Request) (provider.Response, error) {
	r.mu.Lock()
	r.times = append(r.times, time.Now())
	r.mu.Unlock()
	return provider.Response{}, nil
}

func TestPacesRequestsEvenly(t *testing.T) {
	rec := &recorder{}
	l := Wrap(rec, 600, 0) // one request every 100ms
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() { defer wg.Done(); l.Complete(context.Background(), provider.Request{}) }()
	}
	wg.Wait()
	first, last := rec.times[0], rec.times[0]
	for _, ts := range rec.times {
		if ts.Before(first) {
			first = ts
		}
		if ts.After(last) {
			last = ts
		}
	}
	// 5 requests at 100ms spacing span at least 400ms, even when sent at once.
	if span := last.Sub(first); span < 380*time.Millisecond {
		t.Fatalf("5 requests took %v, want them spread over about 400ms", span)
	}
}

func TestBudget(t *testing.T) {
	rec := &recorder{}
	l := Wrap(rec, 0, 3)
	var errs int
	for range 5 {
		if _, err := l.Complete(context.Background(), provider.Request{}); errors.Is(err, provider.ErrBudgetExhausted) {
			errs++
		}
	}
	if len(rec.times) != 3 || errs != 2 || l.Sent() != 3 {
		t.Fatalf("sent %d, refused %d, Sent()=%d; want 3, 2, 3", len(rec.times), errs, l.Sent())
	}
}

func TestWaitRespectsCancel(t *testing.T) {
	l := Wrap(&recorder{}, 1, 0) // one per minute
	l.Complete(context.Background(), provider.Request{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := l.Complete(ctx, provider.Request{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline error, got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("waiting for a slot must stop when the context ends")
	}
}

func TestNoLimits(t *testing.T) {
	rec := &recorder{}
	l := Wrap(rec, 0, 0)
	start := time.Now()
	for range 100 {
		l.Complete(context.Background(), provider.Request{})
	}
	if time.Since(start) > 100*time.Millisecond || l.Sent() != 100 {
		t.Fatal("without limits requests must pass straight through")
	}
}
