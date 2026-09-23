# Contributing to agentevals-go

Thanks for your interest in contributing! This project is a from-scratch Go port of
[agentevals](https://github.com/agentevals-dev/agentevals) (Python) - see
[docs/STATUS.md](docs/STATUS.md) for exactly what's ported, what's a known simplification, and what's
intentionally out of scope before picking up work, so effort doesn't collide with something already tracked
there.

**Status: work in progress.** Core evaluation metrics, trace ingestion, and the UI are usable today, but the
API surface, auth model, and a few metrics are still incomplete (see
[docs/STATUS.md#not-yet-ported](docs/STATUS.md#not-yet-ported)). Expect breaking changes between releases.

## Ground rules

- **Fidelity first.** Every ported function should name the exact Python source it was ported from (file and
  function/class), so a future `google-adk`/`agentevals` upstream diff is a comparison, not an archaeology
  exercise. If you're porting new behavior, read the Python source first and mirror its logic - don't
  reinvent from the docstring or the tests alone.
- **No `litellm`, no Python subprocess.** Judge-model calls go through
  [`google.golang.org/genai`](https://pkg.go.dev/google.golang.org/genai) directly; Vertex AI Managed Eval
  calls go through `internal/vertexeval`. Don't add a dependency on the Python project or a language runtime
  other than Go.
- **Test what you change.** `judge.Model`/similar interfaces exist specifically so parsing/aggregation logic
  can be unit-tested against a scripted fake without a live API key. Add a test alongside any new metric,
  handler, or non-trivial bugfix.
- **Small, focused PRs.** Prefer one metric/endpoint/bugfix per PR over a large batch - easier to review,
  easier to bisect if something regresses.

## Development setup

```bash
go build -o bin/agentevals ./cmd/agentevals
go test ./...
go vet ./...
gofmt -l .        # should print nothing

cd ui && npm install && npm run build   # tsc + vite; required before `go build`/`make serve`
                                          # embeds ui/dist into the binary
```

`make serve` builds the UI and runs the full server (REST API, OTLP/HTTP+gRPC receivers, embedded UI) on
`:8001`/`:4318`/`:4317`. See the [README](README.md#quick-start) for CLI usage and env vars (judge API keys,
Vertex AI ADC, session persistence, GitHub OAuth).

## Before opening a PR

- [ ] `go build ./...`, `go vet ./...`, and `gofmt -l .` are clean.
- [ ] `go test ./...` passes (or any pre-existing unrelated failure is called out in the PR description).
- [ ] If you touched `ui/`: `npm run build` (tsc + vite) is clean.
- [ ] New/changed behavior has a test.
- [ ] If you ported something from Python, the comment names the source file/function.
- [ ] `README.md`'s Status section is updated if you ported something previously listed as "not yet ported",
      or newly discovered a gap.

## Reporting issues

Open a GitHub issue with: what you ran (CLI command or API request), what you expected, what happened
instead, and - for a scoring discrepancy - ideally the trace/eval-set file (redacted of anything sensitive)
that reproduces it.

## License

By contributing, you agree that your contributions will be licensed under the [Apache License 2.0](LICENSE),
the same license as the rest of this project.
