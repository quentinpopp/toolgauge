package report

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/quentinpopp/toolgauge/internal/stats"
)

// Status is the verdict for one scenario when comparing two reports.
type Status string

const (
	Unchanged Status = "unchanged"
	Regressed Status = "REGRESSED"
	Improved  Status = "improved"
	Added     Status = "added"
	Removed   Status = "removed"
	// Changed means the scenario's prompt or expectation was edited, so the
	// two pass rates measure different things and are not compared.
	Changed Status = "changed"
)

// CompareOptions decide what counts as a real change.
type CompareOptions struct {
	// Alpha is the significance level after Holm correction.
	Alpha float64
	// MinDelta ignores changes smaller than this many percentage points
	// (as a fraction), even if significant. A drop from 99% to 97% over
	// thousands of trials is real but rarely worth failing a build for.
	MinDelta float64
}

// Diff is the comparison of one scenario.
type Diff struct {
	ID      string
	Base    *Summary
	Current *Summary
	Delta   float64
	PValue  float64 // Holm-adjusted, one-sided in the direction of Delta
	Status  Status
}

// Comparison is the result of comparing a current report with a baseline.
type Comparison struct {
	Diffs []Diff
	// Total pools the scenarios that were compared, so added, removed and
	// changed scenarios do not distort it.
	Total Diff
	// ToolsChanged is true when the tool definitions or system prompt differ
	// from the baseline. That is expected when comparing descriptions and a
	// warning sign otherwise.
	ToolsChanged bool
	// Incomplete is true when either report comes from a cancelled run.
	Incomplete bool
}

// Regressed reports whether any scenario or the total got significantly worse.
func (c Comparison) Regressed() bool {
	if c.Total.Status == Regressed {
		return true
	}
	for _, d := range c.Diffs {
		if d.Status == Regressed {
			return true
		}
	}
	return false
}

// Compare tests each scenario for a change in pass rate. For every scenario
// in both reports with the same fingerprint it runs a one-sided Fisher exact
// test in the direction of the observed change, then corrects all p-values
// together with Holm's method so that large suites do not produce false
// alarms. Reports without fingerprints (older baselines) are compared by ID.
func Compare(base, cur *Report, opt CompareOptions) Comparison {
	baseBy := map[string]ScenarioSummary{}
	for _, s := range base.Scenarios {
		baseBy[s.ID] = s
	}
	curBy := map[string]bool{}
	var diffs []Diff
	var raw []float64
	var tested []int
	var bt, ct Summary
	for _, s := range cur.Scenarios {
		curBy[s.ID] = true
		c := s.Summary
		b, ok := baseBy[s.ID]
		if !ok {
			diffs = append(diffs, Diff{ID: s.ID, Current: &c, Status: Added})
			continue
		}
		bs := b.Summary
		d := Diff{ID: s.ID, Base: &bs, Current: &c, Delta: c.PassRate - bs.PassRate}
		if b.Fingerprint != "" && s.Fingerprint != "" && b.Fingerprint != s.Fingerprint {
			d.Status = Changed
			diffs = append(diffs, d)
			continue
		}
		raw = append(raw, oneSided(bs, c))
		tested = append(tested, len(diffs))
		diffs = append(diffs, d)
		bt.Passes, bt.Trials = bt.Passes+bs.Passes, bt.Trials+bs.Trials
		ct.Passes, ct.Trials = ct.Passes+c.Passes, ct.Trials+c.Trials
	}
	for _, s := range base.Scenarios {
		if !curBy[s.ID] {
			b := s.Summary
			diffs = append(diffs, Diff{ID: s.ID, Base: &b, Status: Removed})
		}
	}
	adj := stats.Holm(raw)
	for i, di := range tested {
		diffs[di].PValue = adj[i]
		diffs[di].Status = verdict(diffs[di].Delta, adj[i], opt)
	}

	bt.PassRate, ct.PassRate = rateOf(bt), rateOf(ct)
	total := Diff{ID: "TOTAL", Base: &bt, Current: &ct, Delta: ct.PassRate - bt.PassRate, Status: Unchanged, PValue: 1}
	if bt.Trials > 0 && ct.Trials > 0 {
		total.PValue = oneSided(bt, ct)
		total.Status = verdict(total.Delta, total.PValue, opt)
	}
	return Comparison{
		Diffs:        diffs,
		Total:        total,
		ToolsChanged: base.ToolsFingerprint != "" && cur.ToolsFingerprint != "" && base.ToolsFingerprint != cur.ToolsFingerprint,
		Incomplete:   base.Incomplete || cur.Incomplete,
	}
}

func rateOf(s Summary) float64 {
	if s.Trials == 0 {
		return 0
	}
	return float64(s.Passes) / float64(s.Trials)
}

func oneSided(b, c Summary) float64 {
	if c.PassRate < b.PassRate {
		return stats.FisherLess(b.Passes, b.Trials, c.Passes, c.Trials)
	}
	return stats.FisherGreater(b.Passes, b.Trials, c.Passes, c.Trials)
}

// eps absorbs float rounding in pass-rate differences: 0.95-0.90 is
// 0.04999999999999993, which must count as a 5 pp change.
const eps = 1e-9

func verdict(delta, p float64, opt CompareOptions) Status {
	switch {
	case p >= opt.Alpha || abs(delta) < opt.MinDelta-eps:
		return Unchanged
	case delta < 0:
		return Regressed
	default:
		return Improved
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// WriteText prints the comparison as a table.
func (c Comparison) WriteText(w io.Writer) {
	if c.Incomplete {
		fmt.Fprintln(w, "warning: at least one report comes from a cancelled run")
	}
	if c.ToolsChanged {
		fmt.Fprintln(w, "note: tool definitions or system prompt differ from the baseline")
	}
	if c.Incomplete || c.ToolsChanged {
		fmt.Fprintln(w)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SCENARIO\tBASELINE\tCURRENT\tDELTA\tP (HOLM)\tSTATUS")
	for _, d := range append(c.Diffs, c.Total) {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", d.ID, rate(d.Base), rate(d.Current), delta(d), pval(d), d.Status)
	}
	tw.Flush()
}

func rate(s *Summary) string {
	if s == nil {
		return "–"
	}
	return fmt.Sprintf("%s (%d/%d)", pct(s.PassRate), s.Passes, s.Trials)
}

func delta(d Diff) string {
	if d.Base == nil || d.Current == nil {
		return "–"
	}
	return fmt.Sprintf("%+.0f pp", d.Delta*100)
}

func pval(d Diff) string {
	if d.Base == nil || d.Current == nil || d.Status == Changed {
		return "–"
	}
	if d.PValue < 0.001 {
		return "<0.001"
	}
	return fmt.Sprintf("%.3f", d.PValue)
}
