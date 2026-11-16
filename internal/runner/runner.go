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
	// up to MaxBackoff and gets up to 50% jitter on top.
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	Seed        uint64
}

// Validate rejects settings that cannot produce a meaningful run. Zero values
// for Concurrency, Timeout and the backoffs mean "use the default".
func (c Config) Validate() error {
	switch {
	case c.Runs < 1:
		return errors.New("runs must be at least 1")
	case c.Concurrency < 0:
		return errors.New("concurrency must not be negative")
	case c.MaxRetries < 0:
		return errors.New("retries must not be negative")
	case c.Timeout < 0 || c.BaseBackoff < 0 || c.MaxBackoff < 0:
		return errors.New("durations must not be negative")
	}
	return nil
}

func (c *Config) defaults() {
	if c.Concurrency == 0 {
		c.Concurrency = 1
	}
	if c.Timeout == 0 {
		c.Timeout = 60 * time.Second
	}
	if c.BaseBackoff == 0 {
		c.BaseBackoff = 200 * time.Millisecond
	}
	if c.MaxBackoff == 0 {
		c.MaxBackoff = 30 * time.Second
	}
	c.MaxBackoff = max(c.MaxBackoff, c.BaseBackoff)
}

// backoff returns the wait before retry number attempt+1: BaseBackoff doubled
// per attempt, capped at MaxBackoff, plus up to 50% jitter. Doubling stops at
// the cap, so a high retry count cannot overflow.
func (c Config) backoff(attempt int, jitter *rand.Rand) time.Duration {
	d := c.BaseBackoff
	for range attempt {
		if d >= c.MaxBackoff/2 {
			d = c.MaxBackoff
			break
		}
		d *= 2
	}
	return d + time.Duration(jitter.Int64N(int64(d)/2+1))
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
// errors become trials with outcome "error".
//
// If ctx is cancelled, or the provider reports provider.ErrBudgetExhausted,
// Run stops sending requests and returns the trials that finished before,
// together with the reason, so the caller can still save what was paid for.
func Run(parent context.Context, p provider.Provider, s *spec.Suite, cfg Config) ([]Trial, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg.defaults()
	ctx, stop := context.WithCancelCause(parent)
	defer stop(nil)
	trials := make([]Trial, len(s.Scenarios)*cfg.Runs)
	done := make([]bool, len(trials))
	jobs := make(chan job)

	var wg sync.WaitGroup
	for range cfg.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				trials[j.slot], done[j.slot] = runTrial(ctx, stop, p, s, cfg, j)
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
	if ctx.Err() != nil {
		finished := make([]Trial, 0, len(trials))
		for i, t := range trials {
			if done[i] {
				finished = append(finished, t)
			}
		}
		return finished, context.Cause(ctx)
	}
	return trials, nil
}

// runTrial runs one trial. The bool is false if the run was stopped before
// the trial reached a result of its own, so it must not be counted.
func runTrial(ctx context.Context, stop context.CancelCauseFunc, p provider.Provider, s *spec.Suite, cfg Config, j job) (Trial, bool) {
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
		if errors.Is(err, provider.ErrBudgetExhausted) {
			// Out of budget is not the model's fault: stop the whole run
			// rather than record an error trial.
			stop(err)
			return t, false
		}
		if ctx.Err() != nil {
			// Whatever came back was cut short by the cancellation.
			return t, false
		}
		if err == nil || attempt >= cfg.MaxRetries {
			break
		}
		if !provider.IsRetryable(err) && !timedOut {
			break
		}
		// Honour the server's Retry-After if it asks for a longer pause.
		wait := time.NewTimer(max(cfg.backoff(attempt, jitter), provider.RetryAfter(err)))
		select {
		case <-wait.C:
		case <-ctx.Done():
			wait.Stop()
			return t, false
		}
	}
	t.Latency = time.Since(start)
	if err != nil {
		t.Outcome, t.Detail = grader.Error, err.Error()
		return t, true
	}
	t.ToolCalls, t.Usage = resp.ToolCalls, resp.Usage
	t.Outcome, t.Detail = grader.Grade(j.sc.Expect, resp)
	return t, true
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
