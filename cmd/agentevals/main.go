// Command agentevals is the Go rewrite of the agentevals CLI: score
// OpenTelemetry agent traces against a golden eval set, no re-execution, no
// Python. See README.md for what's ported and what isn't yet.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/triageagent-dev/agentevals-go/internal/adk"
	"github.com/triageagent-dev/agentevals-go/internal/api"
	"github.com/triageagent-dev/agentevals-go/internal/eval"
	"github.com/triageagent-dev/agentevals-go/internal/judge"
	tracepkg "github.com/triageagent-dev/agentevals-go/internal/trace"
	"github.com/triageagent-dev/agentevals-go/internal/vertexeval"
)

// judgeMetrics are the metric names the CLI routes to internal/judge
// instead of eval.RunMetrics, since they need a live judge model call
// (context, API key) that the local-only eval package deliberately has no
// dependency on. Every other metric name in eval.NotYetImplemented still
// goes through eval.RunMetrics and gets its existing "not yet ported" error.
var judgeMetrics = map[string]bool{
	"final_response_match_v2":                true,
	"hallucinations_v1":                      true,
	"rubric_based_final_response_quality_v1": true,
	"rubric_based_tool_use_quality_v1":       true,
}

// vertexEvalMetrics are the metric names the CLI routes to
// internal/vertexeval (Vertex AI's Managed Eval Service) instead of
// internal/judge or eval.RunMetrics - architecturally distinct from
// judgeMetrics, see internal/vertexeval's package doc.
var vertexEvalMetrics = map[string]bool{
	"safety_v1":                        true,
	"multi_turn_task_success_v1":       true,
	"multi_turn_trajectory_quality_v1": true,
	"multi_turn_tool_use_quality_v1":   true,
}

// runJudgeMetric dispatches one judgeMetrics-listed metric name to its
// internal/judge implementation. Mirrors internal/api/evaluate.go's
// runJudgeMetric.
func runJudgeMetric(ctx context.Context, name string, model judge.Model, actual, expected []adk.Invocation, rubrics []judge.Rubric, numSamples int, threshold float64) (eval.Result, error) {
	switch name {
	case "final_response_match_v2":
		if expected == nil {
			return eval.Result{}, fmt.Errorf("metric %q requires --eval-set (golden eval set)", name)
		}
		return judge.FinalResponseMatchV2(ctx, model, actual, expected, numSamples, threshold)
	case "hallucinations_v1":
		return judge.HallucinationsV1(ctx, model, actual, threshold)
	case "rubric_based_final_response_quality_v1":
		if len(rubrics) == 0 {
			return eval.Result{}, fmt.Errorf("metric %q requires at least one --rubric", name)
		}
		return judge.RubricBasedFinalResponseQualityV1(ctx, model, actual, rubrics, numSamples, threshold)
	case "rubric_based_tool_use_quality_v1":
		if len(rubrics) == 0 {
			return eval.Result{}, fmt.Errorf("metric %q requires at least one --rubric", name)
		}
		return judge.RubricBasedToolUseQualityV1(ctx, model, actual, rubrics, numSamples, threshold)
	default:
		return eval.Result{}, fmt.Errorf("unknown judge metric %q", name)
	}
}

// runVertexEvalMetric dispatches one vertexEvalMetrics-listed metric name
// to its internal/vertexeval implementation. Mirrors
// internal/api/evaluate.go's runVertexEvalMetric.
func runVertexEvalMetric(ctx context.Context, name string, client *vertexeval.Client, actual, expected []adk.Invocation, threshold float64) (eval.Result, error) {
	switch name {
	case "safety_v1":
		return vertexeval.SafetyV1(ctx, client, actual, expected, threshold)
	case "multi_turn_task_success_v1":
		return vertexeval.MultiTurnTaskSuccessV1(ctx, client, actual, threshold)
	case "multi_turn_trajectory_quality_v1":
		return vertexeval.MultiTurnTrajectoryQualityV1(ctx, client, actual, threshold)
	case "multi_turn_tool_use_quality_v1":
		return vertexeval.MultiTurnToolUseQualityV1(ctx, client, actual, threshold)
	default:
		return eval.Result{}, fmt.Errorf("unknown vertex-eval metric %q", name)
	}
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "run":
		err = runCmd(os.Args[2:])
	case "serve":
		err = serveCmd(os.Args[2:])
	case "auth":
		err = authCmd(os.Args[2:])
	case "mcp":
		err = mcpCmd(os.Args[2:])
	case "help", "-h", "--help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "agentevals: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "agentevals: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `agentevals-go - score OTel agent traces against a golden eval set.

Usage:
  agentevals run <trace-file>... --eval-set <path> -m <metric> [-m <metric>...] [flags]
  agentevals serve [--addr :8001] [--otlp-addr :4318] [--otlp-grpc-addr :4317] [--health-addr :9090] [--session-db path] [--session-secret secret]
  agentevals auth mint-token --name <label> [--ttl-days 3650] [--secret ...]
  agentevals mcp [--server-url http://localhost:8001] [--session-token ...]

Run flags:
  --eval-set string            golden eval set JSON (required for tool_trajectory_avg_score, response_match_score,
                                final_response_match_v2)
  -m, --metric value            metric to run (repeatable); default: tool_trajectory_avg_score
  --trajectory-match-type value EXACT | IN_ORDER | ANY_ORDER (default EXACT)
  --threshold float              pass/fail threshold applied to every metric (default 0.5)
  --output string                 text | json (default text)
  --judge-api-key string          Gemini API key for judge-model metrics (falls back to GEMINI_API_KEY/GOOGLE_API_KEY,
                                    or ADC/Vertex if GOOGLE_GENAI_USE_VERTEXAI=true - no key needed)
  --judge-model string             judge model for judge-model metrics (default gemini-2.5-flash)
  --judge-samples int               samples per invocation for judge-model metrics, majority-voted (default 5)
  --rubric string                  rubric text for rubric_based_*_v1 metrics (repeatable); required by those metrics

Metrics needing a live judge model (final_response_match_v2, hallucinations_v1, rubric_based_final_response_quality_v1,
rubric_based_tool_use_quality_v1) call Gemini directly. Metrics needing Vertex AI's Managed Eval Service (safety_v1,
multi_turn_task_success_v1, multi_turn_trajectory_quality_v1, multi_turn_tool_use_quality_v1) read
GOOGLE_CLOUD_PROJECT/GOOGLE_CLOUD_LOCATION and use Application Default Credentials - no API key, ever.

Only trace files in Jaeger JSON format, from ADK-instrumented (gcp.vertex.agent.*) traces, are supported today.
See README.md for what agentevals (Python) supports that this port does not yet.

MCP: agentevals mcp starts a Model Context Protocol server on stdio (list_metrics,
evaluate_traces, list_sessions, summarize_session) for use with Claude Code, Cursor, GitHub Copilot
CLI, etc. list_sessions/summarize_session call --server-url/AGENTEVALS_SERVER_URL (a running
'agentevals serve'); pass --session-token/AGENTEVALS_SESSION_TOKEN (from 'agentevals auth
mint-token') if that server has GitHub OAuth enabled. evaluate_traces is fully offline.
`)
}

type traceReport struct {
	TraceID string        `json:"trace_id"`
	Results []eval.Result `json:"results"`
}

type metricFlags []string

func (m *metricFlags) String() string { return strings.Join(*m, ",") }
func (m *metricFlags) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// runFlagsWithValue lists every "run" flag that consumes the following
// token as its value (none of them are boolean). Used by splitPositional
// to pull trace-file paths out regardless of where they fall relative to
// the flags, since Go's flag package (unlike click/argparse) stops parsing
// flags at the first non-flag argument.
var runFlagsWithValue = map[string]bool{
	"eval-set":              true,
	"m":                     true,
	"metric":                true,
	"trajectory-match-type": true,
	"threshold":             true,
	"output":                true,
	"judge-api-key":         true,
	"judge-model":           true,
	"judge-samples":         true,
	"rubric":                true,
}

// splitPositional separates positional arguments (trace file paths) from
// flag tokens, so `agentevals run trace.json --eval-set set.json` and
// `agentevals run --eval-set set.json trace.json` both work.
func splitPositional(args []string, flagsWithValue map[string]bool) (positional, flagArgs []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
			continue
		}
		flagArgs = append(flagArgs, a)
		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") {
			continue // value is embedded in this token (--flag=value)
		}
		if flagsWithValue[name] && i+1 < len(args) {
			i++
			flagArgs = append(flagArgs, args[i])
		}
	}
	return positional, flagArgs
}

func runCmd(args []string) error {
	traceFiles, flagArgs := splitPositional(args, runFlagsWithValue)

	fs := flag.NewFlagSet("run", flag.ExitOnError)
	evalSetPath := fs.String("eval-set", "", "golden eval set JSON path")
	var metrics metricFlags
	fs.Var(&metrics, "m", "metric to run (repeatable)")
	fs.Var(&metrics, "metric", "metric to run (repeatable)")
	matchTypeFlag := fs.String("trajectory-match-type", "EXACT", "EXACT | IN_ORDER | ANY_ORDER")
	threshold := fs.Float64("threshold", 0.5, "pass/fail threshold")
	output := fs.String("output", "text", "text | json")
	judgeAPIKey := fs.String("judge-api-key", "", "Gemini API key for judge-model metrics; falls back to GEMINI_API_KEY/GOOGLE_API_KEY, or ADC/Vertex if GOOGLE_GENAI_USE_VERTEXAI=true")
	judgeModel := fs.String("judge-model", judge.DefaultModel, "judge model for judge-model metrics")
	judgeSamples := fs.Int("judge-samples", judge.DefaultNumSamples, "samples per invocation for judge-model metrics, aggregated by majority vote")
	var rubrics metricFlags
	fs.Var(&rubrics, "rubric", "rubric text for rubric_based_*_v1 metrics (repeatable); required by those metrics")
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}

	if len(traceFiles) == 0 {
		return fmt.Errorf("at least one trace file is required")
	}
	if len(metrics) == 0 {
		metrics = metricFlags{"tool_trajectory_avg_score"}
	}

	matchType, err := eval.ParseMatchType(*matchTypeFlag)
	if err != nil {
		return err
	}

	var evalSet *adk.EvalSet
	if *evalSetPath != "" {
		evalSet, err = eval.LoadEvalSet(*evalSetPath)
		if err != nil {
			return fmt.Errorf("loading eval set: %w", err)
		}
	}

	thresholds := make(map[string]float64, len(metrics))
	for _, m := range metrics {
		thresholds[m] = *threshold
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
		client, err := judge.NewGenAIModel(context.Background(), *judgeAPIKey, *judgeModel)
		if err != nil {
			return fmt.Errorf("setting up judge model: %w", err)
		}
		judgeModelClient = client
	}

	var vertexEvalClient *vertexeval.Client
	if len(requestedVertexMetrics) > 0 {
		client, err := vertexeval.NewClient(context.Background())
		if err != nil {
			return fmt.Errorf("setting up Vertex AI Managed Eval Service client: %w", err)
		}
		vertexEvalClient = client
	}

	rubricObjects := judge.RubricsFromStrings(rubrics)

	var reports []traceReport
	failures := 0

	for _, path := range traceFiles {
		traces, err := tracepkg.LoadJaegerJSON(path)
		if err != nil {
			return fmt.Errorf("loading %s: %w", path, err)
		}
		for _, tr := range traces {
			conv := adk.ConvertADKTrace(tr)
			for _, w := range conv.Warnings {
				fmt.Fprintf(os.Stderr, "warning: %s\n", w)
			}

			var results []eval.Result
			if len(localMetrics) > 0 {
				results = append(results, eval.RunMetrics(conv.Invocations, evalSet, localMetrics, matchType, thresholds)...)
			}

			expected := eval.FindExpectedInvocations(conv.Invocations, evalSet)
			for _, m := range requestedJudgeMetrics {
				result, err := runJudgeMetric(context.Background(), m, judgeModelClient, conv.Invocations, expected, rubricObjects, *judgeSamples, thresholds[m])
				if err != nil {
					return fmt.Errorf("running %s: %w", m, err)
				}
				results = append(results, result)
			}
			for _, m := range requestedVertexMetrics {
				result, err := runVertexEvalMetric(context.Background(), m, vertexEvalClient, conv.Invocations, expected, thresholds[m])
				if err != nil {
					return fmt.Errorf("running %s: %w", m, err)
				}
				results = append(results, result)
			}

			for _, r := range results {
				if r.Error != "" || r.Status == eval.StatusFailed {
					failures++
				}
			}
			reports = append(reports, traceReport{TraceID: tr.TraceID, Results: results})
		}
	}

	if *output == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(reports); err != nil {
			return err
		}
	} else {
		printTextReport(reports)
	}

	if failures > 0 {
		os.Exit(1)
	}
	return nil
}

func printTextReport(reports []traceReport) {
	for _, r := range reports {
		fmt.Printf("Trace: %s\n", r.TraceID)
		for _, res := range r.Results {
			if res.Error != "" {
				fmt.Printf("[ERROR] %-30s %s\n", res.MetricName, res.Error)
				continue
			}
			status := "PASS"
			if res.Status == eval.StatusFailed {
				status = "FAIL"
			}
			fmt.Printf("[%s]  %-30s score=%.2f status=%s\n", status, res.MetricName, res.Score, res.Status)
		}
		fmt.Println()
	}
}

func serveCmd(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8001", "listen address for the REST API and UI (behind auth when --session-secret is set)")
	otlpAddr := fs.String("otlp-addr", ":4318", "listen address for the OTLP/HTTP trace receiver (never gated)")
	otlpGRPCAddr := fs.String("otlp-grpc-addr", ":4317", "listen address for the OTLP/gRPC trace receiver (never gated)")
	healthAddr := fs.String("health-addr", ":9090", "listen address for GET /api/health (its own port, always unauthenticated - see internal/api/server.go's Serve doc comment for why)")
	sessionDB := fs.String("session-db", os.Getenv("AGENTEVALS_SESSION_DB_PATH"),
		"SQLite file to persist streaming sessions across restarts (default: none, in-memory only); falls back to AGENTEVALS_SESSION_DB_PATH")
	sessionSecret := fs.String("session-secret", os.Getenv("AGENTEVALS_SESSION_SECRET"),
		"HMAC secret used to sign/verify the agentevals_session cookie (default: none, unauthenticated); falls back to AGENTEVALS_SESSION_SECRET. Required (along with --github-client-id/--github-client-secret/--github-org/--public-url) to enable this service's own independent GitHub OAuth login flow; see internal/api/oauth.go.")
	githubClientID := fs.String("github-client-id", os.Getenv("AGENTEVALS_GITHUB_CLIENT_ID"),
		"GitHub OAuth App client ID; falls back to AGENTEVALS_GITHUB_CLIENT_ID. Setting this and --github-client-secret enables this service's own /auth/login, /auth/callback and /auth/logout - no other process is needed to log a user in.")
	githubClientSecret := fs.String("github-client-secret", os.Getenv("AGENTEVALS_GITHUB_CLIENT_SECRET"),
		"GitHub OAuth App client secret; falls back to AGENTEVALS_GITHUB_CLIENT_SECRET.")
	githubOrg := fs.String("github-org", os.Getenv("AGENTEVALS_GITHUB_ORG"),
		"GitHub org whose active members are granted access once OAuth is enabled; falls back to AGENTEVALS_GITHUB_ORG. Required whenever --github-client-id/--github-client-secret are set.")
	publicURL := fs.String("public-url", os.Getenv("AGENTEVALS_PUBLIC_URL"),
		"this service's own externally-reachable base URL, e.g. https://your-app.example.com, used to build an exact OAuth redirect_uri (PublicURL+\"/auth/callback\"); falls back to AGENTEVALS_PUBLIC_URL. Required whenever --github-client-id/--github-client-secret are set (no fallback is derived from the incoming request - see internal/api/oauth.go's package doc).")
	if err := fs.Parse(args); err != nil {
		return err
	}
	githubOAuth, err := api.GitHubOAuthConfigFromParams(*githubClientID, *githubClientSecret, *githubOrg, *sessionSecret, *publicURL)
	if err != nil {
		return fmt.Errorf("github OAuth config: %w", err)
	}
	return api.Serve(*addr, *otlpAddr, *otlpGRPCAddr, *healthAddr, *sessionDB, *sessionSecret, githubOAuth)
}
