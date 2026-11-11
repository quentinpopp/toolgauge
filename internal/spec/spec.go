// Package spec defines the suite format: the tools an agent can call and the
// scenarios that describe what a correct agent should do with them.
package spec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// Suite is one test suite, usually one integration.
type Suite struct {
	Name      string     `yaml:"name" json:"name"`
	System    string     `yaml:"system,omitempty" json:"system,omitempty"`
	Tools     []Tool     `yaml:"tools" json:"tools"`
	Scenarios []Scenario `yaml:"scenarios" json:"scenarios"`
}

// Tool is a tool definition as it is shown to the model. Parameters is a JSON
// Schema object, the same shape MCP and the major chat APIs use.
type Tool struct {
	Name        string         `yaml:"name" json:"name"`
	Description string         `yaml:"description" json:"description"`
	Parameters  map[string]any `yaml:"parameters,omitempty" json:"parameters,omitempty"`
}

// Scenario is one user request and the behaviour expected from the agent.
type Scenario struct {
	ID     string      `yaml:"id" json:"id"`
	Prompt string      `yaml:"prompt" json:"prompt"`
	Expect Expectation `yaml:"expect" json:"expect"`
}

// Expectation describes the first tool call a correct agent makes.
// Exactly one of Tool or NoTool must be set.
type Expectation struct {
	Tool   string                `yaml:"tool,omitempty" json:"tool,omitempty"`
	NoTool bool                  `yaml:"no_tool,omitempty" json:"no_tool,omitempty"`
	Args   map[string]ArgMatcher `yaml:"args,omitempty" json:"args,omitempty"`
}

// MatchKind selects how an expected argument is compared.
type MatchKind string

const (
	MatchEquals   MatchKind = "equals"
	MatchContains MatchKind = "contains"
	MatchOneOf    MatchKind = "one_of"
	MatchPresent  MatchKind = "present"
)

// ArgMatcher checks a single argument of a tool call.
//
// In YAML a plain value means "equals":
//
//	repo: acme/web
//
// A mapping always selects a matcher and must have exactly one of the keys
// equals, contains, one_of or present:
//
//	title: {contains: login}
//	state: {one_of: [open, all]}
//	body:  {present: true}
//	filter: {equals: {state: open}}   # an object value needs equals
//
// Anything else is an error, so a typo such as {contians: login} cannot turn
// into an equality check that silently fails every trial.
type ArgMatcher struct {
	Kind  MatchKind `json:"kind"`
	Value any       `json:"value,omitempty"`
}

// UnmarshalYAML implements the shorthand described on ArgMatcher.
func (m *ArgMatcher) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.MappingNode {
		if len(n.Content) != 2 {
			return fmt.Errorf("line %d: a matcher needs exactly one of equals, contains, one_of, present (wrap object values in equals)", n.Line)
		}
		key := n.Content[0].Value
		switch MatchKind(key) {
		case MatchEquals, MatchContains, MatchOneOf, MatchPresent:
		default:
			return fmt.Errorf("line %d: unknown matcher %q, want equals, contains, one_of or present (wrap object values in equals)", n.Content[0].Line, key)
		}
		var v any
		if err := n.Content[1].Decode(&v); err != nil {
			return err
		}
		m.Kind, m.Value = MatchKind(key), normalize(v)
		if err := m.check(); err != nil {
			return fmt.Errorf("line %d: %w", n.Line, err)
		}
		return nil
	}
	var v any
	if err := n.Decode(&v); err != nil {
		return err
	}
	m.Kind, m.Value = MatchEquals, normalize(v)
	return nil
}

func (m ArgMatcher) check() error {
	switch m.Kind {
	case MatchContains:
		if _, ok := m.Value.(string); !ok {
			return errors.New("contains needs a string")
		}
	case MatchOneOf:
		if l, ok := m.Value.([]any); !ok || len(l) == 0 {
			return errors.New("one_of needs a non-empty list")
		}
	case MatchPresent:
		if b, ok := m.Value.(bool); !ok || !b {
			return errors.New("present must be true")
		}
	}
	return nil
}

// Match reports whether an actual argument value satisfies the matcher.
// ok is false when the argument is missing from the call. The YAML shorthand
// guarantees Kind and Value fit together; matchers built in code must use
// the same shapes.
func (m ArgMatcher) Match(actual any, ok bool) bool {
	if !ok {
		return false
	}
	actual = normalize(actual)
	switch m.Kind {
	case MatchPresent:
		// An explicit null is no value: {"body": null} does not supply a body.
		return actual != nil
	case MatchContains:
		s, isStr := actual.(string)
		return isStr && strings.Contains(strings.ToLower(s), strings.ToLower(m.Value.(string)))
	case MatchOneOf:
		for _, v := range m.Value.([]any) {
			if reflect.DeepEqual(v, actual) {
				return true
			}
		}
		return false
	default:
		return reflect.DeepEqual(m.Value, actual)
	}
}

// Example returns a value that satisfies the matcher. The mock provider uses
// it to play a model that knows the right answer.
func (m ArgMatcher) Example() any {
	switch m.Kind {
	case MatchOneOf:
		return m.Value.([]any)[0]
	case MatchPresent:
		return "example"
	default:
		return m.Value
	}
}

func (m ArgMatcher) String() string {
	b, _ := json.Marshal(m.Value)
	if m.Kind == MatchEquals {
		return string(b)
	}
	return fmt.Sprintf("%s %s", m.Kind, b)
}

// normalize converts a value to the shapes encoding/json produces (float64,
// []any, map[string]any), so YAML input and API output compare equal.
func normalize(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return v
	}
	return out
}

// Load reads and validates a suite file.
func Load(path string) (*Suite, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse decodes and validates a suite.
func Parse(data []byte) (*Suite, error) {
	var s Suite
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("parse suite: %w", err)
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

// Validate checks the suite for mistakes that would make results meaningless.
// It returns all problems at once.
func (s *Suite) Validate() error {
	var errs []error
	if s.Name == "" {
		errs = append(errs, errors.New("suite: name is required"))
	}
	if len(s.Tools) == 0 {
		errs = append(errs, errors.New("suite: at least one tool is required"))
	}
	tools := map[string]Tool{}
	for i, t := range s.Tools {
		switch {
		case t.Name == "":
			errs = append(errs, fmt.Errorf("tools[%d]: name is required", i))
		case tools[t.Name].Name != "":
			errs = append(errs, fmt.Errorf("tool %q: defined twice", t.Name))
		}
		if strings.TrimSpace(t.Description) == "" {
			errs = append(errs, fmt.Errorf("tool %q: description is required", t.Name))
		}
		tools[t.Name] = t
	}
	if len(s.Scenarios) == 0 {
		errs = append(errs, errors.New("suite: at least one scenario is required"))
	}
	ids := map[string]bool{}
	for i, sc := range s.Scenarios {
		name := sc.ID
		if name == "" {
			name = fmt.Sprintf("scenarios[%d]", i)
			errs = append(errs, fmt.Errorf("%s: id is required", name))
		} else if ids[name] {
			errs = append(errs, fmt.Errorf("scenario %q: id used twice", name))
		}
		ids[name] = true
		if strings.TrimSpace(sc.Prompt) == "" {
			errs = append(errs, fmt.Errorf("scenario %q: prompt is required", name))
		}
		e := sc.Expect
		switch {
		case e.NoTool && e.Tool != "":
			errs = append(errs, fmt.Errorf("scenario %q: set either expect.tool or expect.no_tool, not both", name))
		case e.NoTool && len(e.Args) > 0:
			errs = append(errs, fmt.Errorf("scenario %q: expect.args makes no sense with no_tool", name))
		case !e.NoTool && e.Tool == "":
			errs = append(errs, fmt.Errorf("scenario %q: expect.tool or expect.no_tool is required", name))
		case e.Tool != "":
			t, ok := tools[e.Tool]
			if !ok {
				errs = append(errs, fmt.Errorf("scenario %q: unknown tool %q", name, e.Tool))
				break
			}
			props, _ := t.Parameters["properties"].(map[string]any)
			for arg := range e.Args {
				if _, ok := props[arg]; props != nil && !ok {
					errs = append(errs, fmt.Errorf("scenario %q: tool %q has no parameter %q", name, e.Tool, arg))
				}
			}
		}
	}
	return errors.Join(errs...)
}

// Tool returns the tool with the given name.
func (s *Suite) Tool(name string) (Tool, bool) {
	for _, t := range s.Tools {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// Fingerprint identifies what a scenario tests: its prompt and expectation.
// Reports store it so that compare can tell "the model got worse" from "the
// test was edited but kept its ID".
func (sc Scenario) Fingerprint() string {
	return fingerprint(struct {
		Prompt string
		Expect Expectation
	}{sc.Prompt, sc.Expect})
}

// ToolsFingerprint identifies everything the model sees besides the prompt:
// the system prompt and the tool definitions. It changes on purpose when
// comparing descriptions, so compare reports it rather than rejecting it.
func (s *Suite) ToolsFingerprint() string {
	return fingerprint(struct {
		System string
		Tools  []Tool
	}{s.System, s.Tools})
}

// fingerprint hashes the JSON form of v. encoding/json sorts map keys, so the
// result is stable across runs.
func fingerprint(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("fingerprint: %v", err)) // only plain data reaches here
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// LoadDescriptions reads a YAML map of tool name to description, used to try
// alternative wordings without copying the whole suite.
func LoadDescriptions(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]string
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse descriptions: %w", err)
	}
	return m, nil
}

// WithDescriptions returns a copy of the suite with tool descriptions replaced.
// Every name in the map must be a tool of the suite.
func (s *Suite) WithDescriptions(d map[string]string) (*Suite, error) {
	out := *s
	out.Tools = append([]Tool(nil), s.Tools...)
	for name, desc := range d {
		found := false
		for i := range out.Tools {
			if out.Tools[i].Name == name {
				out.Tools[i].Description = desc
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("descriptions: unknown tool %q", name)
		}
	}
	return &out, out.Validate()
}
