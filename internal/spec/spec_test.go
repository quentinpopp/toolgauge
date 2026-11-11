package spec

import (
	"strings"
	"testing"
)

const validSuite = `
name: demo
tools:
  - name: create_issue
    description: Create an issue.
    parameters:
      type: object
      properties:
        repo: {type: string}
        title: {type: string}
        number: {type: integer}
scenarios:
  - id: a
    prompt: open a bug
    expect:
      tool: create_issue
      args:
        repo: acme/web
        number: 3
        title: {contains: Login}
  - id: b
    prompt: hi
    expect: {no_tool: true}
`

func TestParseValid(t *testing.T) {
	s, err := Parse([]byte(validSuite))
	if err != nil {
		t.Fatal(err)
	}
	args := s.Scenarios[0].Expect.Args
	if args["repo"].Kind != MatchEquals || args["title"].Kind != MatchContains {
		t.Fatalf("unexpected matcher kinds: %+v", args)
	}
	// YAML ints are normalised so they compare equal to JSON numbers.
	if !args["number"].Match(float64(3), true) {
		t.Error("integer from YAML should match float64 from JSON")
	}
}

func TestValidateReportsAllProblems(t *testing.T) {
	_, err := Parse([]byte(`
name: broken
tools:
  - name: t
    description: ""
    parameters: {type: object, properties: {a: {type: string}}}
scenarios:
  - id: x
    prompt: p
    expect: {tool: missing}
  - id: x
    prompt: p
    expect: {tool: t, args: {nope: 1}}
  - id: y
    prompt: p
    expect: {}
`))
	if err == nil {
		t.Fatal("expected validation errors")
	}
	for _, want := range []string{
		`tool "t": description is required`,
		`unknown tool "missing"`,
		`id used twice`,
		`has no parameter "nope"`,
		`expect.tool or expect.no_tool is required`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got:\n%v", want, err)
		}
	}
}

func TestUnknownFieldIsRejected(t *testing.T) {
	_, err := Parse([]byte(strings.Replace(validSuite, "prompt: hi", "promt: hi", 1)))
	if err == nil || !strings.Contains(err.Error(), "promt") {
		t.Fatalf("typo in field name should be an error, got %v", err)
	}
}

func TestMatcherTyposAreRejected(t *testing.T) {
	for _, bad := range []string{
		`{contians: login}`,          // misspelled matcher
		`{state: open}`,              // object value without equals
		`{contains: a, one_of: [b]}`, // two matchers at once
	} {
		_, err := Parse([]byte(strings.Replace(validSuite, "repo: acme/web", "repo: "+bad, 1)))
		if err == nil {
			t.Errorf("%s should be rejected", bad)
		}
	}
}

func TestFingerprints(t *testing.T) {
	a, _ := Parse([]byte(validSuite))
	b, _ := Parse([]byte(validSuite))
	if a.Scenarios[0].Fingerprint() != b.Scenarios[0].Fingerprint() || a.ToolsFingerprint() != b.ToolsFingerprint() {
		t.Fatal("same input must give the same fingerprints")
	}
	edited, _ := Parse([]byte(strings.Replace(validSuite, "prompt: open a bug", "prompt: open a ticket", 1)))
	if edited.Scenarios[0].Fingerprint() == a.Scenarios[0].Fingerprint() {
		t.Error("editing the prompt must change the scenario fingerprint")
	}
	if edited.Scenarios[1].Fingerprint() != a.Scenarios[1].Fingerprint() {
		t.Error("other scenarios must keep their fingerprint")
	}
	reworded, _ := a.WithDescriptions(map[string]string{"create_issue": "other words"})
	if reworded.ToolsFingerprint() == a.ToolsFingerprint() {
		t.Error("new descriptions must change the tools fingerprint")
	}
	if reworded.Scenarios[0].Fingerprint() != a.Scenarios[0].Fingerprint() {
		t.Error("new descriptions must not change scenario fingerprints")
	}
}

func TestMatchers(t *testing.T) {
	cases := []struct {
		yaml   string
		actual any
		ok     bool
		want   bool
	}{
		{`acme/web`, "acme/web", true, true},
		{`acme/web`, "acme/api", true, false},
		{`acme/web`, nil, false, false},
		{`{contains: LOGIN}`, "Login button broken", true, true},
		{`{contains: login}`, 42.0, true, false},
		{`{one_of: [open, all]}`, "all", true, true},
		{`{one_of: [open, all]}`, "closed", true, false},
		{`{present: true}`, "", true, true},
		{`{present: true}`, nil, false, false},
		{`{present: true}`, nil, true, false}, // explicit null is not a value
		{`[a, b]`, []any{"a", "b"}, true, true},
		{`{equals: {contains: x}}`, map[string]any{"contains": "x"}, true, true},
	}
	for _, c := range cases {
		s, err := Parse([]byte(strings.Replace(validSuite, "repo: acme/web", "repo: "+c.yaml, 1)))
		if err != nil {
			t.Fatalf("%s: %v", c.yaml, err)
		}
		m := s.Scenarios[0].Expect.Args["repo"]
		if got := m.Match(c.actual, c.ok); got != c.want {
			t.Errorf("%s vs %#v: got %v, want %v", c.yaml, c.actual, got, c.want)
		}
		if c.want && c.ok && !m.Match(m.Example(), true) {
			t.Errorf("%s: Example() %v should satisfy its own matcher", c.yaml, m.Example())
		}
	}
}

func TestBadMatcher(t *testing.T) {
	_, err := Parse([]byte(strings.Replace(validSuite, "repo: acme/web", "repo: {one_of: []}", 1)))
	if err == nil {
		t.Fatal("empty one_of should be rejected")
	}
}

func TestWithDescriptions(t *testing.T) {
	s, _ := Parse([]byte(validSuite))
	out, err := s.WithDescriptions(map[string]string{"create_issue": "new words"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Tools[0].Description != "new words" || s.Tools[0].Description == "new words" {
		t.Fatal("override must change the copy and leave the original alone")
	}
	if _, err := s.WithDescriptions(map[string]string{"nope": "x"}); err == nil {
		t.Fatal("unknown tool in overrides should be an error")
	}
}
