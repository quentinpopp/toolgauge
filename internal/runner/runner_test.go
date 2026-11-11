package runner

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quentinpopp/toolgauge/internal/grader"
	"github.com/quentinpopp/toolgauge/internal/provider"
	"github.com/quentinpopp/toolgauge/internal/spec"
)

// fake is a scriptable provider for runner tests.
type fake struct {
	fn       func(ctx context.Context, req provider.Request) (provider.Response, error)
	inFlight atomic.Int32
	peak     atomic.Int32
	calls    atomic.Int32
}

func (f *fake) Name() string { return "fake" }

func (f *fake) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	f.calls.Add(1)
	n := f.inFlight.Add(1)
	defer f.inFlight.Add(-1)
	for {
		p := f.peak.Load()
		if n <= p || f.peak.CompareAndSwap(p, n) {
			break
		}
	}
	return f.fn(ctx, req)
}

func suite(t *testing.T) *spec.Suite {
	t.Helper()
	s, err := spec.Parse([]byte(`
name: r
tools: [{name: close_issue, description: Close an issue.}]
scenarios:
  - {id: a, prompt: close 1, expect: {tool: close_issue}}
  - {id: b, prompt: hello, expect: {no_tool: true}}
`))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func perfect(_ context.Context, req provider.Request) (provider.Response, error) {
	if req.Oracle == nil {
		return provider.Response{Text: "hi"}, nil
	}
	return provider.Response{ToolCalls: []provider.ToolCall{*req.Oracle}}, nil
}

func TestRunOrderAndConcurrency(t *testing.T) {
	f := &fake{fn: func(ctx context.Context, req provider.Request) (provider.Response, error) {
		time.Sleep(5 * time.Millisecond)
		return perfect(ctx, req)
	}}
	trials, err := Run(context.Background(), f, suite(t), Config{Runs: 10, Concurrency: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(trials) != 20 {
		t.Fatalf("got %d trials, want 20", len(trials))
	}
	for i, tr := range trials {
		wantID := []string{"a", "b"}[i/10]
		if tr.Scenario != wantID || tr.Index != i%10 || tr.Outcome != grader.Pass {
			t.Fatalf("trial %d = %+v", i, tr)
		}
	}
	if p := f.peak.Load(); p > 4 || p < 2 {
		t.Errorf("peak concurrency %d, want between 2 and 4", p)
	}
}

func TestRetriesRetryableErrors(t *testing.T) {
	var mu sync.Mutex
	seen := map[uint64]bool{}
	f := &fake{fn: func(ctx context.Context, req provider.Request) (provider.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		if !seen[req.Seed] && len(seen) < 2 {
			seen[req.Seed] = true
			return provider.Response{}, provider.Retryable(errors.New("429"))
		}
		return perfect(ctx, req)
	}}
	s := suite(t)
	s.Scenarios = s.Scenarios[:1]
	trials, _ := Run(context.Background(), f, s, Config{Runs: 1, MaxRetries: 3, BaseBackoff: time.Millisecond})
	if trials[0].Outcome != grader.Pass || trials[0].Attempts != 3 {
		t.Fatalf("want pass after 3 attempts, got %+v", trials[0])
	}
}

func TestDoesNotRetryPermanentErrors(t *testing.T) {
	f := &fake{fn: func(context.Context, provider.Request) (provider.Response, error) {
		return provider.Response{}, errors.New("400 bad request")
	}}
	trials, _ := Run(context.Background(), f, suite(t), Config{Runs: 1, MaxRetries: 5, BaseBackoff: time.Millisecond})
	if trials[0].Outcome != grader.Error || trials[0].Attempts != 1 {
		t.Fatalf("permanent error should fail at once, got %+v", trials[0])
	}
}

func TestRetriesTimeouts(t *testing.T) {
	var n atomic.Int32
	f := &fake{fn: func(ctx context.Context, req provider.Request) (provider.Response, error) {
		if n.Add(1) == 1 {
			<-ctx.Done() // hang until the per-attempt timeout fires
			return provider.Response{}, ctx.Err()
		}
		return perfect(ctx, req)
	}}
	s := suite(t)
	s.Scenarios = s.Scenarios[:1]
	trials, _ := Run(context.Background(), f, s, Config{
		Runs: 1, MaxRetries: 1, Timeout: 20 * time.Millisecond, BaseBackoff: time.Millisecond,
	})
	if trials[0].Outcome != grader.Pass || trials[0].Attempts != 2 {
		t.Fatalf("want pass on second attempt after timeout, got %+v", trials[0])
	}
}

func TestCancelStopsRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fake{fn: func(ctx context.Context, req provider.Request) (provider.Response, error) {
		cancel()
		<-ctx.Done()
		return provider.Response{}, ctx.Err()
	}}
	done := make(chan struct{})
	go func() {
		_, err := Run(ctx, f, suite(t), Config{Runs: 1000, Concurrency: 2})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("want context.Canceled, got %v", err)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
	if c := f.calls.Load(); c > 10 {
		t.Errorf("%d requests after cancel, want only the in-flight ones", c)
	}
}

func TestSeedsAreStableAndDistinct(t *testing.T) {
	a := trialSeed(1, "x", 0)
	if a != trialSeed(1, "x", 0) {
		t.Fatal("seed must be deterministic")
	}
	for _, b := range []uint64{trialSeed(2, "x", 0), trialSeed(1, "y", 0), trialSeed(1, "x", 1)} {
		if a == b {
			t.Fatal("different inputs gave the same seed")
		}
	}
}
