// Package provider is the boundary between toolgauge and a model API.
package provider

import (
	"context"
	"errors"
	"time"

	"github.com/quentinpopp/toolgauge/internal/spec"
)

// Provider sends one request to a model and returns its answer.
// Implementations must be safe for concurrent use.
type Provider interface {
	Name() string
	Complete(ctx context.Context, req Request) (Response, error)
}

// Request is a single-turn chat request with tools.
type Request struct {
	Model  string
	System string
	Prompt string
	Tools  []spec.Tool
	// Seed is derived from the run seed, scenario and trial, so a provider
	// that supports seeding can make each trial reproducible.
	Seed uint64
	// Oracle is the call a perfect model would make, or nil if it would not
	// call a tool. Only test doubles such as the mock provider may read it;
	// real providers must ignore it.
	Oracle *ToolCall
}

// Response is what the model returned.
type Response struct {
	Text      string
	ToolCalls []ToolCall
	Usage     Usage
}

// ToolCall is one call the model asked for.
type ToolCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

// Usage holds token counts as reported by the provider.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// ErrBudgetExhausted is returned when a run has used up its request budget.
// The runner stops the run when it sees it, like a cancellation.
var ErrBudgetExhausted = errors.New("request budget exhausted")

// retryable marks errors worth retrying: rate limits, timeouts, 5xx.
type retryable struct {
	err   error
	after time.Duration
}

func (r retryable) Error() string { return r.err.Error() }
func (r retryable) Unwrap() error { return r.err }

// Retryable wraps err so the runner retries the request.
func Retryable(err error) error { return retryable{err: err} }

// RetryableAfter is Retryable with a minimum wait, typically from a
// Retry-After header. The runner waits at least this long before retrying.
func RetryableAfter(err error, after time.Duration) error {
	return retryable{err: err, after: after}
}

// IsRetryable reports whether err, or an error it wraps, was marked retryable.
func IsRetryable(err error) bool {
	var r retryable
	return errors.As(err, &r)
}

// RetryAfter returns the minimum wait attached with RetryableAfter, or 0.
func RetryAfter(err error) time.Duration {
	var r retryable
	if errors.As(err, &r) {
		return r.after
	}
	return 0
}
