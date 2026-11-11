package grader

import (
	"testing"

	"github.com/quentinpopp/toolgauge/internal/provider"
	"github.com/quentinpopp/toolgauge/internal/spec"
)

func expectation(t *testing.T) spec.Expectation {
	t.Helper()
	s, err := spec.Parse([]byte(`
name: g
tools:
  - {name: close_issue, description: Close an issue.}
  - {name: create_issue, description: Create an issue.}
scenarios:
  - id: a
    prompt: close 42
    expect:
      tool: close_issue
      args: {number: 42, comment: {contains: dup}}
`))
	if err != nil {
		t.Fatal(err)
	}
	return s.Scenarios[0].Expect
}

func call(name string, args map[string]any) provider.Response {
	return provider.Response{ToolCalls: []provider.ToolCall{{Name: name, Args: args}}}
}

func TestGrade(t *testing.T) {
	exp := expectation(t)
	cases := []struct {
		name string
		resp provider.Response
		want Outcome
	}{
		{"pass", call("close_issue", map[string]any{"number": 42.0, "comment": "Duplicate of #3"}), Pass},
		{"extra args are fine", call("close_issue", map[string]any{"number": 42.0, "comment": "dup", "x": 1}), Pass},
		{"wrong tool", call("create_issue", nil), WrongTool},
		{"no call", provider.Response{Text: "done"}, NoCall},
		{"missing arg", call("close_issue", map[string]any{"number": 42.0}), BadArgs},
		{"wrong arg", call("close_issue", map[string]any{"number": 41.0, "comment": "dup"}), BadArgs},
	}
	for _, c := range cases {
		if got, detail := Grade(exp, c.resp); got != c.want {
			t.Errorf("%s: got %s (%s), want %s", c.name, got, detail, c.want)
		}
	}
}

func TestGradeNoTool(t *testing.T) {
	exp := spec.Expectation{NoTool: true}
	if got, _ := Grade(exp, provider.Response{Text: "hi"}); got != Pass {
		t.Errorf("got %s, want pass", got)
	}
	if got, _ := Grade(exp, call("close_issue", nil)); got != UnexpectedCall {
		t.Errorf("got %s, want unexpected_call", got)
	}
}

func TestDetailIsStable(t *testing.T) {
	exp := expectation(t)
	resp := call("close_issue", map[string]any{})
	_, first := Grade(exp, resp)
	for range 20 {
		if _, d := Grade(exp, resp); d != first {
			t.Fatalf("detail changed between calls: %q vs %q", first, d)
		}
	}
}
