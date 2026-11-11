// Package runner executes every scenario of a suite many times against a
// provider, in parallel, with retries and per-request timeouts.
package runner

import (
	"context"
	"errors"
	"hash/fnv"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/quentinpopp/toolgauge/internal/grader"
	"github.com/quentinpopp/toolgauge/internal/provider"
	"github.com/quentinpopp/toolgauge/internal/spec"
)

// Config controls a run.
type Config struct {
	Model string
	// Runs is how often each scenario is repeated. Models are not
	// deterministic, so one run per scenario says little.
	Runs        int
	Concurrency int
	// Timeout applies to each attempt, not the whole run.
	Timeout    time.Duration
	MaxRetries int
	// BaseBackoff is the delay before the first retry; it doubles each time
	// and gets up to 50% jitter.
	BaseBackoff time.Duration
	Seed        uint64
}

func (c *Config) defaults() {
	if c.Runs <= 0 {
		c.Runs = 1
	}
	if c.Concurrency <= 0 {
		c.Concurrency = 1
	}
	if c.Timeout <= 0 {
		c.Timeout = 60 * time.Second
	}
	if c.MaxRetries < 0 {
		c.MaxRetries = 0
	}
	if c.BaseBackoff <= 0 {
		c.BaseBackoff = 200 * time.Millisecond
	}
}

// Trial is the result of one scenario run once.
type Trial struct {
	Scenario  string              `json:"scenario"`
	Index     int                 `json:"index"`
	Outcome   grader.Outcome      `json:"outcome"`
	Detail    string              `json:"detail,omitempty"`
	ToolCalls []provider.ToolCall `json:"tool_calls,omitempty"`
	Attempts  int                 `json:"attempts"`
	Latency   time.Duration       `json:"latency_ns"`
	Usage     provider.Usage      `json:"usage"`
}

type job struct {
	sc    spec.Scenario
	index int
	slot  int
}

// Run executes the suite and returns one trial per scenario and repetition,
// ordered by scenario then index regardless of completion order. Provider
// errors become trials with outcome "error"; Run itself only fails if ctx is
// cancelled.
func Run(ctx context.Context, p provider.Provider, s *spec.Suite, cfg Config) ([]Trial, error) {
	cfg.defaults()
	trials := make([]Trial, len(s.Scenarios)*cfg.Runs)
	jobs := make(chan job)

	var wg sync.WaitGroup
	for range cfg.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				trials[j.slot] = runTrial(ctx, p, s, cfg, j)
			}
		}()
	}

feed:
	for i, sc := range s.Scenarios {
		for r := range cfg.Runs {
			select {
			case jobs <- job{sc: sc, index: r, slot: i*cfg.Runs + r}:
			case <-ctx.Done():
				break feed
			}
		}
	}
	close(jobs)
	wg.Wait()
	return trials, ctx.Err()
}

func runTrial(ctx context.Context, p provider.Provider, s *spec.Suite, cfg Config, j job) Trial {
	t := Trial{Scenario: j.sc.ID, Index: j.index}
	req := provider.Request{
		Model:  cfg.Model,
		System: s.System,
		Prompt: j.sc.Prompt,
		Tools:  s.Tools,
		Oracle: oracle(j.sc.Expect),
	}
	baseSeed := trialSeed(cfg.Seed, j.sc.ID, j.index)
	jitter := rand.New(rand.NewPCG(baseSeed, 1))

	start := time.Now()
	var resp provider.Response
	var err error
	for attempt := 0; ; attempt++ {
		t.Attempts = attempt + 1
		// A new seed per attempt, so a retry is a fresh sample.
		req.Seed = baseSeed + uint64(attempt)
		actx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		resp, err = p.Complete(actx, req)
		timedOut := errors.Is(actx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
		cancel()
		if err == nil || attempt >= cfg.MaxRetries || ctx.Err() != nil {
			break
		}
		if !provider.IsRetryable(err) && !timedOut {
			break
		}
		backoff := cfg.BaseBackoff << attempt
		backoff += time.Duration(jitter.Int64N(int64(backoff)/2 + 1))
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
		}
	}
	t.Latency = time.Since(start)
	if err != nil {
		t.Outcome, t.Detail = grader.Error, err.Error()
		return t
	}
	t.ToolCalls, t.Usage = resp.ToolCalls, resp.Usage
	t.Outcome, t.Detail = grader.Grade(j.sc.Expect, resp)
	return t
}

func oracle(e spec.Expectation) *provider.ToolCall {
	if e.NoTool {
		return nil
	}
	call := &provider.ToolCall{Name: e.Tool, Args: map[string]any{}}
	for k, m := range e.Args {
		call.Args[k] = m.Example()
	}
	return call
}

// trialSeed derives an independent seed for each scenario and repetition,
// so results do not depend on which worker ran a trial or in what order.
func trialSeed(seed uint64, scenario string, index int) uint64 {
	h := fnv.New64a()
	h.Write([]byte(scenario))
	return splitmix(splitmix(seed^h.Sum64()) + uint64(index))
}

func splitmix(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}
