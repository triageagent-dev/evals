# Status

> Full porting detail moved out of the main [README](../README.md) to keep it short. This is the
> complete picture: what's ported and working, one known fidelity gap, everything intentionally not
> yet ported (and why), and a full file-by-file module inventory against the upstream Python project.

## Ported and working

- **Jaeger JSON trace loading** (`internal/trace`) - full parity with `loader/jaeger.py`.
- **ADK-format trace → Invocation conversion** (`internal/adk`) - the `gcp.vertex.agent.*` attribute path
  (`converter.py`'s `_convert_adk_trace`), including the ADK-first/GenAI-semconv-fallback attribute extraction
  helpers from `extraction.py` (`extract_user_text_from_attrs`, `extract_agent_response_from_attrs`,
  `extract_tool_call_from_attrs`, `extract_tool_result_from_attrs`).
- **GenAI-semconv / OpenInference trace → Invocation conversion** (`internal/adk/genai_converter.go`) - format
  auto-detection (`ConvertTrace`) plus the non-ADK batch converter (`genai_converter.py`'s
  `convert_genai_trace`), including the OpenInference-turn pairing path (`_extract_openinference_turns`) that
  realtime/voice producers without `gen_ai.input.messages` arrays need (e.g. triage-core's Gemini Live voice
  sessions). Wired into session completion (`internal/api/sessions.go`'s `completeSession`, populating real
  `invocations` in `GET /api/streaming/sessions` and the `session_complete` SSE event) and into
  `POST /api/streaming/create-eval-set` (`internal/api/server.go`).
- **`tool_trajectory_avg_score`** (`internal/eval/trajectory.go`) - exact port of
  `google.adk.evaluation.trajectory_evaluator.TrajectoryEvaluator`: EXACT / IN_ORDER / ANY_ORDER matching.
- **`response_match_score`** (`internal/eval/rouge.go`) - ROUGE-1 F-measure, same shape as
  `google.adk.evaluation.final_response_match_v1.RougeEvaluator`, with one fidelity gap (below).
- **Eval set loading + expected-invocation matching** (`internal/eval/evalset.go`) - exact port of
  `runner.py`'s `_find_expected_invocations` (single eval case used unconditionally; multiple eval cases matched
  by first-turn user text).
- **CLI** (`cmd/agentevals`) - `run` (multi-trace, multi-metric, `--trajectory-match-type`, `--threshold`,
  `--output text|json`, non-zero exit on any failure/error) and `serve` (health check + static UI + OTLP receiver).
- **OTLP/HTTP trace ingestion** (`internal/otlp`, `internal/api/sessions.go`) - `POST /v1/traces` on a separate
  port (`:4318` by default, matching the Python project's port layout), accepting both OTLP/JSON and
  `application/x-protobuf` (via `go.opentelemetry.io/proto/otlp` + `protojson`, mirroring the
  `ParseFromString` + `MessageToDict` bridge in `otlp_processing.py`'s `decode_protobuf_traces`). The `AnyValue`
  decoder (`otlp_anyvalue.py`), the OTLP/JSON→Span converter (`loader/otlp.py`), and session grouping by
  `agentevals.session_name` → `gen_ai.conversation.id` → synthetic-name fallback
  (`ws_server.py`'s `get_or_create_otlp_session`) are all ported. Read back via `GET /api/streaming/sessions`
  (the `{"data": [...]}` envelope + camelCase `SessionInfo` field shape the UI's `LiveStreamingView.tsx`
  actually expects, not a guessed shape) and `GET /api/streaming/get-trace?session_id=`.
- **OTLP/gRPC trace ingestion** (`internal/api/grpc.go`) - `:4317` by default, a `TraceServiceServer`
  implementation (`go.opentelemetry.io/proto/otlp/collector/trace/v1`) sharing the exact same `SessionStore`
  and conversion path (`otlp.ExportRequestToMap` → `ParseExportRequest`) as the HTTP receiver - grpc hands
  `Export` an already-decoded `*ExportTraceServiceRequest`, so there's no separate protobuf-unmarshal step the
  way the HTTP receiver needs one for `application/x-protobuf` bodies. Covers gRPC-only OTel exporters, which
  the HTTP-only receiver couldn't accept before. Verified with a real listener + the standard collector gRPC
  client, not just an in-process call (`internal/api/grpc_test.go`).
- **Live SSE push to the UI** (`internal/incremental`, `internal/api/sse.go`) - `GET /stream/ui-updates`, the
  endpoint `ui/src/components/streaming/LiveStreamingView.tsx`'s `EventSource` actually connects to (the
  `/ws/traces` WebSocket in the Python project is a *separate*, not-yet-ported ingestion channel for the
  AgentEvals SDK, not a UI output - easy to mix up, see the comment on `internal/api/sse.go`). Broadcasts
  `session_started`, `span_received`, and, per span, the conversation-element updates from a faithful port of
  `streaming/incremental_processor.py`'s `IncrementalInvocationExtractor` (`user_input`, `agent_response`,
  `tool_call`, `tool_result`, `token_update`, with the same dedup behavior). Verified against a real HTTP/SSE
  round trip, not just unit tests.
- **`final_response_match_v2`** (`internal/judge`) - the first LLM-as-judge metric, via
  [`google.golang.org/genai`](https://pkg.go.dev/google.golang.org/genai) (no `litellm`, no Python). The prompt
  template (`_FINAL_RESPONSE_MATCH_V2_PROMPT`), the `_parse_critique` regex-based label extraction, and the
  majority-vote-over-`num_samples`-then-average-fraction-valid aggregation are all ported verbatim from
  `final_response_match_v2.py` / `llm_as_judge.py`. Defaults match ADK's `JudgeModelOptions`: model
  `gemini-2.5-flash`, 5 samples per invocation. `judge.Model` is an interface so the parsing/aggregation logic
  is unit-tested against a scripted fake - this environment has no `GEMINI_API_KEY`, so a live call hasn't been
  exercised end-to-end, only client construction's fail-fast error path.
- **Vertex AI / Application Default Credentials auth for the judge model** (`internal/judge/client.go`'s
  `NewGenAIModel`) - an empty `apiKey` no longer forces the Gemini Developer API backend; it leaves
  `genai.ClientConfig.Backend` unspecified so the SDK's own env-based detection picks Vertex AI + ADC when
  `GOOGLE_GENAI_USE_VERTEXAI` is set (matching ADK Python's default `Gemini` model class, which does the same
  thing one layer up - see `builtin_metrics.py`'s `_build_judge_model`). An explicit `apiKey` (CLI's
  `--judge-api-key`) still forces the Gemini Developer API backend. `GET /api/config`'s `apiKeys.google` also
  reports `true` in Vertex AI mode, not just on a literal API key env var, so the UI stops red-flagging a judge
  model that actually works - `deploy/k8s.yaml.example` wires this up (see above).
- **`POST /api/evaluate`** (`internal/api/evaluate.go`) - ported from `api/routes.py`'s `evaluate_traces` /
  `runner.py`'s `run_evaluation`: multipart upload of one or more trace files (Jaeger or OTLP JSON/JSONL,
  auto-detected - `internal/loader`, ported from `loader/auto.py`) plus an optional golden eval set file,
  scored per the `evaluators` list in the `config` form field (`ui/src/lib/types.ts`'s `EvalConfig`, plus an
  additive `rubrics []string` field for the two rubric-based metrics - not yet surfaced in the UI itself, see
  below). Judge metrics (`final_response_match_v2`, `hallucinations_v1`, `rubric_based_final_response_quality_v1`,
  `rubric_based_tool_use_quality_v1`), Vertex-eval metrics (`safety_v1`, `multi_turn_task_success_v1`,
  `multi_turn_trajectory_quality_v1`, `multi_turn_tool_use_quality_v1`), and local metrics all dispatch through
  the same evaluator list - one judge model client per distinct `judgeModel` name, and a single shared Vertex
  eval client (Application Default Credentials only, built lazily and only if a Vertex-eval metric is actually
  requested). Not ported: `credential_refs` (per-request credential injection - this port's judge/Vertex auth is
  environment-wide, see above), and `RunResult.performanceMetrics` (token/latency aggregation - `trace_metrics.py`,
  still not ported, see below).
- **`POST /api/evaluate/stream`** (`internal/api/evaluate.go`'s `evaluateStreamHandler`) - the SSE-progress
  variant of the above, ported from `api/routes.py`'s `evaluate_traces_stream`. This is the endpoint the UI
  actually calls (`ui/src/context/TraceProvider.tsx` always uses `evaluateTracesStreaming`, never the plain
  `POST /api/evaluate`) - without it the "Prepare Evaluation" flow 404'd outright. Emits `message` (progress),
  `traceProgress` (accumulating per-trace `metricResults`, one event per completed metric - mirrors
  `runner.py`'s `_evaluate_trace`'s `trace_progress_callback`, called incrementally, not once per trace), and a
  final `done` event carrying the same `RunResult` shape as `POST /api/evaluate`. A single evaluation runs in
  its own goroutine while the handler's main loop also selects on a 15s ticker emitting an SSE `: ping` comment
  - matching Python's identical idle-timeout rationale (a live judge-model call can take 30-40s+, and an
  intermediate proxy buffering an idle streaming response can otherwise silently swallow the eventual `done`
  event even though the run succeeded server-side). Not ported: the early best-effort `performance_metrics`
  event (`trace_metrics.py`'s `extract_performance_metrics`/`extract_trace_metadata`, see below - the UI
  only uses this event for a "loading" preview and unconditionally derives its final table from `done`, so
  its absence doesn't block correctness, only that early preview).
- **`POST /api/streaming/get-trace`** (`internal/api/server.go`) - now matches the UI's actual contract
  (`streaming_routes.py`'s `GetTraceData`): `POST` with a `{session_id}` JSON body, returning
  `{traceContent, numSpans}` - OTLP JSONL text a session's spans are re-encoded into
  (`internal/otlp/jsonl.go`'s `EncodeTraceJSONL`, the inverse of the OTLP JSONL parser `/api/evaluate` uses),
  the same shape `LiveStreamingView.tsx`/`AnnotationQueueView.tsx` blob straight into a re-uploaded `.jsonl`
  file. Previously this returned an unrelated ad-hoc span-tree shape under `GET ?session_id=`, so every
  trace file in the "Prepare Evaluation" flow was silently empty/`undefined` before this fix.
- **Independent GitHub OAuth login** (`internal/api/oauth.go`) - `/auth/login` -> GitHub -> `/auth/callback`
  verifies `AGENTEVALS_GITHUB_ORG` membership (only an HTTP 204 from GitHub's membership API counts; a
  302/404 both deny) and mints the same signed `agentevals_session` cookie `/auth/me`/`requireSession`
  already validated, `/auth/logout` clears it. Depends on no other service to be reachable - set
  `AGENTEVALS_GITHUB_CLIENT_ID`/`_SECRET`, `AGENTEVALS_GITHUB_ORG`, `AGENTEVALS_PUBLIC_URL` and
  `AGENTEVALS_SESSION_SECRET` (or the matching `--github-client-id` etc. flags) to enable it; with none of
  those set, every endpoint is unauthenticated - don't expose `serve` beyond localhost/a trusted network in
  that mode.
- **`agentevals auth mint-token`** (`cmd/agentevals/auth.go`, `internal/api/oauth.go`'s exported
  `MintSessionToken`) - a signed, long-lived (default 3650-day) token for a non-browser client, matching
  `cli.py`'s `auth mint-token` flags/behavior exactly. `requireSession` already accepted this token format as
  an `Authorization` header alternative to the session cookie before this change (this port's own addition,
  not upstream's - Python's service-to-service bearer-token fallback only guards its in-process MCP mount,
  see below); this just adds a CLI command to mint one.
- **GitHub-token bearer auth** (`internal/api/githubtoken.go`) - `requireSession`/`extractSessionUsername`
  additionally accept a live GitHub access token as the `Authorization: Bearer` value, validated against
  GitHub's `/user` + org-membership APIs (the same two calls `/auth/callback` already makes) and cached for
  5 minutes to bound the added GitHub API traffic. Additive over Python (no equivalent there): a non-browser
  client with no agentevals-minted token of its own - most notably `agentevals mcp` - can authenticate with
  whatever GitHub token it already has, e.g. `gh auth token`'s output, instead of needing
  `agentevals auth mint-token` at all.
- **MCP server** (`cmd/agentevals/mcp.go`, `agentevals mcp`) - a stdio Model Context Protocol server (via
  [`github.com/mark3labs/mcp-go`](https://pkg.go.dev/github.com/mark3labs/mcp-go)), ported from
  `mcp_server.py`'s `create_server`: `list_metrics`, `evaluate_traces` (fully offline - loads trace files
  from disk and scores them in-process, no server needed), `list_sessions`, `summarize_session`. Tool and
  field names are Python's exact snake_case (`trace_id`, `is_complete`, ...), a deliberate exception to the
  rest of this port's camelCase REST API, since MCP tool schemas are a separate contract for LLM
  tool-calling clients trained on the Python shape. `list_sessions`/`summarize_session` call a running
  `agentevals serve` over HTTP (`--server-url`/`AGENTEVALS_SERVER_URL`) and authenticate against a
  GitHub-OAuth-gated deployment non-interactively via, in order: `--session-token`/
  `AGENTEVALS_SESSION_TOKEN` if set, else `gh auth token`'s output if `gh` is installed and logged in (see
  "GitHub-token bearer auth" above) - an enhancement over Python's stdio tools, which send no auth header at
  all. Also additive over Python (no equivalent there, since Python's `mcp_server.py` has no run-history
  persistence to expose at all): `list_runs`/`get_run_results`, which surface this port's own run-history
  storage (`internal/api/runs.go`'s `GET /api/runs`/`GET /api/runs/{id}/results`, so they also require
  `serve --session-db`/`AGENTEVALS_SESSION_DB_PATH`) - `list_runs` deliberately omits each run's full
  eval-set/eval-config blob (potentially megabytes of raw trace/conversation data) from its summary, since
  it's meant for discovering run IDs and pass/fail counts, not for re-fetching traces. Not ported:
  `evaluate_sessions` (its backing endpoint, `POST /api/streaming/evaluate-sessions`, isn't implemented in
  this port at all - see below) and the `eval_config_file` parameter on `evaluate_traces` (no
  `eval_config.yaml` loader exists in this port yet).
- **Session persistence** (`internal/api/sqlitestore.go`) - ported from `storage/session_store.py`'s
  `SqliteSessionStore`: one SQLite row per session, the whole session serialized as a JSON blob (so a field
  added to `Session` never needs a migration), WAL mode, restore-at-startup + a 10s periodic snapshot (matching
  `ws_server.py`'s `persist_interval_seconds` default) + a final flush on `SIGINT`/`SIGTERM`. Uses
  [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite) (pure Go, no CGO). Opt-in via `serve
  --session-db path` / `AGENTEVALS_SESSION_DB_PATH`, same as the Python CLI; omitted, sessions stay in-memory
  only as before. Verified with an actual subprocess kill + restart against the same file, not just
  same-process round-tripping.

- **`hallucinations_v1`** (`internal/judge/hallucinations_v1.go`) - two-stage judge pipeline ported verbatim
  from `google.adk.evaluation.hallucinations_v1`: a segmenter prompt splits the final response into sentences,
  then a validator prompt classifies each sentence as `supported`/`unsupported`/`contradictory`/`disputed`/
  `not_applicable` against a context string (developer instructions + user prompt + tool definitions). Both
  prompt templates and both response-parsing regexes are byte-for-byte copies. `evaluate_intermediate_nl_responses`
  is left at its google-adk default (`false`, final response only). Context-building intentionally omits
  tool-call history: `builtin_metrics.py`'s own `_METRICS_NEEDING_INVOCATION_EVENTS` adapter set doesn't include
  `hallucinations_v1`, so even Python's own port never includes it on agentevals-style traces - this Go port
  matches that real behavior exactly, not a Go-specific simplification. Unlike both the upstream ADK evaluator
  and the original Go port, the per-sentence rationale/excerpt the validator prompt already asks for is no
  longer discarded: `MetricResult.details.per_invocation[i].sentences` carries each sentence's label, rationale,
  and supporting/contradicting excerpt, rendered in the UI (`HallucinationSentenceDetails.tsx`) so users can
  see *why* a hallucination score came out the way it did, not just the number.

- **"Algorithm vs. LLM judge" explainability** - every `MetricResult` now reports `evaluatorKind`
  (`"algorithm"` | `"llm_judge"` | `"vertex_judge"`) and, for `llm_judge` results, the actual `judgeModel` used
  (`internal/api/evaluate.go`'s `metricKind`). The UI shows this as a small badge next to each metric name
  (`EvaluatorKindBadge.tsx`) so users can immediately tell whether a result is a deterministic computation or a
  live LLM-as-judge call, and by which model - this classification/labeling has no Python-upstream equivalent;
  it's additive.
- **`rubric_based_final_response_quality_v1` / `rubric_based_tool_use_quality_v1`** (`internal/judge/rubric.go`,
  `rubric_based_v1.go`) - both prompt templates ported verbatim from `rubric_based_final_response_quality_v1.py`
  / `rubric_based_tool_use_quality_v1.py`; shared sampling/aggregation logic (`runRubricMetric`) ports
  `rubric_based_evaluator.py`'s `RubricBasedEvaluator`: `judge.DefaultNumSamples` (5) samples per invocation,
  each sample's `ID:`/`Property:`/`Rationale:`/`Verdict:` blocks parsed and matched back to a caller-supplied
  rubric by id or (falling back to) normalized property text, majority-voted per rubric across samples
  (`MajorityVotePerInvocationResultsAggregator`), then averaged across every (invocation, rubric) pair in the
  whole trace (`MeanInvocationResultsSummarizer` - not a mean of per-invocation means). There is no built-in
  rubric set: both metrics require caller-supplied rubric text (`judge.RubricsFromStrings`, matching
  `builtin_metrics.py`'s `rubric_strings_to_objects`) - CLI: repeatable `--rubric "..."`; API:
  `evaluatorConfigDTO.rubrics` (`[]string`, additive - not yet in `ui/src/lib/types.ts`'s `EvalConfig`, which
  only shows a "Requires Rubrics" badge with no input yet). One known simplification: rubric-text matching
  skips true Unicode NFKC normalization (`_normalize_text`) since no NFKC implementation is available without
  adding an external Go dependency in this environment - smart-quote/whitespace/decoration normalization is
  still applied, covering the common markdown-wrapping case.
- **`safety_v1`, `multi_turn_task_success_v1`, `multi_turn_trajectory_quality_v1`,
  `multi_turn_tool_use_quality_v1`** (`internal/vertexeval`) - a from-scratch Go REST client for Vertex AI's
  Managed Evaluation Service (`:evaluateInstances`), architecturally distinct from `internal/judge`: no prompt
  is built or sent by this port at all for these four metrics - structured instance data (prompt/response/
  reference, or a full multi-turn conversation) is POSTed to `https://{location}-aiplatform.googleapis.com/
  v1beta1/projects/{project}/locations/{location}/:evaluateInstances` and Vertex's own managed autorater scores
  it server-side. Authenticated via Application Default Credentials (`cloud.google.com/go/auth/credentials`,
  `cloud-platform` scope) - the same Workload Identity service account already used for the judge model, no API
  key ever accepted. Reads `GOOGLE_CLOUD_PROJECT` (required) / `GOOGLE_CLOUD_LOCATION` (defaults `us-central1`).
  The exact wire format (`agent_data` renamed to `agent_eval_data` on the wire, `other_data`/`rubric_groups`
  camelCased but everything else staying snake_case, the synthetic single-turn `agent_eval_data` conversation
  Python always sends alongside the flat prompt/response/reference fields) was reverse-engineered from
  `vertexai._genai.evals`'s request-building source, not guessed. `safety_v1` calls once per invocation
  (skipping invocations with no user text, matching `METRICS_SINGLE_TURN_VERTEX_PROMPT_REQUIRED`'s guard); the
  three `multi_turn_*` metrics submit the whole conversation in one call, and - matching Vertex's own semantics,
  not a Go-port simplification - only the trailing invocation ever gets a real score back, every prior one is
  `NOT_EVALUATED`.

The Python project continues to evolve out from under this port (OpenInference/voice-module support, the
per-invocation span-bucketing fix in `_bucket_spans_by_invocation`, this session-persistence feature, the UI's
time-range filter and opt-in comparison sessions, GitHub OAuth). `ui/` is periodically re-synced wholesale from
`agentevals/ui` since it's copied verbatim; the backend is ported file-by-file as covered above - check
`git diff` in the Python repo before assuming this port already reflects a given change.

## Known fidelity gap

`response_match_score`'s ROUGE-1 tokenizer does not stem (the Python evaluator's `rouge_score` library uses a
Porter stemmer, `use_stemmer=True`) and does not special-case CJK/Thai/combining-mark scripts the way
`final_response_match_v1.py`'s `_UnicodeAwareTokenizer` does. Scores will run a little lower here on inflected
English text and won't be meaningful on non-Latin scripts yet. Fixing this means either vendoring a Go Porter
stemmer or reimplementing `_UnicodeAwareTokenizer`'s character-class logic in Go.

## Not yet ported

Scoped from a prior analysis of what's actually local computation vs. a remote call in `google-adk`'s Python
eval module:

**Straightforward Go work (infra), not yet started:**
- `/v1/logs` ingestion (needed for frameworks, e.g. Strands, that store message content in log records rather
  than span attributes - `utils/log_enrichment.py`; also means `IncrementalInvocationExtractor.process_log`
  isn't wired to any live input yet, though the Go port of the function itself - for when `/v1/logs` lands - is
  not written either). Note this is *not* required for triage-core (this cluster's only producer today): it
  never emits OTLP logs, so its tool-call visibility gap was the missing batch converter below, not this.
- The REST API surface beyond health/sessions/get-trace/create-eval-set/evaluate/evaluate-stream/stream-ui-updates/`/v1/traces`/`/api/runs`
  - `/api/validate/eval-set`, `/api/evaluate/json*` (JSON-body request/response and streaming
  variants), custom evaluator protocol (stdin/stdout JSON subprocess contract), `credential_refs`-based
  per-request judge credential injection
- Python's in-process MCP mount via `serve --mcp-port` (`ServiceAuthMiddleware`) - this port's MCP server
  (see "Ported and working" above) is a standalone stdio process only (`agentevals mcp`), not mounted into
  `serve`.
- `POST /api/evaluate`'s `RunResult.performanceMetrics`/`TraceResult.performanceMetrics` (token/latency
  percentiles, model/agent identity - `trace_metrics.py`'s `extract_performance_metrics`) aren't populated yet;
  `metricResults`/`conversionWarnings`/`numInvocations` are.
- `POST /api/streaming/evaluate-sessions` (`streaming_routes.py`) - golden-session-derived eval set run against
  every complete session concurrently, semaphore-limited. Not needed for the UI's actual "Prepare Evaluation"
  flow (which goes through `create-eval-set` then the generic `/api/evaluate` upload path, both ported), but a
  documented gap if something calls this endpoint directly.

**Needs careful, faithful porting of ADK Python source, not reinvention - genuinely out of scope, not just
unfinished:**
- **`per_turn_user_simulator_quality_v1`** - `LlmBackedUserSimulatorCriterion`/`per_turn_user_simulator_quality_v1.py`
  unconditionally requires a `ConversationScenario` (`conversation_plan`/`user_persona`/`starting_prompt`) on the
  eval case. That shape only exists for live LLM-user-simulator eval runs generating new turns on the fly - it
  has no equivalent in this port's data model (`adk.EvalCase`/`Invocation`), which only ever scores already-
  recorded production trace turns. Forcing a fake/empty scenario would silently misscore rather than genuinely
  port the metric, so it's skipped.
- **`response_evaluation_score`** - unlike every other metric here, this one does *not* map to a Vertex
  predefined `metric_spec_name`: Python's `ResponseEvaluator` resolves it to `PrebuiltMetric.COHERENCE`, which
  `vertexai._genai._evals_constant.SUPPORTED_PREDEFINED_METRICS` doesn't include, so it falls through to a
  GCS-hosted YAML prompt template plus a client-side `result_parsing_function` (an executed Python code string)
  - a fundamentally different and more fragile mechanism than the 4 `internal/vertexeval` metrics above, which
  are all clean predefined-metric-spec calls. Porting it faithfully would mean replicating an under-specified,
  GCS-fetched, exec'd-code contract sight-unseen; deferred rather than risk a subtly wrong port.

`internal/eval/runner.go`'s `unported` map is the single source of truth for these two; its `NotYetImplemented`
map separately lists every judge-model/Vertex-eval metric name `internal/eval.RunMetric` itself can't compute
(they're implemented in `internal/judge`/`internal/vertexeval` instead - see above - and only reach `RunMetric`
at all if some future caller bypasses the CLI's/API's own dispatch).

Every ported function has a comment naming the exact Python source it was ported from (e.g. "Ported from
`converter.py`'s `_extract_user_content`"), so re-verifying fidelity against a `google-adk` upgrade is a diff,
not an archaeology exercise.

## Full module inventory

A complete pass over every file in `agentevals/src/agentevals/` at the Python project's current commit
(`b1450cf`, plus its uncommitted working-tree changes - this repo has no tags, so there's no released version
number to pin against), cross-checked against this port. The categories above cover the story; this is the
checklist that produced it, so a gap doesn't have to be rediscovered by re-reading the Python source again.

**Ported:**

| Python | Go |
|---|---|
| `loader/base.py`, `loader/jaeger.py` | `internal/trace/` |
| `loader/otlp.py`, `otlp_anyvalue.py` | `internal/otlp/` |
| `loader/auto.py` (format auto-detection + dispatch, minus legacy Tempo `batches`/wrapper shapes) | `internal/loader/` |
| `extraction.py` (ADK-first + OpenInference-fallback paths) | `internal/adk/extract.go` |
| `converter.py` (format auto-detect + ADK conversion) | `internal/adk/converter.go`, `genai_converter.go` |
| `genai_converter.py` (GenAI-semconv + OpenInference batch conversion, incl. `_extract_openinference_turns` for realtime/voice producers like triage-core) | `internal/adk/genai_converter.go` |
| `trace_attrs.py` (subset the above need) | `internal/adk/attrs.go` |
| `trajectory_evaluator.py`, `final_response_match_v1.py` (via `builtin_metrics.py`) | `internal/eval/trajectory.go`, `rouge.go` |
| `runner.py` (metric dispatch, expected-invocation matching; performance-metrics aggregation not ported) | `internal/eval/runner.go`, `evalset.go` |
| `final_response_match_v2.py`, `llm_as_judge.py`, `hallucinations_v1.py`, `rubric_based_final_response_quality_v1.py`, `rubric_based_tool_use_quality_v1.py`, `rubric_based_evaluator.py` (via `builtin_metrics.py`) | `internal/judge/` |
| `vertex_ai_eval_facade.py`, `safety_evaluator.py`, `multi_turn_task_success_evaluator.py`, `multi_turn_trajectory_quality_evaluator.py`, `multi_turn_tool_use_quality_evaluator.py` | `internal/vertexeval/` |
| `api/otlp_processing.py` (`process_traces`; `process_logs` not ported), `api/otlp_routes.py` (HTTP only), `api/otlp_grpc.py` (trace service only; logs service, gzip, max-message/concurrency options, and the bearer-token interceptor are not ported) | `internal/api/server.go`, `sessions.go`, `grpc.go` |
| `streaming/session.py`, `streaming/ws_server.py` (session grouping, completion state machine incl. idle/grace timers, batch invocation extraction at completion, SSE broadcast) | `internal/api/sessions.go`, `sse.go`, `invocations.go` |
| `streaming/incremental_processor.py` (`process_span`; `process_log` not ported) | `internal/incremental/` |
| `storage/session_store.py` | `internal/api/sqlitestore.go` |
| `api/models.py`, `api/streaming_routes.py` (`sessions`/`get-trace`/`create-eval-set`), `api/routes.py`'s `evaluate_traces`/`evaluate_traces_stream` | struct types + handlers across `internal/api/` (`server.go`, `evaluate.go`) |
| `utils/genai_messages.py` | inlined into `internal/adk/extract.go` |
| `cli.py` (the `run`/`serve`/`auth mint-token`/`mcp` subset) | `cmd/agentevals/main.go`, `auth.go`, `mcp.go` |
| `mcp_server.py` (minus `evaluate_sessions`/`eval_config_file`, see above) | `cmd/agentevals/mcp.go` |

**Not applicable to a Go port** (Python-agent-side client tooling, not server/CLI functionality):
- `sdk.py`, `streaming/processor.py` - the `AgentEvals()` Python SDK Python *agents* import to instrument
  themselves (OTel SpanProcessor/LogRecordProcessor + WebSocket boilerplate). A Go equivalent would be a
  library for Go agents to import, not a port of this file.

**Not yet ported** (beyond what's covered above - every remaining `src/agentevals/` file):
- `per_turn_user_simulator_quality_v1.py`, `response_evaluator.py` - genuinely out of scope; see above
  ("Needs careful, faithful porting..." bullets) for why
- `_protocol.py`, `custom_evaluators.py`, `evaluator/*` (`resolver.py`, `sources.py`, `templates.py`,
  `venv.py`) - the custom evaluator stdin/stdout subprocess protocol, remote evaluator fetching, and
  per-evaluator venv management
- `eval_config_loader.py`, `config.py` - YAML `eval_config.yaml` loading (`evaluators:` list, `builtin`/
  `code`/`remote`/`openai_eval` entries) and the run-config model it populates. The CLI here only takes
  metrics as repeated `-m` flags; there's no config-file input at all yet.
- `openai_eval_backend.py` - delegating grading to the OpenAI Evals API (`type: openai_eval` evaluators)
- `output.py` - the Python CLI's table/JSON formatting (not a gap exactly: `cmd/agentevals/main.go` has its
  own simpler formatter, not a port of this one - output won't look identical)
- `trace_metrics.py` - `extract_performance_metrics`/`extract_trace_metadata` (latency percentiles, token
  usage summaries, model/agent identity) backing the Python API's dashboard-style responses; nothing here
  computes or exposes this yet
- `resolvers/*` (`env`, `kubernetes`) - secret resolution for evaluator credentials
- `run/*` (`fetcher.py`, `result_builder.py`, `service.py`, `sinks.py`, `worker.py`), `storage/postgres/*`,
  `storage/repos/*`, `storage/config.py`, `storage/models.py`, `api/runs_routes.py` - upstream's async,
  queue/worker-backed `/api/runs` pipeline (itself a **preview feature** there) isn't ported as such. Instead,
  `internal/api/runsstore.go`/`runs.go` add a much smaller synchronous alternative sharing `SQLiteStore`'s
  connection: `evaluateHandler`/`evaluateStreamHandler` write one Run + its RunResultRows directly when an
  evaluation finishes (no submit/queue/worker step), and `GET /api/runs`/`GET /api/runs/{id}`/
  `GET /api/runs/{id}/results` read them back - enough to back the already-built Run History dashboard
  (`ui/src/components/runs`) with real data. Only available when `--session-db`/`AGENTEVALS_SESSION_DB_PATH`
  is set (matches the "storage unavailable" 503 the UI already expects); `evalSetItemId`/`evalSetItemName`
  are populated from the uploaded trace's ID/filename rather than a named eval-set item, since this flow has
  no such concept; `RunResultRow.spanId`/`tokensUsed`/`latencyMs` and `RunSummary.performance_metrics` aren't
  populated (same `extract_performance_metrics` gap noted above).
- **EvalSet persistence** (`internal/api/evalsetsstore.go`/`evalsets.go`) - additive, no Python-upstream
  equivalent: upstream's EvalSet Builder (and this port's, until now) only ever downloaded a golden-reference
  JSON file to disk (`ui/src/lib/evalset-builder.ts`'s `downloadEvalSet`), with nothing to show it had been
  saved anywhere. `POST /api/evalsets` (upserted by `eval_set_id`), `GET /api/evalsets` (summaries), and
  `GET /api/evalsets/{id}` (full document) persist to the same `SQLiteStore` file as Run History, so a saved
  EvalSet shows up in a "Saved EvalSets" list on the Builder's landing screen (`SavedEvalSetsList.tsx`) and can
  be reopened for editing - the "Save EvalSet" button still also downloads the file as before. Same
  `--session-db`-gated 503 contract as `/api/runs`.
- `api/app.py`, `api/otlp_app.py`, `api/dependencies.py`, `api/debug_routes.py`, `api/routes.py`'s remaining
  routes (`/api/metrics`, `/api/validate/eval-set`, `/api/convert`, `/api/evaluate/json`/`/json/stream`,
  `credential_refs` handling - `evaluate_traces`/`evaluate_traces_stream` themselves are ported, see above) -
  the rest of the FastAPI app; `internal/api` is a much smaller, purpose-built REST surface, not a port of
  this app-factory structure
- `api/otlp_grpc.py` - the OTLP gRPC receiver (`:4317`)
- `utils/log_enrichment.py`, `utils/log_buffer.py`, `/v1/logs` ingestion, `IncrementalInvocationExtractor.process_log`
- `auth/*`'s service-to-service bearer-token fallback as Python implements it (guarding only its in-process
  MCP mount's `ServiceAuthMiddleware`, which isn't ported - see above; this port's own OTLP HTTP/gRPC
  receivers are never gated at all, matching Python's own `otlp_routes.py`/`otlp_grpc.py`) - the interactive
  GitHub OAuth login/callback/logout flow, signed session cookies, and this port's own bearer-token
  acceptance (used by `agentevals mcp` to reach a GitHub-OAuth-gated `serve`, mintable via
  `agentevals auth mint-token`) are all ported (`internal/api/oauth.go`, `sessionauth.go`)
- Session completion state machine (grace/idle timers, `is_complete`), `/ws/traces` WebSocket ingestion,
  per-invocation span-bucketing (`_bucket_spans_by_invocation`) for scoped token/latency/tool metrics

