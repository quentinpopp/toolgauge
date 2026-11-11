// Package mock is a stand-in model for running toolgauge without an API key.
//
// It is a toy, not a language model, but it behaves like one in the ways that
// matter for testing the harness:
//
//   - It picks a tool by word overlap between the prompt and each tool's name
//     and description, then samples from a softmax over those scores. Clear
//     descriptions win reliably, vague ones lead to wrong picks, so changing a
//     description changes results the way it would with a real model.
//   - Its output is random but reproducible: the same seed gives the same
//     answer.
//   - It can get arguments wrong and fail with retryable errors at
//     configurable rates.
//
// When it picks the right tool it fills in arguments from Request.Oracle,
// which real providers never see.
package mock

import (
	"context"
	"errors"
	"maps"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/quentinpopp/toolgauge/internal/provider"
	"github.com/quentinpopp/toolgauge/internal/spec"
)

// Config controls how the mock behaves.
type Config struct {
	// Temperature of the softmax over tool scores. Lower is more decisive.
	Temperature float64
	// NoToolScore is the score of answering without a tool. Tools that share
	// fewer words than this with the prompt usually lose to it.
	NoToolScore float64
	// ArgErrorRate is the chance that each argument comes back wrong.
	ArgErrorRate float64
	// FailureRate is the chance that a request fails with a retryable error.
	FailureRate float64
	// Latency is added to every request to imitate network time.
	Latency time.Duration
}

// DefaultConfig gives a mock that is good but not perfect.
func DefaultConfig() Config {
	return Config{Temperature: 0.3, NoToolScore: 1.0, ArgErrorRate: 0.03}
}

// Provider is the mock provider.
type Provider struct {
	cfg Config
	// toolWords caches the word set of each tool, keyed by its full text, so
	// thousands of trials over the same tools tokenize them only once.
	toolWords sync.Map // string -> map[string]bool
}

func (p *Provider) wordsOf(t spec.Tool) map[string]bool {
	text := toolText(t)
	if w, ok := p.toolWords.Load(text); ok {
		return w.(map[string]bool)
	}
	w := stems(text)
	p.toolWords.Store(text, w)
	return w
}

// New returns a mock provider.
func New(cfg Config) *Provider {
	if cfg.Temperature <= 0 {
		cfg.Temperature = DefaultConfig().Temperature
	}
	return &Provider{cfg: cfg}
}

// Name implements provider.Provider.
func (p *Provider) Name() string { return "mock" }

// ErrTransient is returned for simulated failures.
var ErrTransient = errors.New("mock: simulated transient failure")

// Complete implements provider.Provider.
func (p *Provider) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	if p.cfg.Latency > 0 {
		t := time.NewTimer(p.cfg.Latency)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return provider.Response{}, ctx.Err()
		case <-t.C:
		}
	}
	rng := rand.New(rand.NewPCG(req.Seed, 0x9e3779b97f4a7c15))
	if rng.Float64() < p.cfg.FailureRate {
		return provider.Response{}, provider.Retryable(ErrTransient)
	}

	// Score every tool, with index len(tools) standing for "no tool".
	words := stems(req.Prompt)
	scores := make([]float64, len(req.Tools)+1)
	for i, t := range req.Tools {
		scores[i] = overlap(words, p.wordsOf(t))
	}
	scores[len(req.Tools)] = p.cfg.NoToolScore
	choice := sample(rng, scores, p.cfg.Temperature)

	usage := provider.Usage{InputTokens: len(strings.Fields(req.Prompt)) * 4 / 3, OutputTokens: 20}
	if choice == len(req.Tools) {
		return provider.Response{Text: "I can answer that without a tool.", Usage: usage}, nil
	}
	call := provider.ToolCall{Name: req.Tools[choice].Name, Args: map[string]any{}}
	if req.Oracle != nil && req.Oracle.Name == call.Name {
		// Sorted: map order is random in Go, and drawing random numbers in a
		// random order would make the same seed give different answers.
		for _, k := range slices.Sorted(maps.Keys(req.Oracle.Args)) {
			v := req.Oracle.Args[k]
			if rng.Float64() < p.cfg.ArgErrorRate {
				if rng.IntN(2) == 0 {
					continue // forget the argument
				}
				v = corrupt(v)
			}
			call.Args[k] = v
		}
	}
	return provider.Response{ToolCalls: []provider.ToolCall{call}, Usage: usage}, nil
}

// toolText is everything a model sees about a tool: its name, description and
// the names and descriptions of its parameters.
func toolText(t spec.Tool) string {
	var b strings.Builder
	b.WriteString(strings.ReplaceAll(t.Name, "_", " "))
	b.WriteString(" " + t.Description)
	props, _ := t.Parameters["properties"].(map[string]any)
	// Sorted, so the same tool always gives the same text (it is a cache key).
	for _, name := range slices.Sorted(maps.Keys(props)) {
		b.WriteString(" " + strings.ReplaceAll(name, "_", " "))
		if m, ok := props[name].(map[string]any); ok {
			if d, ok := m["description"].(string); ok {
				b.WriteString(" " + d)
			}
		}
	}
	return b.String()
}

func corrupt(v any) any {
	switch x := v.(type) {
	case string:
		return x + "-typo"
	case float64:
		return x + 1
	case bool:
		return !x
	default:
		return nil
	}
}

func sample(rng *rand.Rand, scores []float64, temp float64) int {
	max := math.Inf(-1)
	for _, s := range scores {
		max = math.Max(max, s)
	}
	weights := make([]float64, len(scores))
	var total float64
	for i, s := range scores {
		weights[i] = math.Exp((s - max) / temp)
		total += weights[i]
	}
	r := rng.Float64() * total
	for i, w := range weights {
		if r < w {
			return i
		}
		r -= w
	}
	return len(scores) - 1
}

func overlap(a, b map[string]bool) float64 {
	n := 0
	for w := range a {
		if b[w] {
			n++
		}
	}
	return float64(n)
}

var stopwords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a an and are as at be by can for from has have i in is it
		its me my of on or our please that the this to us was we what when which will with you your
		all any some new one`) {
		stopwords[w] = true
	}
}

// stems splits text into lowercase words, drops stopwords and strips common
// English suffixes, so "issues" matches "issue" and "closing" matches "close".
func stems(text string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if stopwords[w] || len(w) < 2 {
			continue
		}
		out[stem(w)] = true
	}
	return out
}

func stem(w string) string {
	switch {
	case len(w) > 4 && strings.HasSuffix(w, "ies"):
		w = w[:len(w)-3] + "y"
	case len(w) > 5 && strings.HasSuffix(w, "ing"):
		w = w[:len(w)-3]
	case len(w) > 4 && strings.HasSuffix(w, "ed"):
		w = w[:len(w)-2]
	case len(w) > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss"):
		w = w[:len(w)-1]
	}
	if len(w) > 3 && strings.HasSuffix(w, "e") {
		w = w[:len(w)-1]
	}
	return w
}
