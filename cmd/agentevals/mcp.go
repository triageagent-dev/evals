// mcp.go implements `agentevals mcp`: a Model Context Protocol server on
// stdio, for use with Claude Code, Cursor, GitHub Copilot CLI, and other
// MCP clients. Ported from agentevals' mcp_server.py's create_server -
// same four core tools (list_metrics, evaluate_traces, list_sessions,
// summarize_session), same tool/parameter names and response field names
// (snake_case, matching Python's pydantic models - this is a different
// wire contract than internal/api's camelCase REST DTOs, which exist to
// match the React UI's TypeScript types instead). Additive over Python:
// list_runs/get_run_results expose this Go port's run-history storage
// (internal/api/runs.go, GET /api/runs and /api/runs/{id}/results), which
// has no Python equivalent - mcp_server.py doesn't have run history
// persistence to surface at all.
//
// Not ported: evaluate_sessions (streaming_routes.py's
// POST /api/streaming/evaluate-sessions isn't implemented yet - see
// docs/STATUS.md) and the eval_config_file parameter on evaluate_traces
// (no eval_config.yaml loader exists in this port at all yet).
//
// Auth (additive over Python, which sends no auth header at all yet):
// every tool that calls a running `agentevals serve` over HTTP (list_
// sessions, summarize_session, list_runs, get_run_results) can't do an
// interactive GitHub OAuth login itself, so something has to go in an
// `Authorization: Bearer <token>` header on every request to reach a
// GitHub-OAuth-gated deployment non-interactively. Two ways to supply it,
// checked in this order: --session-token/AGENTEVALS_SESSION_TOKEN (an
// agentevals-minted token from `agentevals auth mint-token`), or, if
// neither is set, a fallback to running `gh auth token` and sending that
// GitHub token straight through - internal/api's requireSession accepts a
// live GitHub token as an alternative to its own signed tokens (see
// internal/api/githubtoken.go), precisely so this needs no separately
// minted secret at all when the caller already has `gh` authenticated as
// a member of the deployment's GitHub org. evaluate_traces needs no
// server and no token at all: it loads trace files from local disk and
// evaluates them in-process, exactly like `agentevals run`.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/triageagent-dev/agentevals-go/internal/adk"
	"github.com/triageagent-dev/agentevals-go/internal/eval"
	"github.com/triageagent-dev/agentevals-go/internal/judge"
	"github.com/triageagent-dev/agentevals-go/internal/loader"
	"github.com/triageagent-dev/agentevals-go/internal/otlp"
	"github.com/triageagent-dev/agentevals-go/internal/vertexeval"
)

// mcpServerVersion is reported to MCP clients during initialize; not
// otherwise meaningful (this project has no release tags to pin to yet -
// see README.md).
const mcpServerVersion = "0.1.0"

const defaultMCPServerURL = "http://localhost:8001"

func mcpCmd(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	serverURL := fs.String("server-url", os.Getenv("AGENTEVALS_SERVER_URL"),
		"agentevals server URL for list_sessions/summarize_session (default: http://localhost:8001); falls back to AGENTEVALS_SERVER_URL")
	sessionToken := fs.String("session-token", os.Getenv("AGENTEVALS_SESSION_TOKEN"),
		"bearer token sent as an Authorization: Bearer header on every request to --server-url, for a GitHub-OAuth-gated deployment; falls back to AGENTEVALS_SESSION_TOKEN, then to `gh auth token` if neither is set and gh is installed and logged in. Mint one with: agentevals auth mint-token --name <label>, or just use gh auth login instead.")
	noGHAuth := fs.Bool("no-gh-auth", false, "disable the gh auth token fallback; fail with no credential instead of shelling out to gh")
	if err := fs.Parse(args); err != nil {
		return err
	}

	url := strings.TrimRight(*serverURL, "/")
	if url == "" {
		url = defaultMCPServerURL
	}

	token := *sessionToken
	if token == "" && !*noGHAuth {
		token = ghAuthToken()
	}

	backend := &mcpBackend{
		baseURL: url,
		token:   token,
		client:  &http.Client{Timeout: 60 * time.Second},
	}

	s := newMCPServer(backend)
	return server.ServeStdio(s)
}

// ghAuthToken shells out to `gh auth token` as a zero-config fallback
// credential for a GitHub-OAuth-gated deployment: if the caller already
// has the GitHub CLI authenticated (gh auth login), that token is an
// active member's real GitHub token, which internal/api/githubtoken.go
// accepts directly - no agentevals-specific secret to mint or store
// anywhere. Best-effort: gh not being installed, not being logged in, or
// exiting non-zero for any other reason all just mean "no token", exactly
// as if --session-token/AGENTEVALS_SESSION_TOKEN had been left unset;
// this never fails mcpCmd outright - a request to an auth-gated
// deployment then simply fails at the HTTP layer with a normal "not
// authenticated" error, same as always having had no token at all.
func ghAuthToken() string {
	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// newMCPServer builds the FastMCP-equivalent server and registers all four
// tools. Split out from mcpCmd so tests can call it directly without
// spinning up stdio.
func newMCPServer(backend *mcpBackend) *server.MCPServer {
	s := server.NewMCPServer("agentevals", mcpServerVersion, server.WithToolCapabilities(false))

	s.AddTool(mcp.NewTool("list_metrics",
		mcp.WithDescription("List all available evaluation metrics with their descriptions and requirements. "+
			"Call this first to discover which metrics you can pass to evaluate_traces."),
	), listMetricsHandler(backend))

	s.AddTool(mcp.NewTool("evaluate_traces",
		mcp.WithDescription("Evaluate one or more local agent trace files against selected evaluators. "+
			"Loads trace files from disk, converts them to the internal invocation format, and runs each "+
			"requested evaluator. Does not require the agentevals server to be running."),
		mcp.WithArray("trace_files", mcp.Required(),
			mcp.Description("Absolute paths to trace files on disk. Supports Jaeger JSON and OTLP JSON/JSONL. Each file may contain one or more traces."),
			mcp.Items(map[string]any{"type": "string"})),
		mcp.WithArray("metrics",
			mcp.Description("Metric names to evaluate (from list_metrics). Defaults to [\"tool_trajectory_avg_score\"] if not specified."),
			mcp.Items(map[string]any{"type": "string"})),
		mcp.WithString("trace_format",
			mcp.Description("Optional explicit format override (\"jaeger-json\" or \"otlp-json\"). Auto-detected from file contents when omitted.")),
		mcp.WithString("eval_set_file",
			mcp.Description("Absolute path to a golden eval set JSON file (ADK EvalSet format). Required by comparison metrics such as tool_trajectory_avg_score and response_match_score.")),
		mcp.WithString("judge_model",
			mcp.Description("LLM model name for judge-based metrics, e.g. \"gemini-2.5-flash\". Required by metrics like hallucinations_v1 and final_response_match_v2.")),
		mcp.WithNumber("threshold", mcp.DefaultNumber(0.5),
			mcp.Description("Score threshold for PASS/FAIL classification, between 0.0 and 1.0.")),
		mcp.WithArray("rubrics",
			mcp.Description("Rubric text for rubric_based_final_response_quality_v1/rubric_based_tool_use_quality_v1 (repeatable); required by those metrics."),
			mcp.Items(map[string]any{"type": "string"})),
	), evaluateTracesHandler())

	s.AddTool(mcp.NewTool("list_sessions",
		mcp.WithDescription("List recent streaming trace sessions, ordered most recent first. Use this to discover "+
			"session IDs for summarize_session. Requires the agentevals server to be running (agentevals serve)."),
		mcp.WithNumber("limit", mcp.DefaultNumber(20),
			mcp.Description("Maximum number of sessions to return.")),
	), listSessionsHandler(backend))

	s.AddTool(mcp.NewTool("summarize_session",
		mcp.WithDescription("Get a structured summary of a session showing its invocations, tool calls, and messages "+
			"in human-readable form. Use this to understand what an agent did during a session before running "+
			"evaluation. Requires the agentevals server to be running."),
		mcp.WithString("session_id", mcp.Required(),
			mcp.Description("Session ID obtained from list_sessions.")),
	), summarizeSessionHandler(backend))

	s.AddTool(mcp.NewTool("list_runs",
		mcp.WithDescription("List past evaluation runs (run history), most recent first. Use this to discover run "+
			"IDs for get_run_results. Requires the agentevals server to be running with --session-db/"+
			"AGENTEVALS_SESSION_DB_PATH set (run history storage)."),
		mcp.WithNumber("limit", mcp.DefaultNumber(20),
			mcp.Description("Maximum number of runs to return.")),
		mcp.WithArray("status",
			mcp.Description("Optional status filter, e.g. [\"succeeded\"], [\"failed\"], [\"running\"]. Returns all statuses if omitted."),
			mcp.Items(map[string]any{"type": "string"})),
	), listRunsHandler(backend))

	s.AddTool(mcp.NewTool("get_run_results",
		mcp.WithDescription("Get the full result detail for one evaluation run: its status, summary counts, "+
			"per-metric averages, and the per-eval-case/per-evaluator result rows (scores, pass/fail, errors). "+
			"Use this after list_runs to inspect why a run passed or failed. Requires the agentevals server to "+
			"be running with run history storage enabled."),
		mcp.WithString("run_id", mcp.Required(),
			mcp.Description("Run ID obtained from list_runs.")),
	), getRunResultsHandler(backend))

	return s
}

// ---------------------------------------------------------------------------
// HTTP backend for list_sessions/summarize_session
// ---------------------------------------------------------------------------

// mcpBackend is a thin HTTP client for the two MCP tools that need a live
// `agentevals serve` process (list_sessions, summarize_session) - unlike
// evaluate_traces/list_metrics... actually list_metrics also calls the
// server (GET /api/metrics); only evaluate_traces is fully offline.
// Ported from mcp_server.py's create_server: same base URL env var, same
// {"data":...,"error":...} envelope unwrapping, same "start it with..."
// hint on connection failure - plus an optional bearer token Python's
// version doesn't send yet (see package doc).
type mcpBackend struct {
	baseURL string
	token   string
	client  *http.Client
}

// envelope mirrors every internal/api JSON handler's {"data":..., "error":
// ...} response shape (api/routes.py's generic response envelope).
type envelope struct {
	Data  json.RawMessage `json:"data"`
	Error *string         `json:"error"`
}

func (b *mcpBackend) request(ctx context.Context, method, path string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reqBody = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.baseURL+path, reqBody)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if b.token != "" {
		req.Header.Set("Authorization", "Bearer "+b.token)
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach agentevals server at %s: %w (start it with: agentevals serve)", b.baseURL, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response from %s: %w", path, err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("server at %s returned 401 Unauthorized - pass --session-token/AGENTEVALS_SESSION_TOKEN (mint one with: agentevals auth mint-token --name <label>)", b.baseURL)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("server error %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var env envelope
	if err := json.Unmarshal(respBody, &env); err != nil {
		return fmt.Errorf("decoding response from %s: %w", path, err)
	}
	if env.Error != nil && *env.Error != "" {
		return fmt.Errorf("API error: %s", *env.Error)
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("decoding data from %s: %w", path, err)
		}
	}
	return nil
}

func (b *mcpBackend) get(ctx context.Context, path string, out any) error {
	return b.request(ctx, http.MethodGet, path, nil, out)
}

func (b *mcpBackend) post(ctx context.Context, path string, body, out any) error {
	return b.request(ctx, http.MethodPost, path, body, out)
}

// ---------------------------------------------------------------------------
// list_metrics
// ---------------------------------------------------------------------------

// metricInfoMCP mirrors mcp_server.py's MetricInfoResponse field-for-field
// (snake_case) - internal/api/config.go's metricInfo (this tool's actual
// HTTP source) uses camelCase to match the UI instead.
type metricInfoMCP struct {
	Name            string `json:"name"`
	Category        string `json:"category"`
	RequiresEvalSet bool   `json:"requires_eval_set"`
	RequiresLLM     bool   `json:"requires_llm"`
	RequiresGCP     bool   `json:"requires_gcp"`
	RequiresRubrics bool   `json:"requires_rubrics"`
	Description     string `json:"description"`
	Working         bool   `json:"working"`
}

func listMetricsHandler(backend *mcpBackend) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var raw []struct {
			Name            string `json:"name"`
			Category        string `json:"category"`
			RequiresEvalSet bool   `json:"requiresEvalSet"`
			RequiresLLM     bool   `json:"requiresLLM"`
			RequiresGCP     bool   `json:"requiresGCP"`
			RequiresRubrics bool   `json:"requiresRubrics"`
			Description     string `json:"description"`
			Working         bool   `json:"working"`
		}
		if err := backend.get(ctx, "/api/metrics", &raw); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		out := make([]metricInfoMCP, len(raw))
		for i, m := range raw {
			out[i] = metricInfoMCP{
				Name: m.Name, Category: m.Category,
				RequiresEvalSet: m.RequiresEvalSet, RequiresLLM: m.RequiresLLM,
				RequiresGCP: m.RequiresGCP, RequiresRubrics: m.RequiresRubrics,
				Description: m.Description, Working: m.Working,
			}
		}
		return mcp.NewToolResultStructuredOnly(out), nil
	}
}

// ---------------------------------------------------------------------------
// evaluate_traces (fully offline - no HTTP call)
// ---------------------------------------------------------------------------

type metricScoreMCP struct {
	Metric string   `json:"metric"`
	Score  *float64 `json:"score,omitempty"`
	Status string   `json:"status"`
	Error  string   `json:"error,omitempty"`
}

type traceEvalMCP struct {
	TraceID        string           `json:"trace_id"`
	NumInvocations int              `json:"num_invocations"`
	Metrics        []metricScoreMCP `json:"metrics"`
	Warnings       []string         `json:"warnings,omitempty"`
}

type evaluateTracesResultMCP struct {
	Passed bool           `json:"passed"`
	Traces []traceEvalMCP `json:"traces"`
	Errors []string       `json:"errors,omitempty"`
}

func evaluateTracesHandler() server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		traceFiles := req.GetStringSlice("trace_files", nil)
		if len(traceFiles) == 0 {
			return mcp.NewToolResultError("trace_files is required"), nil
		}
		metrics := req.GetStringSlice("metrics", []string{"tool_trajectory_avg_score"})
		traceFormat := req.GetString("trace_format", "")
		evalSetFile := req.GetString("eval_set_file", "")
		judgeModelName := req.GetString("judge_model", judge.DefaultModel)
		threshold := req.GetFloat("threshold", 0.5)
		rubricTexts := req.GetStringSlice("rubrics", nil)

		result, err := runEvaluateTraces(ctx, traceFiles, metrics, traceFormat, evalSetFile, judgeModelName, threshold, rubricTexts)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultStructuredOnly(result), nil
	}
}

// runEvaluateTraces is evaluate_traces' core logic, deliberately mirroring
// runCmd's loop (cmd/agentevals/main.go) rather than sharing code with it -
// same reason runCmd's own runJudgeMetric/runVertexEvalMetric already
// mirror internal/api/evaluate.go's instead of importing internal/api:
// this stays independent of any HTTP-specific machinery. Unlike runCmd
// (which only reads Jaeger JSON, matching the historical CLI quickstart
// exactly), this uses internal/loader's format auto-detection +
// adk.ConvertTrace, matching what /api/evaluate and Python's evaluate_traces
// tool both actually accept (Jaeger JSON or OTLP JSON/JSONL).
func runEvaluateTraces(
	ctx context.Context,
	traceFiles, metrics []string,
	traceFormat, evalSetFile, judgeModelName string,
	threshold float64,
	rubricTexts []string,
) (*evaluateTracesResultMCP, error) {
	var evalSet *adk.EvalSet
	if evalSetFile != "" {
		var err error
		evalSet, err = eval.LoadEvalSet(evalSetFile)
		if err != nil {
			return nil, fmt.Errorf("loading eval set: %w", err)
		}
	}

	thresholds := make(map[string]float64, len(metrics))
	for _, m := range metrics {
		thresholds[m] = threshold
	}

	var localMetrics, requestedJudgeMetrics, requestedVertexMetrics []string
	for _, m := range metrics {
		switch {
		case judgeMetrics[m]:
			requestedJudgeMetrics = append(requestedJudgeMetrics, m)
		case vertexEvalMetrics[m]:
			requestedVertexMetrics = append(requestedVertexMetrics, m)
		default:
			localMetrics = append(localMetrics, m)
		}
	}

	var judgeModelClient judge.Model
	if len(requestedJudgeMetrics) > 0 {
		client, err := judge.NewGenAIModel(ctx, "", judgeModelName)
		if err != nil {
			return nil, fmt.Errorf("setting up judge model: %w", err)
		}
		judgeModelClient = client
	}

	var vertexEvalClient *vertexeval.Client
	if len(requestedVertexMetrics) > 0 {
		client, err := vertexeval.NewClient(ctx)
		if err != nil {
			return nil, fmt.Errorf("setting up Vertex AI Managed Eval Service client: %w", err)
		}
		vertexEvalClient = client
	}

	rubricObjects := judge.RubricsFromStrings(rubricTexts)

	result := &evaluateTracesResultMCP{Passed: true}
	for _, path := range traceFiles {
		content, err := os.ReadFile(path)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("reading %s: %v", path, err))
			continue
		}
		traces, err := loader.ParseFile(path, content, traceFormat)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("loading %s: %v", path, err))
			continue
		}

		for _, tr := range traces {
			conv := adk.ConvertTrace(tr)
			expected := eval.FindExpectedInvocations(conv.Invocations, evalSet)

			var results []eval.Result
			if len(localMetrics) > 0 {
				results = append(results, eval.RunMetrics(conv.Invocations, evalSet, localMetrics, eval.MatchExact, thresholds)...)
			}
			for _, m := range requestedJudgeMetrics {
				r, err := runJudgeMetric(ctx, m, judgeModelClient, conv.Invocations, expected, rubricObjects, judge.DefaultNumSamples, thresholds[m])
				if err != nil {
					r = eval.Result{MetricName: m, Error: err.Error()}
				}
				results = append(results, r)
			}
			for _, m := range requestedVertexMetrics {
				r, err := runVertexEvalMetric(ctx, m, vertexEvalClient, conv.Invocations, expected, thresholds[m])
				if err != nil {
					r = eval.Result{MetricName: m, Error: err.Error()}
				}
				results = append(results, r)
			}

			te := traceEvalMCP{TraceID: tr.TraceID, NumInvocations: len(conv.Invocations), Warnings: conv.Warnings}
			for _, r := range results {
				// Ported from mcp_server.py's summarize_run_result, plus one
				// deliberate addition: an errored metric gets status "ERROR"
				// (matching internal/api/evaluate.go's toMetricResultDTO
				// convention, which the REST API/UI already use) rather than
				// Python's status left as whatever eval_status happened to be
				// - and, unlike Python's exact `all(status != "FAILED")`,
				// "ERROR" also fails the top-level passed field here, so a
				// crashed metric can't silently read as passing.
				status := string(r.Status)
				if r.Error != "" {
					status = "ERROR"
				}
				ms := metricScoreMCP{Metric: r.MetricName, Status: status, Error: r.Error}
				if r.Error == "" {
					score := r.Score
					ms.Score = &score
				}
				if status == "FAILED" || status == "ERROR" {
					result.Passed = false
				}
				te.Metrics = append(te.Metrics, ms)
			}
			result.Traces = append(result.Traces, te)
		}
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// list_sessions
// ---------------------------------------------------------------------------

type sessionSummaryMCP struct {
	SessionID  string `json:"session_id"`
	IsComplete bool   `json:"is_complete"`
	SpanCount  int    `json:"span_count"`
	StartedAt  string `json:"started_at"`
}

func listSessionsHandler(backend *mcpBackend) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		limit := req.GetInt("limit", 20)

		// Matches internal/api's SessionSummary (server.go's sessionsHandler).
		var raw []struct {
			SessionID  string `json:"sessionId"`
			IsComplete bool   `json:"isComplete"`
			SpanCount  int    `json:"spanCount"`
			StartedAt  string `json:"startedAt"`
		}
		if err := backend.get(ctx, "/api/streaming/sessions", &raw); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		// Most-recent-first, matching mcp_server.py's list_sessions
		// (sessions.sort(key=..., reverse=True)); ISO 8601 timestamps sort
		// correctly as plain strings.
		for i := 1; i < len(raw); i++ {
			for j := i; j > 0 && raw[j].StartedAt > raw[j-1].StartedAt; j-- {
				raw[j], raw[j-1] = raw[j-1], raw[j]
			}
		}
		if limit > 0 && len(raw) > limit {
			raw = raw[:limit]
		}

		out := make([]sessionSummaryMCP, len(raw))
		for i, s := range raw {
			out[i] = sessionSummaryMCP{SessionID: s.SessionID, IsComplete: s.IsComplete, SpanCount: s.SpanCount, StartedAt: s.StartedAt}
		}
		return mcp.NewToolResultStructuredOnly(out), nil
	}
}

// ---------------------------------------------------------------------------
// summarize_session
// ---------------------------------------------------------------------------

type toolCallMCP struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

type invocationSummaryMCP struct {
	User      string        `json:"user"`
	Response  string        `json:"response"`
	ToolCalls []toolCallMCP `json:"tool_calls"`
}

type summarizeSessionResultMCP struct {
	SessionID      string                 `json:"session_id"`
	NumSpans       int                    `json:"num_spans"`
	NumInvocations int                    `json:"num_invocations"`
	Invocations    []invocationSummaryMCP `json:"invocations"`
}

func summarizeSessionHandler(backend *mcpBackend) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sessionID, err := req.RequireString("session_id")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		var raw struct {
			TraceContent string `json:"traceContent"`
			NumSpans     int    `json:"numSpans"`
		}
		if err := backend.post(ctx, "/api/streaming/get-trace", map[string]string{"session_id": sessionID}, &raw); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		out := summarizeSessionResultMCP{SessionID: sessionID, NumSpans: raw.NumSpans, Invocations: []invocationSummaryMCP{}}

		traces, err := otlp.ParseJSONLSpans([]byte(raw.TraceContent))
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("parsing trace content: %v", err)), nil
		}
		for _, tr := range traces {
			conv := adk.ConvertTrace(tr)
			for _, inv := range conv.Invocations {
				var calls []toolCallMCP
				for _, tc := range inv.ToolCalls() {
					args := tc.Args
					if args == nil {
						args = map[string]any{}
					}
					calls = append(calls, toolCallMCP{Tool: tc.Name, Args: args})
				}
				if calls == nil {
					calls = []toolCallMCP{}
				}
				out.Invocations = append(out.Invocations, invocationSummaryMCP{
					User:      inv.UserText(),
					Response:  inv.FinalResponse.Text(),
					ToolCalls: calls,
				})
			}
		}
		out.NumInvocations = len(out.Invocations)
		return mcp.NewToolResultStructuredOnly(out), nil
	}
}

// ---------------------------------------------------------------------------
// list_runs
// ---------------------------------------------------------------------------

type runResultCountsMCP struct {
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Errored int `json:"errored"`
	Skipped int `json:"skipped"`
}

type runPerMetricMCP struct {
	Passed   int      `json:"passed"`
	Failed   int      `json:"failed"`
	Errored  int      `json:"errored"`
	Skipped  int      `json:"skipped"`
	AvgScore *float64 `json:"avg_score"`
}

type runSummaryMCP struct {
	TraceCount   int                        `json:"trace_count"`
	ResultCounts runResultCountsMCP         `json:"result_counts"`
	PerMetric    map[string]runPerMetricMCP `json:"per_metric,omitempty"`
	Agents       []string                   `json:"agents,omitempty"`
	Errors       []string                   `json:"errors,omitempty"`
}

// runListItemMCP mirrors internal/api/runs.go's runDTO, deliberately
// dropping its Spec's full eval-set/eval-config blobs (potentially
// megabytes of raw trace/conversation data per run) - list_runs is for
// discovering run IDs and their pass/fail summary, not for fetching the
// traces a run was evaluated against.
type runListItemMCP struct {
	RunID      string         `json:"run_id"`
	Status     string         `json:"status"`
	Approach   string         `json:"approach,omitempty"`
	Error      *string        `json:"error,omitempty"`
	Summary    *runSummaryMCP `json:"summary,omitempty"`
	CreatedAt  string         `json:"created_at"`
	StartedAt  *string        `json:"started_at,omitempty"`
	FinishedAt *string        `json:"finished_at,omitempty"`
}

func listRunsHandler(backend *mcpBackend) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		limit := req.GetInt("limit", 20)
		statuses := req.GetStringSlice("status", nil)

		// Matches internal/api's runDTO (server.go's listRunsHandler).
		var raw []struct {
			RunID  string `json:"runId"`
			Status string `json:"status"`
			Spec   struct {
				Approach string `json:"approach,omitempty"`
			} `json:"spec"`
			Error      *string        `json:"error,omitempty"`
			Summary    *runSummaryMCP `json:"summary,omitempty"`
			CreatedAt  string         `json:"createdAt"`
			StartedAt  *string        `json:"startedAt,omitempty"`
			FinishedAt *string        `json:"finishedAt,omitempty"`
		}

		path := fmt.Sprintf("/api/runs?limit=%d", limit)
		for _, s := range statuses {
			path += "&status=" + url.QueryEscape(s)
		}
		if err := backend.get(ctx, path, &raw); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		out := make([]runListItemMCP, len(raw))
		for i, r := range raw {
			out[i] = runListItemMCP{
				RunID:      r.RunID,
				Status:     r.Status,
				Approach:   r.Spec.Approach,
				Error:      r.Error,
				Summary:    r.Summary,
				CreatedAt:  r.CreatedAt,
				StartedAt:  r.StartedAt,
				FinishedAt: r.FinishedAt,
			}
		}
		return mcp.NewToolResultStructuredOnly(out), nil
	}
}

// ---------------------------------------------------------------------------
// get_run_results
// ---------------------------------------------------------------------------

type runResultRowMCP struct {
	ResultID            string         `json:"result_id"`
	EvalSetItemID       string         `json:"eval_set_item_id"`
	EvalSetItemName     string         `json:"eval_set_item_name"`
	EvaluatorName       string         `json:"evaluator_name"`
	EvaluatorType       string         `json:"evaluator_type"`
	Status              string         `json:"status"`
	Score               *float64       `json:"score"`
	PerInvocationScores []*float64     `json:"per_invocation_scores,omitempty"`
	TraceID             *string        `json:"trace_id,omitempty"`
	Details             map[string]any `json:"details,omitempty"`
	ErrorText           *string        `json:"error_text,omitempty"`
}

type getRunResultsResultMCP struct {
	RunID      string            `json:"run_id"`
	Status     string            `json:"status"`
	Approach   string            `json:"approach,omitempty"`
	Error      *string           `json:"error,omitempty"`
	Summary    *runSummaryMCP    `json:"summary,omitempty"`
	CreatedAt  string            `json:"created_at"`
	StartedAt  *string           `json:"started_at,omitempty"`
	FinishedAt *string           `json:"finished_at,omitempty"`
	Results    []runResultRowMCP `json:"results"`
}

func getRunResultsHandler(backend *mcpBackend) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		runID, err := req.RequireString("run_id")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		// Matches internal/api's runDTO (server.go's getRunHandler).
		var run struct {
			RunID  string `json:"runId"`
			Status string `json:"status"`
			Spec   struct {
				Approach string `json:"approach,omitempty"`
			} `json:"spec"`
			Error      *string        `json:"error,omitempty"`
			Summary    *runSummaryMCP `json:"summary,omitempty"`
			CreatedAt  string         `json:"createdAt"`
			StartedAt  *string        `json:"startedAt,omitempty"`
			FinishedAt *string        `json:"finishedAt,omitempty"`
		}
		if err := backend.get(ctx, "/api/runs/"+url.PathEscape(runID), &run); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		// Matches internal/api's runResultRowDTO (server.go's getRunResultsHandler).
		var rows []struct {
			ResultID            string         `json:"resultId"`
			EvalSetItemID       string         `json:"evalSetItemId"`
			EvalSetItemName     string         `json:"evalSetItemName"`
			EvaluatorName       string         `json:"evaluatorName"`
			EvaluatorType       string         `json:"evaluatorType"`
			Status              string         `json:"status"`
			Score               *float64       `json:"score"`
			PerInvocationScores []*float64     `json:"perInvocationScores"`
			TraceID             *string        `json:"traceId,omitempty"`
			Details             map[string]any `json:"details,omitempty"`
			ErrorText           *string        `json:"errorText,omitempty"`
		}
		if err := backend.get(ctx, "/api/runs/"+url.PathEscape(runID)+"/results", &rows); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		out := getRunResultsResultMCP{
			RunID:      run.RunID,
			Status:     run.Status,
			Approach:   run.Spec.Approach,
			Error:      run.Error,
			Summary:    run.Summary,
			CreatedAt:  run.CreatedAt,
			StartedAt:  run.StartedAt,
			FinishedAt: run.FinishedAt,
			Results:    make([]runResultRowMCP, len(rows)),
		}
		for i, r := range rows {
			out.Results[i] = runResultRowMCP{
				ResultID:            r.ResultID,
				EvalSetItemID:       r.EvalSetItemID,
				EvalSetItemName:     r.EvalSetItemName,
				EvaluatorName:       r.EvaluatorName,
				EvaluatorType:       r.EvaluatorType,
				Status:              r.Status,
				Score:               r.Score,
				PerInvocationScores: r.PerInvocationScores,
				TraceID:             r.TraceID,
				Details:             r.Details,
				ErrorText:           r.ErrorText,
			}
		}
		return mcp.NewToolResultStructuredOnly(out), nil
	}
}
