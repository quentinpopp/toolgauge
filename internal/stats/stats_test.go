package stats

import (
	"math"
	"testing"
)

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestWilson(t *testing.T) {
	// Reference values computed independently from the closed-form Wilson formula.
	cases := []struct {
		k, n      int
		low, high float64
	}{
		{8, 10, 0.4902, 0.9433},
		{0, 20, 0.0, 0.1611},
		{20, 20, 0.8389, 1.0},
		{50, 100, 0.4038, 0.5962},
	}
	for _, c := range cases {
		lo, hi := Wilson(c.k, c.n)
		if !near(lo, c.low, 1e-3) || !near(hi, c.high, 1e-3) {
			t.Errorf("Wilson(%d,%d) = %.4f–%.4f, want %.4f–%.4f", c.k, c.n, lo, hi, c.low, c.high)
		}
	}
	if lo, hi := Wilson(0, 0); lo != 0 || hi != 1 {
		t.Errorf("no data should give the full interval, got %v–%v", lo, hi)
	}
}

func TestFisher(t *testing.T) {
	// Reference: exact hypergeometric sum P(X <= 10) with N=40, K=28, n=20,
	// computed independently with integer binomials: 0.0069071.
	if p := FisherLess(18, 20, 10, 20); !near(p, 0.0069071, 1e-6) {
		t.Errorf("FisherLess = %.7f, want 0.0069071", p)
	}
	// Identical groups: no evidence either way.
	if p := FisherLess(10, 20, 10, 20); p < 0.5 {
		t.Errorf("equal groups should give a large p-value, got %.3f", p)
	}
	// A change in the other direction is never evidence of a drop.
	if p := FisherLess(5, 20, 15, 20); p < 0.99 {
		t.Errorf("improvement tested for a drop should give p≈1, got %.3f", p)
	}
	if a, b := FisherGreater(10, 20, 18, 20), FisherLess(18, 20, 10, 20); !near(a, b, 1e-12) {
		t.Errorf("FisherGreater should mirror FisherLess: %v vs %v", a, b)
	}
}

func TestHolm(t *testing.T) {
	got := Holm([]float64{0.01, 0.04, 0.03, 0.005})
	// Sorted: 0.005*4=0.02, 0.01*3=0.03, 0.03*2=0.06, 0.04*1=0.04 -> monotone 0.06.
	want := []float64{0.03, 0.06, 0.06, 0.02}
	for i := range want {
		if !near(got[i], want[i], 1e-12) {
			t.Fatalf("Holm = %v, want %v", got, want)
		}
	}
	if len(Holm(nil)) != 0 {
		t.Fatal("empty input should give empty output")
	}
}
