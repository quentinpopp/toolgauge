# Design

This document explains the decisions behind toolgauge and the trade-offs that
came with them.

## Goal

Answer one question reliably: *did this change make the agent worse at using
its tools?* "This change" can be a new tool description, an added tool, a new
model version or a different system prompt.

## Architecture

```
suite.yaml ──► spec ──► runner ──► provider (mock | cohere | …)
                          │
                          ▼
                       grader ──► report ──► compare ──► exit code
                                     │
                                     ▼
                                report.json (baseline)
```

| Package | Responsibility |
|---|---|
| `spec` | Suite format, matchers, validation |
| `provider` | Interface to a model API; `mock` is a test double, `cohere` calls the Chat API |
| `provider/limit`, `provider/cache` | Wrappers: pacing and budget, response cache |
| `runner` | Worker pool, retries, timeouts, seeds |
| `grader` | One response against one expectation, failure by cause |
| `stats` | Wilson interval, Fisher exact test, Holm correction |
| `report` | Summaries, JSON baseline, comparison |

Dependencies point in one direction only, and everything a
provider needs to know is in `provider.Request`, so adding a provider does not
touch the runner.

## Decisions

### Repeated trials instead of one run per scenario

A model can pick the right tool 9 times out of 10. One run tells you nothing
about that 10th time, and comparing two single runs mostly compares noise.
Every scenario therefore runs N times (default 20), and results are pass
rates.

*Trade-off:* N times the cost and time. Mitigations: high concurrency, the
mock provider for developing suites, and (planned) a response cache keyed by
request so unchanged scenarios are not paid for twice.

### Wilson intervals

Pass rates near 0% and 100% are common in evals, and the textbook interval
p ± 1.96·SE breaks down there (it gives 100% ± 0 for 20/20). Wilson intervals
stay within [0, 1] and are honest about small samples: 20/20 is reported as
84–100%, which is the right amount of humility for 20 trials.

### Fisher's exact test, one-sided

With 10 to 50 trials per scenario, normal approximations of the difference
between two proportions are poor. Fisher's exact test has no such problem and
is cheap at these sizes. The test is one-sided in the direction of the
observed change, because the question CI asks is "did it get worse?".

### Holm correction across scenarios

A suite with 40 scenarios tested at α = 0.05 would flag about two
"regressions" per run from noise alone, and a CI gate that cries wolf gets
switched off. The Holm-Bonferroni method controls the family-wise error rate
and is uniformly more powerful than plain Bonferroni.

*Trade-off:* a real but small drop in one scenario can go unflagged in a big
suite. The total pass rate is tested separately (uncorrected), which catches
broad small drops; for single critical scenarios, raise `--runs`.

### Minimum effect size

With enough trials, even a 99% → 98% change is significant. `--min-delta`
(default 5 percentage points) keeps CI from failing on changes too small to
matter. Statistical significance and practical significance are separate
questions, and both have to say yes.

### Failures classified by cause

`wrong_tool`, `no_call`, `unexpected_call` and `bad_args` point to different
fixes: the first three usually to descriptions or the tool set, the last to
parameter schemas or descriptions. A single pass/fail number would hide that.

### Retries: only for retryable errors and timeouts

Rate limits, 5xx responses and timeouts are retried with exponential backoff
and jitter; a 400 is not, because it will fail the same way again and retrying
only hides a broken request. Providers mark errors with
`provider.Retryable(err)`. The timeout applies per attempt, so one slow
request cannot use up the whole budget. Each retry uses a new seed: a retry
is a fresh sample, not a replay. The backoff doubles up to a cap (30 s by
default) and stops doubling there, so a high retry count neither waits for
hours nor overflows.

### Reproducibility

Each trial gets a seed derived from the run seed, the scenario ID and the
trial index (FNV-1a hash plus SplitMix64 mixing). Results do not depend on
which worker ran a trial or in what order, so the same command gives the same
report, and trials are stored in a fixed order however they completed.

### The mock provider

To build and test the harness without paying for a model, and to let anyone
try toolgauge without an API key, there is a mock provider. It is not a
language model, but it reacts to the same input that matters for tool
selection: it scores each tool by word overlap between the prompt and the
tool's name, description and parameter descriptions, and samples from a
softmax over those scores. Clear descriptions win reliably and vague ones do
not, so description changes move results in the direction a real model would.

When it picks the right tool, it takes the arguments from `Request.Oracle`
(the expected call), with a configurable error rate. Real providers never read
the oracle. This keeps the mock honest about what it is: a way to exercise the
runner, the grader and the statistics, not a stand-in for model quality.

Building the example suite against the mock already turned up a real-world
problem: an example value (`e.g. acme/web`) in one parameter description
pulled unrelated requests towards that tool. That is the kind of issue
toolgauge exists to find.

*Limitation:* word overlap does not understand negation. "Do not use this to
search" counts as a match for "search". Real models do understand it, so do
not tune descriptions for the mock.

### Strict suite parsing

Unknown fields are errors (`promt:` instead of `prompt:` fails loudly), and
validation reports every problem at once. A typo that silently skips an
expectation would make a suite pass for the wrong reason.

The same applies to matchers: an argument written as a mapping must be
exactly one of `equals`, `contains`, `one_of` or `present`. An earlier
version treated any other mapping as a literal value to compare against, so
`{contians: login}` became an equality check that failed every trial and
looked like a model problem. Object values now need an explicit `equals`.

### Fingerprints: comparing like with like

A pass rate only means something together with the test that produced it.
Each report stores a hash of every scenario's prompt and expectation, and a
separate hash of the system prompt and tool definitions. `compare` uses them
in two different ways:

- A scenario whose fingerprint changed is reported as `changed` and left out
  of the statistics and the total, because its baseline measured something
  else.
- Changed tool definitions are only noted, not rejected: comparing two
  versions of the tools is the main reason to run `compare`.

### Interrupted runs

Against a real API, trials cost money. If a run is cancelled, the runner
stops sending requests (also during a retry backoff), drops trials that were
cut off mid-request, and returns the ones that finished. The CLI writes them
as a report marked `incomplete`, and `compare` warns about it.

### Real providers: pacing, cache and budget as wrappers

A Cohere trial key allows 20 chat requests per minute and 1,000 per month.
One default run of the example suite is 200 requests, a fifth of the month.
Three wrappers keep that under control, stacked around the provider in this
order:

```
cache ──► limit (pacing + budget) ──► cohere
```

- The **cache** is outermost, so a cached answer neither waits for a rate
  limit slot nor counts against the budget. Its key covers everything the
  model sees (model, system prompt, prompt, tools, seed) and nothing else:
  the mock's oracle is left out, because real models never receive it.
  Entries are written to a temp file and renamed, so parallel workers and
  crashes cannot leave half-written files. Errors are never cached.
- **Pacing** spaces requests evenly instead of letting them out in bursts,
  because providers measure per-minute limits over sliding windows. The wait
  for a slot counts against the attempt's timeout, so with pacing on keep
  `--concurrency` × interval well below `--timeout`.
- The **budget** counts retries too, because the API does. When it runs out,
  the runner stops the run like a cancellation: finished trials are kept,
  the refused one is not recorded as a model error.

Doing this as wrappers rather than inside the runner keeps the runner free of
provider-specific limits and lets each piece be tested alone.

A `429` carries a `Retry-After` header; the provider passes it on with
`provider.RetryableAfter` and the runner waits at least that long.

### Cohere adapter

The adapter maps a scenario to one `POST /v2/chat` with a system message, a
user message and the tools as `{"type": "function", ...}`. The shapes were
checked against the live API before writing it. Tool call arguments arrive
as a JSON string; if the model produces invalid JSON there, the call is kept
with no arguments so the grader reports `bad_args`. That is a model error,
not a transport error, and must not be retried away. Token counts use
`billed_units`, which is what the account is charged.

## Not in scope (yet)

- **Multi-step agents.** Only the first tool call is graded. Multi-step
  scenarios need a mock API with state to check the end result, which is the
  next big step.
- **Answer quality.** Whether the final text answer is faithful to the tool
  result is a separate kind of eval.
- **Storage beyond JSON files.** A baseline is a file you commit or keep as a
  CI artifact. That is enough until history across many runs is needed.
