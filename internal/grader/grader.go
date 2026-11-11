// Package grader decides whether a model response meets a scenario's
// expectation.
package grader

import (
	"fmt"
	"sort"
	"strings"

	"github.com/quentinpopp/toolgauge/internal/provider"
	"github.com/quentinpopp/toolgauge/internal/spec"
)

// Outcome classifies a single trial. Failures are split by cause because
// "picked the wrong tool" and "picked the right tool with a wrong argument"
// call for different fixes.
type Outcome string

const (
	Pass           Outcome = "pass"
	WrongTool      Outcome = "wrong_tool"
	NoCall         Outcome = "no_call"
	UnexpectedCall Outcome = "unexpected_call"
	BadArgs        Outcome = "bad_args"
	Error          Outcome = "error"
)

// Grade compares the first tool call in resp with the expectation.
// Detail explains a failure in a form that can go straight into a report.
func Grade(exp spec.Expectation, resp provider.Response) (Outcome, string) {
	if exp.NoTool {
		if len(resp.ToolCalls) > 0 {
			return UnexpectedCall, fmt.Sprintf("called %s, expected no tool", resp.ToolCalls[0].Name)
		}
		return Pass, ""
	}
	if len(resp.ToolCalls) == 0 {
		return NoCall, fmt.Sprintf("no tool call, expected %s", exp.Tool)
	}
	call := resp.ToolCalls[0]
	if call.Name != exp.Tool {
		return WrongTool, fmt.Sprintf("called %s, expected %s", call.Name, exp.Tool)
	}
	var problems []string
	for name, m := range exp.Args {
		v, ok := call.Args[name]
		if m.Match(v, ok) {
			continue
		}
		if !ok {
			problems = append(problems, fmt.Sprintf("%s missing", name))
		} else {
			problems = append(problems, fmt.Sprintf("%s=%v, want %s", name, v, m))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return BadArgs, strings.Join(problems, "; ")
	}
	return Pass, ""
}
