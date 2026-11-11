// Package stats holds the small amount of statistics toolgauge needs to tell
// a real change in pass rate from sampling noise.
package stats

import (
	"math"
	"sort"
)

// Wilson returns the Wilson score interval for k successes in n trials at
// about 95% confidence. Unlike the textbook p ± 1.96·SE it stays inside [0,1]
// and behaves well at 0% and 100%, which are common in eval results.
func Wilson(k, n int) (low, high float64) {
	if n == 0 {
		return 0, 1
	}
	const z = 1.959963984540054
	p := float64(k) / float64(n)
	nf := float64(n)
	den := 1 + z*z/nf
	center := (p + z*z/(2*nf)) / den
	half := z * math.Sqrt(p*(1-p)/nf+z*z/(4*nf*nf)) / den
	return math.Max(0, center-half), math.Min(1, center+half)
}

// FisherLess is the one-sided p-value of Fisher's exact test for the
// hypothesis that group B has a lower success rate than group A, given kA of
// nA and kB of nB successes. Exact tests suit eval runs, where n per scenario
// is often 10 to 50 and normal approximations are poor.
func FisherLess(kA, nA, kB, nB int) float64 {
	// Under H0, with the total successes fixed, kB is hypergeometric.
	n, k := nA+nB, kA+kB
	lo := max(0, k-nA)
	var p float64
	for x := lo; x <= kB; x++ {
		p += math.Exp(logHypergeom(x, n, k, nB))
	}
	return math.Min(1, p)
}

// FisherGreater is the one-sided p-value for B having a higher success rate.
func FisherGreater(kA, nA, kB, nB int) float64 {
	return FisherLess(nA-kA, nA, nB-kB, nB)
}

// logHypergeom is log P(X = x) for x successes in a sample of size draws from
// a population of size n containing k successes.
func logHypergeom(x, n, k, draws int) float64 {
	return logChoose(k, x) + logChoose(n-k, draws-x) - logChoose(n, draws)
}

func logChoose(n, k int) float64 {
	a, _ := math.Lgamma(float64(n + 1))
	b, _ := math.Lgamma(float64(k + 1))
	c, _ := math.Lgamma(float64(n - k + 1))
	return a - b - c
}

// Holm adjusts p-values for testing many scenarios at once with the
// Holm-Bonferroni method. Without it, a suite of 40 scenarios at α = 0.05
// would report about two "regressions" per run from noise alone.
func Holm(p []float64) []float64 {
	idx := make([]int, len(p))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return p[idx[a]] < p[idx[b]] })
	out := make([]float64, len(p))
	running := 0.0
	m := len(p)
	for rank, i := range idx {
		adj := math.Min(1, float64(m-rank)*p[i])
		running = math.Max(running, adj) // keep adjusted values monotone
		out[i] = running
	}
	return out
}
