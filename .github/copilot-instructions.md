# agentevals-go

A from-scratch Go rewrite of the Python [agentevals](https://github.com/agentevals-dev/agentevals) project:
scores AI agent behavior from OpenTelemetry traces (no re-execution, no Python, no `litellm`). This is an
in-progress port - **README.md's "Status" section is the source of truth** for what's ported, what has a
known fidelity gap, and what's not started; check it before assuming a feature exists or matches Python
behavior exactly.

## Build, test, lint

```bash
go build -o bin/agentevals ./cmd/agentevals   # or: make build
go test ./...                                  # or: make test
go test ./internal/eval/... -run TestRouge1FMeasure -v   # single test by name (works at any package path)
go vet ./...                                    # or: make vet
gofmt -l .                                       # or: make fmt (lists unformatted files, doesn't rewrite)
```

UI (React + TypeScript + Vite, in `ui/`, copied unmodified from the Python project):
```bash
cd ui && npm ci && npm run build   # or: make build-ui
cd ui && npx tsc --noEmit           # type check only
cd ui && npm run dev                 # dev server on :5173, calls backend at :8001 directly (no proxy)
```

Run a scored example end-to-end: `make run ARGS="samples/helm.json --eval-set samples/eval_set_helm.json -m tool_trajectory_avg_score"`.
Run the server: `make serve` (builds UI, serves REST API + UI on `:8001`, OTLP/HTTP on `:4318`, OTLP/gRPC on `:4317`).

No CI config or linter config exists in-repo; `go vet`/`gofmt`/`go test` above are what's actually enforced.

## Architecture

Pipeline: **trace loading → ADK conversion → metric evaluation**, plus a parallel **live ingestion → streaming**
path for the UI.

```
cmd/agentevals/       CLI entrypoint (run, serve subcommands)
internal/trace/        normalized Span/Trace model + Jaeger JSON loader
internal/adk/           ADK Invocation/EvalSet data model, attribute extraction, ADK trace -> Invocation converter
internal/eval/           local built-in metrics (trajectory, ROUGE-1), eval set loading, metric dispatch
internal/judge/           LLM-as-judge metrics via google.golang.org/genai (final_response_match_v2)
internal/otlp/             AnyValue decoding, protobuf<->JSON bridge, OTLP export -> Span/Trace conversion
internal/incremental/       real-time span/log -> conversation-element extraction, feeds the SSE hub
internal/api/                 REST/UI server, OTLP/HTTP+gRPC receivers, session store (in-memory + optional SQLite), SSE hub
ui/                              React UI, copied verbatim from agentevals/ui - re-synced wholesale, not hand-edited
samples/                          sample traces + eval sets, copied from the Python project
```

Key flows:
- **Offline `run`**: `trace.LoadJaegerJSON` → `adk.ConvertADKTrace` (only the `gcp.vertex.agent.*` ADK attribute
  path is supported; the GenAI-semconv/non-ADK converter isn't ported) → `eval.RunMetrics` for local metrics or
  `judge.FinalResponseMatchV2` for judge metrics (dispatch split lives in `cmd/agentevals/main.go`'s
  `judgeMetrics` map, since local metrics deliberately have no judge/API-key dependency).
- **Live `serve`**: OTLP/HTTP (`:4318`) and OTLP/gRPC (`:4317`) receivers both feed the *same*
  `internal/api` `SessionStore` and conversion path - don't special-case one over the other. Sessions group by
  `agentevals.session_name` → `gen_ai.conversation.id` → synthetic-name fallback. The live UI feed
  (`ui/src/components/streaming/LiveStreamingView.tsx`) connects over `GET /ws/ui-updates` (WebSocket,
  `internal/api/wsupdates.go`), not SSE - it was switched from `EventSource` because the cluster's gateway
  buffers/never forwards long-lived SSE responses; `GET /stream/ui-updates` (SSE, `internal/api/sse.go`,
  same underlying `sseHub`) still exists as a curl-able fallback and is what Python's equivalent uses. Both
  are distinct from `/ws/traces`, a separate, not-yet-ported SDK ingestion channel - easy to confuse with
  `/ws/ui-updates`.
- Session persistence (`internal/api/sqlitestore.go`) stores one SQLite row per session as a whole-session JSON
  blob (adding a `Session` field never needs a migration) - opt-in via `serve --session-db` /
  `AGENTEVALS_SESSION_DB_PATH`.
- Everything in `serve` is unauthenticated except when `--session-secret`/`AGENTEVALS_SESSION_SECRET` is set
  (validates the same signed cookie the Python project's GitHub OAuth sets - no OAuth flow is implemented
  here). The OTLP receivers and `/api/health` (own port, `:9090`) are never gated.

## Conventions

- **Every ported function/file has a comment naming its exact Python source** (e.g. "Ported from `converter.py`'s
  `_convert_adk_trace`"). When modifying ported logic, preserve or update this comment and keep behavior
  faithful to the named Python source rather than reinventing it - re-verifying fidelity against a
  `google-adk`/agentevals upgrade should stay a diff against Python, not an archaeology exercise.
- New unported metrics should be added to `eval.NotYetImplemented` (or `judgeMetrics` in `main.go`, if they need
  a judge model) so they fail with a clear "not yet ported" error instead of silently scoring 0.
- Judge/LLM metrics go through the `judge.Model` interface (see `internal/judge/client.go`) so parsing/
  aggregation logic can be unit-tested against a scripted fake without a live API key.
- `ui/` is synced wholesale from the Python project's `agentevals/ui` - don't hand-edit it piecemeal; treat
  backend changes there as a re-sync, not an incremental port.
- No CGO: `modernc.org/sqlite` is used specifically to keep the binary CGO-free.
