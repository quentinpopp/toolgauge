package mock

import (
	"context"
	"reflect"
	"testing"

	"github.com/quentinpopp/toolgauge/internal/provider"
	"github.com/quentinpopp/toolgauge/internal/spec"
)

var tools = []spec.Tool{
	{Name: "create_issue", Description: "Create a new issue to report a bug or request a feature."},
	{Name: "close_issue", Description: "Close an issue that is fixed or a duplicate."},
}

func pickRate(t *testing.T, p *Provider, tools []spec.Tool, prompt, want string) float64 {
	t.Helper()
	hits := 0
	const n = 500
	for i := range n {
		resp, err := p.Complete(context.Background(), provider.Request{Prompt: prompt, Tools: tools, Seed: uint64(i)})
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.ToolCalls) > 0 && resp.ToolCalls[0].Name == want {
			hits++
		}
	}
	return float64(hits) / n
}

func TestClearDescriptionsBeatVagueOnes(t *testing.T) {
	p := New(DefaultConfig())
	prompt := "Please report a bug: the login page is broken"
	clear := pickRate(t, p, tools, prompt, "create_issue")
	vague := pickRate(t, p, []spec.Tool{
		{Name: "create_issue", Description: "Handles things."},
		{Name: "close_issue", Description: "Other things."},
	}, prompt, "create_issue")
	if clear < 0.8 {
		t.Errorf("clear description picked only %.0f%% of the time", clear*100)
	}
	if vague > clear-0.3 {
		t.Errorf("vague descriptions should do much worse: clear %.2f, vague %.2f", clear, vague)
	}
}

func TestDeterministic(t *testing.T) {
	// Many arguments and a high error rate, so any dependence on map order
	// shows up as different arguments for the same seed.
	p := New(Config{Temperature: 1, ArgErrorRate: 0.5})
	args := map[string]any{}
	for _, k := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		args[k] = k
	}
	for seed := range uint64(30) {
		req := provider.Request{
			Prompt: "close the duplicate issue", Tools: tools, Seed: seed,
			Oracle: &provider.ToolCall{Name: "close_issue", Args: args},
		}
		first, _ := p.Complete(context.Background(), req)
		for range 20 {
			if got, _ := p.Complete(context.Background(), req); !reflect.DeepEqual(got, first) {
				t.Fatalf("seed %d gave different answers: %+v vs %+v", seed, first, got)
			}
		}
	}
}

func TestFailureIsRetryable(t *testing.T) {
	p := New(Config{FailureRate: 1})
	_, err := p.Complete(context.Background(), provider.Request{Prompt: "x", Tools: tools})
	if !provider.IsRetryable(err) {
		t.Fatalf("simulated failures must be retryable, got %v", err)
	}
}

func TestStem(t *testing.T) {
	for in, want := range map[string]string{
		"issues": "issu", "issue": "issu", "closing": "clos", "closed": "clos",
		"close": "clos", "categories": "category", "class": "class",
	} {
		if got := stem(in); got != want {
			t.Errorf("stem(%q) = %q, want %q", in, got, want)
		}
	}
}
