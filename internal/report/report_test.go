package report

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/quentinpopp/toolgauge/internal/grader"
	"github.com/quentinpopp/toolgauge/internal/runner"
)

// fakeReport builds a report with the given passes out of n per scenario.
func fakeReport(n int, passes map[string]int, order ...string) *Report {
	var trials []runner.Trial
	for _, id := range order {
		for i := range n {
			o := grader.WrongTool
			if i < passes[id] {
				o = grader.Pass
			}
			trials = append(trials, runner.Trial{Scenario: id, Index: i, Outcome: o})
		}
	}
	r := &Report{Suite: "s", Runs: n}
	r.Total, r.Scenarios = Build(trials)
	return r
}

var opts = CompareOptions{Alpha: 0.05, MinDelta: 0.05}

func TestBuild(t *testing.T) {
	r := fakeReport(10, map[string]int{"a": 7, "b": 10}, "a", "b")
	if r.Total.Passes != 17 || r.Total.Trials != 20 {
		t.Fatalf("total = %+v", r.Total)
	}
	if r.Scenarios[0].ID != "a" || r.Scenarios[0].ExampleFailure == "" || r.Scenarios[1].ExampleFailure != "" {
		t.Fatalf("scenarios = %+v", r.Scenarios)
	}
}

func TestCompareFlagsRealDrop(t *testing.T) {
	base := fakeReport(30, map[string]int{"a": 29, "b": 28}, "a", "b")
	cur := fakeReport(30, map[string]int{"a": 12, "b": 28}, "a", "b")
	c := Compare(base, cur, opts)
	if !c.Regressed() || c.Diffs[0].Status != Regressed || c.Diffs[1].Status != Unchanged {
		t.Fatalf("got %+v", c.Diffs)
	}
}

func TestCompareIgnoresNoise(t *testing.T) {
	base := fakeReport(20, map[string]int{"a": 18}, "a")
	cur := fakeReport(20, map[string]int{"a": 15}, "a")
	if c := Compare(base, cur, opts); c.Regressed() {
		t.Fatalf("18/20 -> 15/20 is within noise, got %+v", c.Diffs[0])
	}
}

func TestCompareMinDelta(t *testing.T) {
	// 99% -> 97% over 2000 trials is significant but below the 5 pp threshold.
	base := fakeReport(2000, map[string]int{"a": 1980}, "a")
	cur := fakeReport(2000, map[string]int{"a": 1940}, "a")
	if c := Compare(base, cur, opts); c.Regressed() {
		t.Fatalf("small drop should be ignored, got %+v", c.Diffs[0])
	}
}

func TestCompareDropExactlyAtMinDelta(t *testing.T) {
	// 95% -> 90% is a 5 pp drop, but 0.95-0.90 is 0.0499999… in float64.
	base := fakeReport(1000, map[string]int{"a": 950}, "a")
	cur := fakeReport(1000, map[string]int{"a": 900}, "a")
	if c := Compare(base, cur, opts); !c.Regressed() {
		t.Fatalf("a drop of exactly min-delta must count, got %+v", c.Diffs[0])
	}
}

func TestCompareEditedScenarioIsNotTested(t *testing.T) {
	base := fakeReport(30, map[string]int{"a": 29, "b": 29}, "a", "b")
	cur := fakeReport(30, map[string]int{"a": 5, "b": 29}, "a", "b")
	base.Scenarios[0].Fingerprint, cur.Scenarios[0].Fingerprint = "old", "new"
	base.Scenarios[1].Fingerprint, cur.Scenarios[1].Fingerprint = "same", "same"
	c := Compare(base, cur, opts)
	if c.Diffs[0].Status != Changed || c.Regressed() {
		t.Fatalf("an edited scenario must be reported as changed, not regressed: %+v", c.Diffs)
	}
	// The total only pools scenarios that were compared.
	if c.Total.Base.Trials != 30 || c.Total.Current.Trials != 30 {
		t.Fatalf("total should only include b, got %+v / %+v", c.Total.Base, c.Total.Current)
	}
}

func TestCompareFlagsToolChangesAndIncompleteRuns(t *testing.T) {
	base := fakeReport(10, map[string]int{"a": 9}, "a")
	cur := fakeReport(10, map[string]int{"a": 9}, "a")
	base.ToolsFingerprint, cur.ToolsFingerprint = "x", "y"
	cur.Incomplete = true
	c := Compare(base, cur, opts)
	if !c.ToolsChanged || !c.Incomplete {
		t.Fatalf("got %+v", c)
	}
	var b strings.Builder
	c.WriteText(&b)
	if !strings.Contains(b.String(), "tool definitions") || !strings.Contains(b.String(), "cancelled run") {
		t.Errorf("warnings missing from output:\n%s", b.String())
	}
}

func TestCompareImprovementAndMembership(t *testing.T) {
	base := fakeReport(30, map[string]int{"a": 10, "gone": 30}, "a", "gone")
	cur := fakeReport(30, map[string]int{"a": 29, "new": 30}, "a", "new")
	c := Compare(base, cur, opts)
	got := map[string]Status{}
	for _, d := range c.Diffs {
		got[d.ID] = d.Status
	}
	if got["a"] != Improved || got["new"] != Added || got["gone"] != Removed || c.Regressed() {
		t.Fatalf("got %v", got)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	r := fakeReport(5, map[string]int{"a": 3}, "a")
	path := filepath.Join(t.TempDir(), "r.json")
	if err := r.Save(path); err != nil {
		t.Fatal(err)
	}
	back, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if back.Total.Passes != 3 || back.Scenarios[0].Outcomes[grader.WrongTool] != 2 {
		t.Fatalf("round trip lost data: %+v", back)
	}
}

func TestWriteText(t *testing.T) {
	var b strings.Builder
	fakeReport(10, map[string]int{"a": 7}, "a").WriteText(&b)
	for _, want := range []string{"a ", "7/10", "70%", "TOTAL", "wrong_tool 3"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("output missing %q:\n%s", want, b.String())
		}
	}
}
