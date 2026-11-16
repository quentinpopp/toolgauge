// Command toolgauge tests whether AI agents use their tools correctly.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	"github.com/quentinpopp/toolgauge/internal/provider"
	"github.com/quentinpopp/toolgauge/internal/provider/cache"
	"github.com/quentinpopp/toolgauge/internal/provider/cohere"
	"github.com/quentinpopp/toolgauge/internal/provider/limit"
	"github.com/quentinpopp/toolgauge/internal/provider/mock"
	"github.com/quentinpopp/toolgauge/internal/report"
	"github.com/quentinpopp/toolgauge/internal/runner"
	"github.com/quentinpopp/toolgauge/internal/spec"
)

// Exit codes. CI treats 1 as "quality got worse" and 2 as "toolgauge or its
// input is broken", so the two can be told apart.
const (
	exitOK         = 0
	exitRegression = 1
	exitError      = 2
)

var version = "dev"

const usage = `toolgauge tests whether AI agents use their tools correctly.

Usage:
  toolgauge validate <suite.yaml>
  toolgauge run [flags] <suite.yaml>
  toolgauge compare [flags] <baseline.json> <current.json>
  toolgauge version

Run "toolgauge <command> -h" for the flags of a command.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitError
	}
	var err error
	code := exitOK
	switch args[0] {
	case "validate":
		err = cmdValidate(args[1:], stdout, stderr)
	case "run":
		err = cmdRun(ctx, args[1:], stdout, stderr)
	case "compare":
		code, err = cmdCompare(args[1:], stdout, stderr)
	case "version":
		fmt.Fprintln(stdout, version)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
	default:
		err = fmt.Errorf("unknown command %q\n\n%s", args[0], usage)
	}
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stderr, "error:", err)
		}
		return exitError
	}
	return code
}

func cmdValidate(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("validate needs exactly one suite file")
	}
	s, err := spec.Load(fs.Arg(0))
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "ok: %s, %d tools, %d scenarios\n", s.Name, len(s.Tools), len(s.Scenarios))
	return nil
}

func cmdRun(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		cfg          runner.Config
		providerName = fs.String("provider", "mock", "model provider: mock or cohere")
		descriptions = fs.String("descriptions", "", "YAML file mapping tool names to alternative descriptions")
		out          = fs.String("out", "", "write the full report as JSON to this file")
		rateLimit    = fs.Float64("rate-limit", -1, "max requests per minute (default: 20 for cohere, none for mock; 0 disables)")
		maxRequests  = fs.Int("max-requests", 0, "stop after this many API requests, retries included (0: no limit)")
		cacheDir     = fs.String("cache-dir", ".toolgauge-cache", "cache responses of real providers here")
		noCache      = fs.Bool("no-cache", false, "always call the provider, ignoring the cache")
		mockCfg      = mock.DefaultConfig()
	)
	fs.StringVar(&cfg.Model, "model", "", "model name passed to the provider")
	fs.IntVar(&cfg.Runs, "runs", 20, "repetitions per scenario")
	fs.IntVar(&cfg.Concurrency, "concurrency", 8, "parallel requests")
	fs.DurationVar(&cfg.Timeout, "timeout", 60*time.Second, "timeout per attempt")
	fs.IntVar(&cfg.MaxRetries, "retries", 3, "retries for retryable errors and timeouts")
	fs.Uint64Var(&cfg.Seed, "seed", 1, "seed for reproducible runs")
	fs.Float64Var(&mockCfg.Temperature, "mock-temperature", mockCfg.Temperature, "mock: softmax temperature")
	fs.Float64Var(&mockCfg.ArgErrorRate, "mock-arg-error-rate", mockCfg.ArgErrorRate, "mock: chance each argument is wrong")
	fs.Float64Var(&mockCfg.FailureRate, "mock-failure-rate", mockCfg.FailureRate, "mock: chance a request fails transiently")
	fs.DurationVar(&mockCfg.Latency, "mock-latency", 0, "mock: simulated latency per request")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("run needs exactly one suite file")
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if *maxRequests < 0 {
		return errors.New("max-requests must not be negative")
	}
	s, err := spec.Load(fs.Arg(0))
	if err != nil {
		return err
	}
	if *descriptions != "" {
		d, err := spec.LoadDescriptions(*descriptions)
		if err != nil {
			return err
		}
		if s, err = s.WithDescriptions(d); err != nil {
			return err
		}
	}

	// The provider stack, from the outside in: cache, then rate limit and
	// budget, then the API. Cached answers skip the limits and cost nothing.
	var base provider.Provider
	perMinute := *rateLimit
	switch *providerName {
	case "mock":
		base = mock.New(mockCfg)
		if perMinute < 0 {
			perMinute = 0
		}
	case "cohere":
		c, err := cohere.New(cohere.Config{APIKey: os.Getenv("COHERE_API_KEY")})
		if err != nil {
			return err
		}
		base = c
		if cfg.Model == "" {
			cfg.Model = cohere.DefaultModel
		}
		if perMinute < 0 {
			perMinute = 20 // Cohere's trial limit for chat models
		}
	default:
		return fmt.Errorf("unknown provider %q (available: mock, cohere)", *providerName)
	}
	limited := limit.Wrap(base, perMinute, *maxRequests)
	var p provider.Provider = limited
	var cached *cache.Provider
	if *providerName != "mock" && !*noCache && *cacheDir != "" {
		if cached, err = cache.Wrap(limited, *cacheDir); err != nil {
			return err
		}
		p = cached
	}

	planned := len(s.Scenarios) * cfg.Runs
	if *providerName != "mock" {
		fmt.Fprintf(stdout, "%s · model %s · %d trials", p.Name(), cfg.Model, planned)
		if perMinute > 0 {
			fmt.Fprintf(stdout, " · at most %.0f requests/min, so up to %s if nothing is cached",
				perMinute, (time.Duration(float64(planned) / perMinute * float64(time.Minute))).Round(time.Second))
		}
		fmt.Fprint(stdout, "\n\n")
	}

	trials, runErr := runner.Run(ctx, p, s, cfg)
	stopped := ctx.Err() != nil || errors.Is(runErr, provider.ErrBudgetExhausted)
	if runErr != nil && !stopped {
		return runErr
	}
	// A stopped run still produces a report of the trials that finished,
	// so tokens already paid for are not thrown away.
	r := &report.Report{
		Suite: s.Name, Provider: p.Name(), Model: cfg.Model, Runs: cfg.Runs,
		Seed: cfg.Seed, CreatedAt: time.Now().UTC(), Trials: trials,
		Incomplete: runErr != nil, Requests: limited.Sent(),
	}
	if cached != nil {
		r.CacheHits, _ = cached.Stats()
	}
	r.Total, r.Scenarios = report.Build(trials)
	r.AddFingerprints(s)
	r.WriteText(stdout)
	if *out != "" {
		if err := r.Save(*out); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "\nreport written to %s\n", *out)
	}
	if runErr != nil {
		return fmt.Errorf("run stopped after %d of %d trials: %w", len(trials), planned, runErr)
	}
	return nil
}

func cmdCompare(args []string, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var opt report.CompareOptions
	fs.Float64Var(&opt.Alpha, "alpha", 0.05, "significance level after Holm correction")
	fs.Float64Var(&opt.MinDelta, "min-delta", 0.05, "ignore changes smaller than this fraction")
	if err := fs.Parse(args); err != nil {
		return exitError, err
	}
	if fs.NArg() != 2 {
		return exitError, errors.New("compare needs a baseline and a current report")
	}
	base, err := report.Load(fs.Arg(0))
	if err != nil {
		return exitError, err
	}
	cur, err := report.Load(fs.Arg(1))
	if err != nil {
		return exitError, err
	}
	if base.Suite != cur.Suite {
		fmt.Fprintf(stderr, "warning: comparing different suites (%s vs %s)\n", base.Suite, cur.Suite)
	}
	c := report.Compare(base, cur, opt)
	c.WriteText(stdout)
	if c.Regressed() {
		fmt.Fprintln(stdout, "\nregression detected")
		return exitRegression, nil
	}
	fmt.Fprintln(stdout, "\nno regression")
	return exitOK, nil
}
