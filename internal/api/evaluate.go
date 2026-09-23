package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"time"

	"github.com/triageagent-dev/agentevals-go/internal/adk"
	"github.com/triageagent-dev/agentevals-go/internal/eval"
	"github.com/triageagent-dev/agentevals-go/internal/judge"
	"github.com/triageagent-dev/agentevals-go/internal/loader"
	tracepkg "github.com/triageagent-dev/agentevals-go/internal/trace"
	"github.com/triageagent-dev/agentevals-go/internal/vertexeval"
)

// maxUploadFileSize matches api/routes.py's evaluate_traces per-file cap.
const maxUploadFileSize = 10 << 20 // 10MB

// evaluatorConfigDTO is one entry in POST /api/evaluate's "config" form
// field, matching ui/src/lib/types.ts's EvalConfig.evaluators. Only
// type: "builtin" evaluators are supported - code/remote/OpenAI evaluator
// types aren't ported (see README.md) and are reported as a per-metric
// error rather than silently skipped.
type evaluatorConfigDTO struct {
	Type                string   `json:"type"`
	Name                string   `json:"name"`
	JudgeModel          string   `json:"judgeModel"`
	Threshold           *float64 `json:"threshold"`
	TrajectoryMatchType string   `json:"trajectoryMatchType"`
	// Rubrics is required by rubric_based_final_response_quality_v1 and
	// rubric_based_tool_use_quality_v1 (a plain list of rubric-text
	// strings; see judge.RubricsFromStrings). Not yet in
	// ui/src/lib/types.ts's EvalConfig - the UI only shows a "Requires
	// Rubrics" badge (MetricSelector.tsx) without a way to supply them
	// yet - so this field is additive/backend-only for now, usable by
	// direct API callers.
	Rubrics []string `json:"rubrics,omitempty"`
}

// evalConfigDTO is POST /api/evaluate's "config" form field, matching
// ui/src/lib/types.ts's EvalConfig.
type evalConfigDTO struct {
	Evaluators []evaluatorConfigDTO `json:"evaluators"`
}

// metricResultDTO mirrors ui/src/lib/types.ts's MetricResult (CamelModel
// field names) built from an internal/eval.Result.
type metricResultDTO struct {
	MetricName          string         `json:"metricName"`
	Score               *float64       `json:"score"`
	EvalStatus          string         `json:"evalStatus"`
	PerInvocationScores []*float64     `json:"perInvocationScores"`
	Error               *string        `json:"error"`
	Details             map[string]any `json:"details,omitempty"`
	// EvaluatorKind and JudgeModel explain *how* the score was produced -
	// "algorithm" (deterministic Go code, e.g. tool_trajectory_avg_score),
	// "llm_judge" (a live judge-model call via internal/judge, with
	// JudgeModel naming the actual model used), or "vertex_judge" (Vertex
	// AI's Managed Eval Service :evaluateInstances RPC). Surfaced so the
	// UI can show users which results are deterministic vs. LLM-judged,
	// answering "why/how was this score produced" (see
	// MetricsComparisonSection.tsx).
	EvaluatorKind string `json:"evaluatorKind"`
	JudgeModel    string `json:"judgeModel,omitempty"`
}

func toMetricResultDTO(r eval.Result, kind, judgeModel string) metricResultDTO {
	dto := metricResultDTO{
		MetricName:          r.MetricName,
		PerInvocationScores: make([]*float64, len(r.PerInvocationScores)),
		Details:             r.Details,
		EvaluatorKind:       kind,
		JudgeModel:          judgeModel,
	}
	for i, s := range r.PerInvocationScores {
		s := s
		dto.PerInvocationScores[i] = &s
	}
	if r.Error != "" {
		errCopy := r.Error
		dto.Error = &errCopy
		dto.EvalStatus = "ERROR"
		return dto
	}
	score := r.Score
	dto.Score = &score
	dto.EvalStatus = string(r.Status)
	return dto
}

// traceResultDTO mirrors ui/src/lib/types.ts's TraceResult. Performance
// metrics and agent-identity fields (agentName, model, provider, ...)
// aren't populated yet - see README.md.
type traceResultDTO struct {
	TraceID            string            `json:"traceId"`
	NumInvocations     int               `json:"numInvocations"`
	MetricResults      []metricResultDTO `json:"metricResults"`
	ConversionWarnings []string          `json:"conversionWarnings"`
}

// runResultDTO mirrors ui/src/lib/types.ts's RunResult.
type runResultDTO struct {
	TraceResults []traceResultDTO `json:"traceResults"`
	Errors       []string         `json:"errors"`
}

// judgeMetrics are the metric names routed to internal/judge instead of
// eval.RunMetric, since they need a live judge-model call. Mirrors
// cmd/agentevals/main.go's judgeMetrics map.
var judgeMetrics = map[string]bool{
	"final_response_match_v2":                true,
	"hallucinations_v1":                      true,
	"rubric_based_final_response_quality_v1": true,
	"rubric_based_tool_use_quality_v1":       true,
}

// vertexEvalMetrics are the metric names routed to internal/vertexeval
// (Vertex AI's Managed Eval Service :evaluateInstances RPC) instead of a
// direct judge-model call - architecturally distinct from judgeMetrics,
// see internal/vertexeval's package doc. Mirrors
// cmd/agentevals/main.go's vertexEvalMetrics map.
var vertexEvalMetrics = map[string]bool{
	"safety_v1":                        true,
	"multi_turn_task_success_v1":       true,
	"multi_turn_trajectory_quality_v1": true,
	"multi_turn_tool_use_quality_v1":   true,
}

// vertexEvalServiceLabel names the "model" for vertex_judge results: the
// Vertex Managed Eval Service doesn't accept a caller-chosen model name
// (unlike judgeMetrics), it's a fixed managed backend.
const vertexEvalServiceLabel = "Vertex AI Managed Eval Service"

// metricKind classifies how a builtin metric's score is produced, so the
// UI can tell users whether a result is deterministic ("algorithm") or
// came from a live LLM-as-judge call ("llm_judge") or Vertex AI's Managed
// Eval Service ("vertex_judge").
func metricKind(name string) string {
	switch {
	case judgeMetrics[name]:
		return "llm_judge"
	case vertexEvalMetrics[name]:
		return "vertex_judge"
	default:
		return "algorithm"
	}
}

// evaluateRequest is the parsed, validated form of a POST /api/evaluate or
// POST /api/evaluate/stream request, shared by both handlers so the
// multipart parsing / file loading logic (identical in both Python
// routes) isn't duplicated.
type evaluateRequest struct {
	cfg        evalConfigDTO
	traces     []*tracepkg.Trace
	evalSet    *adk.EvalSet
	loadErrors []string
	// traceFilenames maps each trace's TraceID to its source upload
	// filename, used only for Run History's evalSetItemName (see runs.go);
	// evaluation itself never needs it.
	traceFilenames map[string]string
}

// parseEvaluateRequest reads and validates the "trace_files"/"config"/
// "eval_set_file" multipart form fields shared by /api/evaluate and
// /api/evaluate/stream. Returns an HTTP status + message for the caller to
// report (via a plain JSON error or an SSE error event) when the request
// itself is malformed; per-file load failures are instead accumulated into
// loadErrors, matching Python's behavior of still running the evaluation
// over whatever traces did load.
func parseEvaluateRequest(r *http.Request) (*evaluateRequest, int, string) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		return nil, http.StatusBadRequest, "invalid multipart form: " + err.Error()
	}

	configRaw := r.FormValue("config")
	if configRaw == "" {
		return nil, http.StatusBadRequest, "config is required"
	}
	var cfg evalConfigDTO
	if err := json.Unmarshal([]byte(configRaw), &cfg); err != nil {
		return nil, http.StatusBadRequest, "invalid config JSON: " + err.Error()
	}

	traceFileHeaders := r.MultipartForm.File["trace_files"]
	if len(traceFileHeaders) == 0 {
		return nil, http.StatusBadRequest, "No valid trace files provided"
	}

	var traces []*tracepkg.Trace
	var loadErrors []string
	traceFilenames := map[string]string{}
	for _, fh := range traceFileHeaders {
		content, err := readMultipartFile(fh, maxUploadFileSize)
		if err != nil {
			loadErrors = append(loadErrors, fmt.Sprintf("Failed to read trace file %q: %s", fh.Filename, err))
			continue
		}
		parsed, err := loader.ParseFile(fh.Filename, content, "")
		if err != nil {
			loadErrors = append(loadErrors, fmt.Sprintf("Failed to load trace file %q: %s", fh.Filename, err))
			continue
		}
		for _, t := range parsed {
			traceFilenames[t.TraceID] = fh.Filename
		}
		traces = append(traces, parsed...)
	}

	var evalSet *adk.EvalSet
	if evalSetHeaders := r.MultipartForm.File["eval_set_file"]; len(evalSetHeaders) > 0 {
		content, err := readMultipartFile(evalSetHeaders[0], maxUploadFileSize)
		if err != nil {
			loadErrors = append(loadErrors, fmt.Sprintf("Failed to read eval set file: %s", err))
		} else {
			var set adk.EvalSet
			if err := json.Unmarshal(content, &set); err != nil {
				loadErrors = append(loadErrors, fmt.Sprintf("Failed to parse eval set file: %s", err))
			} else {
				evalSet = &set
			}
		}
	}

	return &evaluateRequest{cfg: cfg, traces: traces, evalSet: evalSet, loadErrors: loadErrors, traceFilenames: traceFilenames}, 0, ""
}

// newJudgeModelGetter returns a memoized per-model-name judge.Model
// factory, shared by evaluateHandler and evaluateStreamHandler so each
// distinct judgeModel named across a request's evaluators is only
// constructed (and thus authenticated) once.
func newJudgeModelGetter(ctx context.Context) func(string) (judge.Model, error) {
	judgeClients := map[string]judge.Model{}
	return func(modelName string) (judge.Model, error) {
		if modelName == "" {
			modelName = judge.DefaultModel
		}
		if m, ok := judgeClients[modelName]; ok {
			return m, nil
		}
		m, err := judge.NewGenAIModel(ctx, "", modelName)
		if err != nil {
			return nil, fmt.Errorf("setting up judge model %s: %w", modelName, err)
		}
		judgeClients[modelName] = m
		return m, nil
	}
}

// newVertexEvalClientGetter returns a memoized vertexeval.Client factory
// (there's only ever one: the Vertex Managed Eval Service client has no
// per-evaluator model-name variance, unlike judge.Model), shared by
// evaluateHandler and evaluateStreamHandler so it's only constructed
// (and thus authenticated) once per request even if several evaluators
// need it.
func newVertexEvalClientGetter(ctx context.Context) func() (*vertexeval.Client, error) {
	var client *vertexeval.Client
	var err error
	var built bool
	return func() (*vertexeval.Client, error) {
		if !built {
			client, err = vertexeval.NewClient(ctx)
			built = true
		}
		return client, err
	}
}

// runJudgeMetric dispatches one judgeMetrics-listed metric name to its
// internal/judge implementation.
func runJudgeMetric(ctx context.Context, name string, model judge.Model, actual, expected []adk.Invocation, rubricTexts []string, numSamples int, threshold float64) eval.Result {
	switch name {
	case "final_response_match_v2":
		if expected == nil {
			return eval.Result{MetricName: name, Error: fmt.Sprintf(
				"Metric '%s' requires expected invocations (golden eval set), but none were provided or matched.", name)}
		}
		r, err := judge.FinalResponseMatchV2(ctx, model, actual, expected, numSamples, threshold)
		if err != nil {
			return eval.Result{MetricName: name, Error: err.Error()}
		}
		return r
	case "hallucinations_v1":
		r, err := judge.HallucinationsV1(ctx, model, actual, threshold)
		if err != nil {
			return eval.Result{MetricName: name, Error: err.Error()}
		}
		return r
	case "rubric_based_final_response_quality_v1", "rubric_based_tool_use_quality_v1":
		rubrics := judge.RubricsFromStrings(rubricTexts)
		if len(rubrics) == 0 {
			return eval.Result{MetricName: name, Error: "Rubrics are required for this metric; supply evaluator.rubrics (a list of rubric-text strings)."}
		}
		var r eval.Result
		var err error
		if name == "rubric_based_final_response_quality_v1" {
			r, err = judge.RubricBasedFinalResponseQualityV1(ctx, model, actual, rubrics, numSamples, threshold)
		} else {
			r, err = judge.RubricBasedToolUseQualityV1(ctx, model, actual, rubrics, numSamples, threshold)
		}
		if err != nil {
			return eval.Result{MetricName: name, Error: err.Error()}
		}
		return r
	default:
		return eval.Result{MetricName: name, Error: fmt.Sprintf("unknown judge metric %q", name)}
	}
}

// runVertexEvalMetric dispatches one vertexEvalMetrics-listed metric name
// to its internal/vertexeval implementation.
func runVertexEvalMetric(ctx context.Context, name string, client *vertexeval.Client, actual, expected []adk.Invocation, threshold float64) eval.Result {
	var r eval.Result
	var err error
	switch name {
	case "safety_v1":
		r, err = vertexeval.SafetyV1(ctx, client, actual, expected, threshold)
	case "multi_turn_task_success_v1":
		r, err = vertexeval.MultiTurnTaskSuccessV1(ctx, client, actual, threshold)
	case "multi_turn_trajectory_quality_v1":
		r, err = vertexeval.MultiTurnTrajectoryQualityV1(ctx, client, actual, threshold)
	case "multi_turn_tool_use_quality_v1":
		r, err = vertexeval.MultiTurnToolUseQualityV1(ctx, client, actual, threshold)
	default:
		return eval.Result{MetricName: name, Error: fmt.Sprintf("unknown vertex-eval metric %q", name)}
	}
	if err != nil {
		return eval.Result{MetricName: name, Error: err.Error()}
	}
	return r
}

// evaluateHandler implements POST /api/evaluate: scores every uploaded
// trace file (Jaeger or OTLP JSON/JSONL, auto-detected) against an
// optional golden eval set, using the evaluator list in the "config" form
// field. Ported from api/routes.py's evaluate_traces / runner.py's
// run_evaluation, minus credential_refs (this port has no per-request
// credential store - judge-model auth is environment-wide, see
// internal/judge/client.go) and the performance-metrics aggregation (see
// README.md).
func evaluateHandler(store *SQLiteStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		req, status, msg := parseEvaluateRequest(r)
		if r.MultipartForm != nil {
			defer r.MultipartForm.RemoveAll()
		}
		if msg != "" {
			writeEvaluateError(w, status, msg)
			return
		}

		if len(req.traces) == 0 {
			writeJSON(w, http.StatusOK, map[string]any{
				"data":  runResultDTO{Errors: append(req.loadErrors, "No traces loaded.")},
				"error": nil,
			})
			return
		}

		ctx := r.Context()
		getJudgeModel := newJudgeModelGetter(ctx)
		getVertexEvalClient := newVertexEvalClientGetter(ctx)

		result := runResultDTO{Errors: req.loadErrors}
		for _, tr := range req.traces {
			result.TraceResults = append(result.TraceResults, evaluateOneTrace(ctx, tr, req.cfg.Evaluators, req.evalSet, getJudgeModel, getVertexEvalClient, nil))
		}

		if store != nil {
			if err := persistCompletedRun(store, runSpecFromRequest(req), req, result); err != nil {
				log.Printf("run history: failed to persist run: %v", err)
			}
		}

		writeJSON(w, http.StatusOK, map[string]any{"data": result, "error": nil})
	}
}

// evaluateStreamHandler implements POST /api/evaluate/stream: identical
// scoring to evaluateHandler, but reports progress over Server-Sent
// Events as each trace/metric completes instead of a single response at
// the end. Ported from api/routes.py's evaluate_traces_stream. The UI
// (ui/src/context/TraceProvider.tsx) always calls this endpoint, not
// POST /api/evaluate directly.
//
// Unlike Python's asyncio queue + 15s idle timeout, the evaluation runs in
// its own goroutine while this handler's main loop selects between that
// goroutine's events channel and a 15s ticker, emitting an SSE comment
// (": ping") on tick. This keeps the connection flushing during a slow
// judge-model call (30-40s+) so an intermediate proxy buffering an idle
// streaming response doesn't silently swallow the eventual "done" event.
func evaluateStreamHandler(store *SQLiteStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		req, _, msg := parseEvaluateRequest(r)
		if r.MultipartForm != nil {
			defer r.MultipartForm.RemoveAll()
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)

		if msg != "" {
			writeSSEData(w, map[string]any{"error": msg})
			flusher.Flush()
			return
		}

		ctx := r.Context()
		events := make(chan map[string]any, 16)

		go func() {
			defer close(events)

			if len(req.traces) == 0 {
				events <- map[string]any{"error": "No traces loaded."}
				return
			}

			getJudgeModel := newJudgeModelGetter(ctx)
			getVertexEvalClient := newVertexEvalClientGetter(ctx)
			total := len(req.traces)
			events <- map[string]any{"message": fmt.Sprintf("Evaluating %d trace(s)...", total)}

			result := runResultDTO{Errors: req.loadErrors}
			for idx, tr := range req.traces {
				events <- map[string]any{"message": fmt.Sprintf("Trace %d/%d: %s", idx+1, total, tr.TraceID)}

				traceResult := evaluateOneTrace(ctx, tr, req.cfg.Evaluators, req.evalSet, getJudgeModel, getVertexEvalClient, func(partial traceResultDTO) {
					events <- map[string]any{"traceProgress": map[string]any{
						"traceId":       partial.TraceID,
						"partialResult": partial,
					}}
				})
				result.TraceResults = append(result.TraceResults, traceResult)
			}

			if store != nil {
				if err := persistCompletedRun(store, runSpecFromRequest(req), req, result); err != nil {
					log.Printf("run history: failed to persist run: %v", err)
				}
			}

			events <- map[string]any{"message": "Evaluation complete"}
			events <- map[string]any{"done": true, "result": result}
		}()

		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case ev, open := <-events:
				if !open {
					return
				}
				writeSSEData(w, ev)
				flusher.Flush()
				if ev["done"] != nil || ev["error"] != nil {
					return
				}
			case <-ticker.C:
				fmt.Fprint(w, ": ping\n\n")
				flusher.Flush()
			case <-ctx.Done():
				return
			}
		}
	}
}

// writeSSEData writes a single bare "data: <json>\n\n" SSE event (no
// "event:" line - the UI's parser only special-cases performance_metrics,
// which this port doesn't emit; see README.md).
func writeSSEData(w http.ResponseWriter, payload map[string]any) {
	data, err := json.Marshal(payload)
	if err != nil {
		data = []byte(fmt.Sprintf(`{"error":%q}`, "failed to encode event: "+err.Error()))
	}
	fmt.Fprintf(w, "data: %s\n\n", data)
}

// evaluateOneTrace scores a single trace against evaluators, optionally
// invoking onMetric after each metric result is appended (streaming
// progress) - mirrors runner.py's _evaluate_trace's trace_progress_callback,
// called incrementally per metric rather than once per trace.
func evaluateOneTrace(
	ctx context.Context,
	tr *tracepkg.Trace,
	evaluators []evaluatorConfigDTO,
	evalSet *adk.EvalSet,
	getJudgeModel func(string) (judge.Model, error),
	getVertexEvalClient func() (*vertexeval.Client, error),
	onMetric func(traceResultDTO),
) traceResultDTO {
	conv := adk.ConvertTrace(tr)
	traceResult := traceResultDTO{
		TraceID:        tr.TraceID,
		NumInvocations: len(conv.Invocations),
		// append onto a non-nil empty slice (rather than assigning conv.Warnings
		// directly) so a nil conv.Warnings still marshals as JSON [] not null -
		// the UI reads conversionWarnings.length without optional chaining.
		ConversionWarnings: append([]string{}, conv.Warnings...),
		MetricResults:      []metricResultDTO{},
	}

	if len(conv.Invocations) == 0 {
		traceResult.MetricResults = []metricResultDTO{toMetricResultDTO(eval.Result{
			MetricName: "(all)",
			Error:      "No invocations extracted from trace.",
		}, "", "")}
		return traceResult
	}

	expected := eval.FindExpectedInvocations(conv.Invocations, evalSet)

	for _, evaluator := range evaluators {
		if evaluator.Type != "" && evaluator.Type != "builtin" {
			traceResult.MetricResults = append(traceResult.MetricResults, toMetricResultDTO(eval.Result{
				MetricName: evaluator.Name,
				Error:      fmt.Sprintf("evaluator type %q is not supported (only builtin evaluators are ported; see README.md)", evaluator.Type),
			}, "", ""))
			notifyMetric(traceResult, onMetric)
			continue
		}

		threshold := 0.5
		if evaluator.Threshold != nil {
			threshold = *evaluator.Threshold
		}

		kind := metricKind(evaluator.Name)
		judgeModelUsed := ""

		var result eval.Result
		switch {
		case judgeMetrics[evaluator.Name]:
			modelName := evaluator.JudgeModel
			if modelName == "" {
				modelName = judge.DefaultModel
			}
			judgeModelUsed = modelName
			model, err := getJudgeModel(evaluator.JudgeModel)
			if err != nil {
				result = eval.Result{MetricName: evaluator.Name, Error: err.Error()}
			} else {
				result = runJudgeMetric(ctx, evaluator.Name, model, conv.Invocations, expected, evaluator.Rubrics, judge.DefaultNumSamples, threshold)
			}
		case vertexEvalMetrics[evaluator.Name]:
			judgeModelUsed = vertexEvalServiceLabel
			client, err := getVertexEvalClient()
			if err != nil {
				result = eval.Result{MetricName: evaluator.Name, Error: err.Error()}
			} else {
				result = runVertexEvalMetric(ctx, evaluator.Name, client, conv.Invocations, expected, threshold)
			}
		default:
			matchType, err := eval.ParseMatchType(evaluator.TrajectoryMatchType)
			if err != nil {
				result = eval.Result{MetricName: evaluator.Name, Error: err.Error()}
			} else {
				result = eval.RunMetric(evaluator.Name, conv.Invocations, expected, matchType, threshold)
			}
		}
		traceResult.MetricResults = append(traceResult.MetricResults, toMetricResultDTO(result, kind, judgeModelUsed))
		notifyMetric(traceResult, onMetric)
	}

	return traceResult
}

// notifyMetric calls onMetric (if non-nil) with a snapshot of tr whose
// MetricResults slice is copied. Without this, since onMetric hands the
// value to a buffered channel consumed by a separate goroutine, later
// append() calls in evaluateOneTrace's loop could reuse the same backing
// array and mutate an already-queued-but-not-yet-marshaled event.
func notifyMetric(tr traceResultDTO, onMetric func(traceResultDTO)) {
	if onMetric == nil {
		return
	}
	snapshot := tr
	snapshot.MetricResults = append([]metricResultDTO(nil), tr.MetricResults...)
	onMetric(snapshot)
}

// readMultipartFile reads an uploaded file's content, rejecting it once
// maxSize bytes have been read (matching api/routes.py's per-file 10MB
// cap) rather than buffering an arbitrarily large upload.
func readMultipartFile(fh *multipart.FileHeader, maxSize int64) ([]byte, error) {
	f, err := fh.Open()
	if err != nil {
		return nil, err
	}
	defer f.Close()

	limited := io.LimitReader(f, maxSize+1)
	content, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > maxSize {
		return nil, fmt.Errorf("exceeds %dMB limit", maxSize/(1<<20))
	}
	return content, nil
}

func writeEvaluateError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"data": nil, "error": message})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
