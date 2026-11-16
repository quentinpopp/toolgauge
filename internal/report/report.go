// Package report turns trials into per-scenario summaries, saves them as
// baselines and compares two of them.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/quentinpopp/toolgauge/internal/grader"
	"github.com/quentinpopp/toolgauge/internal/runner"
	"github.com/quentinpopp/toolgauge/internal/spec"
	"github.com/quentinpopp/toolgauge/internal/stats"
)

// Report is the saved result of a run. Its JSON form is the baseline format.
type Report struct {
	Suite     string    `json:"suite"`
	Provider  string    `json:"provider"`
	Model     string    `json:"model,omitempty"`
	Runs      int       `json:"runs"`
	Seed      uint64    `json:"seed"`
	CreatedAt time.Time `json:"created_at"`
	// ToolsFingerprint identifies the system prompt and tool definitions.
	ToolsFingerprint string `json:"tools_fingerprint,omitempty"`
	// Incomplete is set when the run was cancelled or ran out of budget; only
	// finished trials are included.
	Incomplete bool `json:"incomplete,omitempty"`
	// Requests counts calls that reached the provider, retries included;
	// CacheHits counts answers served from the cache instead.
	Requests  int64             `json:"requests"`
	CacheHits int64             `json:"cache_hits,omitempty"`
	Total     Summary           `json:"total"`
	Scenarios []ScenarioSummary `json:"scenarios"`
	Trials    []runner.Trial    `json:"trials,omitempty"`
}

// AddFingerprints records what was tested, so compare can detect edited
// scenarios and changed tool definitions.
func (r *Report) AddFingerprints(s *spec.Suite) {
	r.ToolsFingerprint = s.ToolsFingerprint()
	fp := map[string]string{}
	for _, sc := range s.Scenarios {
		fp[sc.ID] = sc.Fingerprint()
	}
	for i := range r.Scenarios {
		r.Scenarios[i].Fingerprint = fp[r.Scenarios[i].ID]
	}
}

// Summary counts passes over some set of trials.
type Summary struct {
	Trials   int                    `json:"trials"`
	Passes   int                    `json:"passes"`
	PassRate float64                `json:"pass_rate"`
	CILow    float64                `json:"ci_low"`
	CIHigh   float64                `json:"ci_high"`
	Outcomes map[grader.Outcome]int `json:"outcomes"`
	// MedianLatency is the median over all trials, retries included.
	MedianLatency time.Duration `json:"median_latency_ns"`
}

// ScenarioSummary is a Summary for one scenario, with a sample failure.
type ScenarioSummary struct {
	ID          string `json:"id"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Summary
	ExampleFailure string `json:"example_failure,omitempty"`
}

// Build summarises trials, keeping scenario order as it first appears.
func Build(trials []runner.Trial) (Summary, []ScenarioSummary) {
	var order []string
	by := map[string][]runner.Trial{}
	for _, t := range trials {
		if _, ok := by[t.Scenario]; !ok {
			order = append(order, t.Scenario)
		}
		by[t.Scenario] = append(by[t.Scenario], t)
	}
	scenarios := make([]ScenarioSummary, 0, len(order))
	for _, id := range order {
		ss := ScenarioSummary{ID: id, Summary: summarize(by[id])}
		for _, t := range by[id] {
			if t.Outcome != grader.Pass {
				ss.ExampleFailure = fmt.Sprintf("%s: %s", t.Outcome, t.Detail)
				break
			}
		}
		scenarios = append(scenarios, ss)
	}
	return summarize(trials), scenarios
}

func summarize(trials []runner.Trial) Summary {
	s := Summary{Trials: len(trials), Outcomes: map[grader.Outcome]int{}}
	lat := make([]time.Duration, 0, len(trials))
	for _, t := range trials {
		s.Outcomes[t.Outcome]++
		if t.Outcome == grader.Pass {
			s.Passes++
		}
		lat = append(lat, t.Latency)
	}
	if s.Trials > 0 {
		s.PassRate = float64(s.Passes) / float64(s.Trials)
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		s.MedianLatency = lat[len(lat)/2]
	}
	s.CILow, s.CIHigh = stats.Wilson(s.Passes, s.Trials)
	return s
}

// Save writes the report as indented JSON.
func (r *Report) Save(path string) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// Load reads a report saved with Save.
func Load(path string) (*Report, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Report
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &r, nil
}

// WriteText prints a human-readable summary.
func (r *Report) WriteText(w io.Writer) {
	model := ""
	if r.Model != "" {
		model = " · model " + r.Model
	}
	fmt.Fprintf(w, "suite %s · provider %s%s · %d runs per scenario\n\n", r.Suite, r.Provider, model, r.Runs)
	if r.Incomplete {
		fmt.Fprintf(w, "INCOMPLETE: the run was stopped early, only %d finished trials are included\n\n", r.Total.Trials)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SCENARIO\tPASS\tRATE\t95% CI\tEXAMPLE FAILURE")
	for _, s := range r.Scenarios {
		fmt.Fprintf(tw, "%s\t%d/%d\t%s\t%s\t%s\n", s.ID, s.Passes, s.Trials, pct(s.PassRate),
			ci(s.Summary), truncate(s.ExampleFailure, 60))
	}
	fmt.Fprintf(tw, "TOTAL\t%d/%d\t%s\t%s\t\n", r.Total.Passes, r.Total.Trials, pct(r.Total.PassRate), ci(r.Total))
	tw.Flush()

	var parts []string
	for _, o := range []grader.Outcome{grader.WrongTool, grader.BadArgs, grader.NoCall, grader.UnexpectedCall, grader.Error} {
		if n := r.Total.Outcomes[o]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", o, n))
		}
	}
	if len(parts) > 0 {
		fmt.Fprintf(w, "\nfailures: %s\n", strings.Join(parts, ", "))
	}
	if r.Provider != "mock" {
		var in, out int
		for _, t := range r.Trials {
			in += t.Usage.InputTokens
			out += t.Usage.OutputTokens
		}
		fmt.Fprintf(w, "requests: %d sent, %d from cache · billed tokens: %d in, %d out\n", r.Requests, r.CacheHits, in, out)
	}
}

func pct(f float64) string { return fmt.Sprintf("%.0f%%", f*100) }

func ci(s Summary) string { return fmt.Sprintf("%.0f–%.0f%%", s.CILow*100, s.CIHigh*100) }

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
