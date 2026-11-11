package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

const suite = "../../examples/github-issues/suite.yaml"
const vague = "../../examples/github-issues/descriptions-vague.yaml"

func cli(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out, errOut strings.Builder
	code := run(context.Background(), args, &out, &errOut)
	return code, out.String() + errOut.String()
}

// TestEndToEnd is the demo from the README: a baseline, a rerun with another
// seed that must not be flagged, and a run with vague descriptions that must.
func TestEndToEnd(t *testing.T) {
	dir := t.TempDir()
	base, rerun, bad := filepath.Join(dir, "base.json"), filepath.Join(dir, "rerun.json"), filepath.Join(dir, "bad.json")

	if code, out := cli(t, "validate", suite); code != exitOK {
		t.Fatalf("validate: %d\n%s", code, out)
	}
	if code, out := cli(t, "run", "--out", base, suite); code != exitOK {
		t.Fatalf("run: %d\n%s", code, out)
	}
	if code, out := cli(t, "run", "--seed", "7", "--out", rerun, suite); code != exitOK {
		t.Fatalf("rerun: %d\n%s", code, out)
	}
	if code, out := cli(t, "run", "--descriptions", vague, "--out", bad, suite); code != exitOK {
		t.Fatalf("vague run: %d\n%s", code, out)
	}
	if code, out := cli(t, "compare", base, rerun); code != exitOK {
		t.Errorf("rerun with another seed should not be a regression:\n%s", out)
	}
	code, out := cli(t, "compare", base, bad)
	if code != exitRegression || !strings.Contains(out, "REGRESSED") {
		t.Errorf("vague descriptions should be a regression, got %d:\n%s", code, out)
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"bogus"},
		{"run"},
		{"run", "--provider", "nope", suite},
		{"compare", "only-one.json"},
		{"validate", "missing.yaml"},
	} {
		if code, _ := cli(t, args...); code != exitError {
			t.Errorf("%v: got exit %d, want %d", args, code, exitError)
		}
	}
}
