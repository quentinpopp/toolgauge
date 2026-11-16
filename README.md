# toolgauge

[![CI](https://github.com/quentinpopp/toolgauge/actions/workflows/ci.yml/badge.svg)](https://github.com/quentinpopp/toolgauge/actions/workflows/ci.yml)

**toolgauge tests whether AI agents use their tools correctly.** You describe a
task and what should happen, and toolgauge runs it against a model many times,
checks the tool calls, and flags quality regressions in CI. Because language
models are non-deterministic, it uses statistics instead of single runs, so a
real regression is told apart from noise. It also lets you compare different
wordings of a tool description and see which one actually works best.

## Why

An agent integration can break without a single line of code changing: someone
rewords a tool description, adds a similar tool, or the model behind it gets
updated. Unit tests do not catch that, and a single manual check does not
either, because the same prompt can succeed on one run and fail on the next.

toolgauge treats tool use like any other behaviour worth testing:

- **Scenarios as data.** A YAML file lists the tools and, for each user
  request, the tool and arguments a correct agent would use.
- **Many runs per scenario.** Every scenario runs N times in parallel, and
  results are reported as pass rates with 95% confidence intervals.
- **Regressions that mean something.** `toolgauge compare` uses Fisher's exact
  test with Holm correction across scenarios, so a CI gate fails on real drops
  and not on sampling noise.
- **Failures by cause.** Wrong tool, missing call, call that should not have
  happened, and right tool with wrong arguments are counted separately,
  because each needs a different fix.

## Quick start

No API key needed: the built-in mock provider imitates a model whose choice
of tool depends on how well the descriptions match the request (see
[Design](docs/DESIGN.md#the-mock-provider)).

```bash
go install github.com/quentinpopp/toolgauge/cmd/toolgauge@latest
git clone https://github.com/quentinpopp/toolgauge && cd toolgauge

toolgauge run --out baseline.json examples/github-issues/suite.yaml
```

```
SCENARIO                  PASS     RATE  95% CI   EXAMPLE FAILURE
report-bug                14/20    70%   48–85%   wrong_tool: called close_issue, expected create_issue
feature-request           20/20    100%  84–100%
find-open-issues          18/20    90%   70–97%   bad_args: state=open-typo, want "open"
...
TOTAL                     183/200  92%   87–95%

failures: wrong_tool 3, bad_args 10, unexpected_call 4
```

Now simulate a careless refactor that makes the tool descriptions vague, and
compare against the baseline:

```bash
toolgauge run --descriptions examples/github-issues/descriptions-vague.yaml \
  --out current.json examples/github-issues/suite.yaml
toolgauge compare baseline.json current.json
```

```
note: tool definitions or system prompt differ from the baseline

SCENARIO                  BASELINE       CURRENT       DELTA    P (HOLM)  STATUS
report-bug                70% (14/20)    0% (0/20)     -70 pp   <0.001    REGRESSED
feature-request           100% (20/20)   10% (2/20)    -90 pp   <0.001    REGRESSED
find-open-issues          90% (18/20)    25% (5/20)    -65 pp   <0.001    REGRESSED
list-closed               100% (20/20)   0% (0/20)     -100 pp  <0.001    REGRESSED
close-fixed               95% (19/20)    85% (17/20)   -10 pp   1.000     unchanged
...
TOTAL                     92% (183/200)  43% (86/200)  -49 pp   <0.001    REGRESSED

regression detected
```

`compare` exits with 1, so a CI job fails. Running the unchanged suite again
with another seed (`--seed 7`) gives 181/200 instead of 183/200, and toolgauge
correctly reports that as noise (p = 0.43, no regression).

## Running against Cohere

```bash
export COHERE_API_KEY=...        # a free trial key works
toolgauge run --provider cohere --runs 10 --out cohere.json examples/github-issues/suite.yaml
```

```
cohere · model command-a-03-2025 · 100 trials · at most 20 requests/min, so up to 5m0s if nothing is cached
...
requests: 100 sent, 0 from cache · billed tokens: …
```

Trial keys allow 20 chat requests per minute and 1,000 per month, so the
runner is built to spend them carefully:

- **Pacing.** Requests are spaced to stay under `--rate-limit` (20/min by
  default for Cohere). A `429` is retried after the server's `Retry-After`.
- **Cache.** Responses are stored in `.toolgauge-cache/`, keyed by everything
  the model sees, so rerunning a baseline costs nothing and only changed tool
  descriptions are sent again. `--no-cache` turns it off.
- **Budget.** `--max-requests` stops the run before it exceeds a number of
  API calls, retries included, and saves the finished trials as an
  incomplete report.

`--model` picks another Cohere model, for example `command-r7b-12-2024`.

## Writing a suite

```yaml
name: github-issues
tools:
  - name: close_issue
    description: >
      Close an issue by number, optionally with a closing comment. Use this when
      an issue is fixed, resolved, a duplicate or no longer relevant.
    parameters:                     # JSON Schema, as in MCP and chat APIs
      type: object
      properties:
        repo: {type: string}
        number: {type: integer}
        comment: {type: string}
      required: [repo, number]

scenarios:
  - id: close-duplicate
    prompt: "Close acme/api issue 7, it is a duplicate of 3."
    expect:
      tool: close_issue
      args:
        repo: acme/api              # plain value: must be equal
        number: 7
        comment: {contains: duplicate}   # also: one_of, present

  - id: no-tool-smalltalk           # agents should not call tools needlessly
    prompt: "Thanks, that was helpful!"
    expect:
      no_tool: true
```

An argument written as a mapping is always a matcher, so a typo such as
`{contians: login}` is an error rather than a silent equality check. To expect
an object value, wrap it: `filter: {equals: {state: open}}`.

`toolgauge validate` checks a suite before you spend tokens on it: unknown
tools, arguments the tool does not have, duplicate IDs and typos in field
names or matchers are all reported at once.

## Commands

| Command | Purpose |
|---|---|
| `toolgauge validate <suite>` | Check a suite file |
| `toolgauge run [flags] <suite>` | Run every scenario `--runs` times (default 20) with `--concurrency` parallel requests, retries with backoff for transient errors and timeouts, and an optional `--out` JSON report |
| `toolgauge compare <baseline> <current>` | Per-scenario significance test; exit code 1 on regression |

`run --descriptions <file>` swaps in alternative tool descriptions without
copying the suite, which is how you compare wordings.

Reports record a fingerprint of each scenario and of the tool definitions.
`compare` marks a scenario whose prompt or expectation was edited as
`changed` instead of testing it, and notes when the tool definitions differ
from the baseline. If a run is interrupted (Ctrl-C), the trials that finished
are still written to `--out`, marked as incomplete.

Exit codes: `0` ok, `1` regression, `2` invalid input or internal error.

## Status and roadmap

This is an early version. It works end to end with the mock provider and
with Cohere.

- [x] Suite format, validation, matchers
- [x] Parallel runner with retries, timeouts, reproducible seeds
- [x] Pass rates with Wilson intervals, Fisher + Holm regression test
- [x] Cohere provider (Chat API v2 with tools), with pacing, response cache and request budget
- [ ] Description tuning: rank several wordings, with a held-out set against overfitting
- [ ] Multi-step scenarios checked against the end state of a mock API
- [ ] Anthropic and OpenAI providers
- [ ] Markdown report for pull request comments, GitHub Action

## How this was built

I used AI coding assistants while building toolgauge and reviewed every change
myself. Correctness rests on the test suite (run with `-race` in CI), and the
statistical reference values in the tests were checked independently.

## License

MIT
