package api

import (
	"encoding/json"
	"net/http"
	"os"
)

// configHandler implements GET /api/config, ported from api/routes.py's
// get_config: which provider API keys are present in the environment, so
// the UI can hint which judge-model-backed metrics are actually usable.
// anthropic/openai are reported for UI parity even though this port has no
// Anthropic/OpenAI-backed evaluator at all (see README.md).
//
// "google" also reports true when Vertex AI is configured
// (GOOGLE_GENAI_USE_VERTEXAI + GOOGLE_CLOUD_PROJECT), not just a literal
// API key: judge.NewGenAIModel works with either, via GKE Workload
// Identity/ADC and no key at all - see internal/judge/client.go. Python's
// own /api/config lacks this check too (it only ever looks at the literal
// key env vars), so this is a deliberate improvement over parity, made so
// the UI stops red-flagging a judge model that actually works.
func configHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data": map[string]any{
			"apiKeys": map[string]bool{
				"google":    os.Getenv("GOOGLE_API_KEY") != "" || os.Getenv("GEMINI_API_KEY") != "" || vertexAIConfigured(),
				"anthropic": os.Getenv("ANTHROPIC_API_KEY") != "",
				"openai":    os.Getenv("OPENAI_API_KEY") != "",
			},
		},
		"error": nil,
	})
}

// vertexAIConfigured reports whether the environment is set up for
// judge.NewGenAIModel to reach Vertex AI via Application Default
// Credentials instead of an API key (see google.golang.org/genai's
// backend auto-detection: GOOGLE_GENAI_USE_VERTEXAI truthy + a project to
// call against).
func vertexAIConfigured() bool {
	useVertex := os.Getenv("GOOGLE_GENAI_USE_VERTEXAI")
	truthy := useVertex == "true" || useVertex == "1"
	return truthy && os.Getenv("GOOGLE_CLOUD_PROJECT") != ""
}

// metricInfo mirrors api/models.py's MetricInfo (CamelModel field aliases).
type metricInfo struct {
	Name            string `json:"name"`
	Category        string `json:"category"`
	RequiresEvalSet bool   `json:"requiresEvalSet"`
	RequiresLLM     bool   `json:"requiresLLM"`
	RequiresGCP     bool   `json:"requiresGCP"`
	RequiresRubrics bool   `json:"requiresRubrics"`
	Description     string `json:"description"`
	Working         bool   `json:"working"`
}

// workingMetrics are the only metric names this Go port actually computes
// (internal/eval, internal/judge, internal/vertexeval) - only
// response_evaluation_score below is genuinely out of scope (not just
// unfinished, see docs/STATUS.md's "Not yet ported" section for why), so
// it's the sole allMetrics entry left out of this map.
var workingMetrics = map[string]bool{
	"tool_trajectory_avg_score":              true,
	"response_match_score":                   true,
	"final_response_match_v2":                true,
	"rubric_based_final_response_quality_v1": true,
	"rubric_based_tool_use_quality_v1":       true,
	"hallucinations_v1":                      true,
	"safety_v1":                              true,
	"multi_turn_task_success_v1":             true,
	"multi_turn_trajectory_quality_v1":       true,
	"multi_turn_tool_use_quality_v1":         true,
}

// allMetrics is every metric name google-adk's DEFAULT_METRIC_EVALUATOR_REGISTRY
// exposes (matching api/routes.py's list_metrics category map).
var allMetrics = []metricInfo{
	{Name: "tool_trajectory_avg_score", Category: "trajectory", RequiresEvalSet: true, Description: "Compares the agent's tool-call sequence against a golden reference (EXACT/IN_ORDER/ANY_ORDER)."},
	{Name: "response_match_score", Category: "response", RequiresEvalSet: true, Description: "ROUGE-1 F-measure between the agent's final response and a golden reference."},
	{Name: "response_evaluation_score", Category: "response", RequiresEvalSet: true, RequiresGCP: true, Description: "Vertex AI-judged response quality (:evaluateInstances)."},
	{Name: "final_response_match_v2", Category: "response", RequiresEvalSet: true, RequiresLLM: true, Description: "LLM-as-judge valid/invalid verdict on the final response vs. a golden reference, majority-voted over samples."},
	{Name: "rubric_based_final_response_quality_v1", Category: "quality", RequiresLLM: true, RequiresRubrics: true, Description: "LLM-as-judge scoring against user-supplied rubrics."},
	{Name: "rubric_based_tool_use_quality_v1", Category: "quality", RequiresLLM: true, RequiresRubrics: true, Description: "LLM-as-judge scoring of tool use against user-supplied rubrics."},
	{Name: "hallucinations_v1", Category: "safety", RequiresLLM: true, Description: "LLM-as-judge check for unsupported claims in the response."},
	{Name: "safety_v1", Category: "safety", RequiresGCP: true, Description: "Vertex AI safety classifier (:evaluateInstances)."},
	{Name: "multi_turn_task_success_v1", Category: "multi-turn", RequiresGCP: true, Description: "Vertex AI multi-turn task success judge."},
	{Name: "multi_turn_trajectory_quality_v1", Category: "multi-turn", RequiresGCP: true, Description: "Vertex AI multi-turn trajectory quality judge."},
	{Name: "multi_turn_tool_use_quality_v1", Category: "multi-turn", RequiresGCP: true, Description: "Vertex AI multi-turn tool-use quality judge."},
}

// metricsHandler implements GET /api/metrics.
func metricsHandler(w http.ResponseWriter, r *http.Request) {
	out := make([]metricInfo, len(allMetrics))
	for i, m := range allMetrics {
		m.Working = workingMetrics[m.Name]
		out[i] = m
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": out, "error": nil})
}

// authMeHandler implements GET /auth/me, ported from auth/routes.py's me:
// unlike every other gated route, a missing/invalid session is a normal
// 401 JSON body here, not a redirect - the UI's own "am I logged in" check
// (LiveStreamingView.tsx and friends) expects to react to a failed fetch,
// not follow it into an HTML login page. Session validity is checked here
// directly rather than via requireSession, since this route must be
// reachable (and answer honestly) precisely when there is no valid
// session.
func authMeHandler(sessionSecret string, ghTokens *githubTokenValidator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		username, ok := extractSessionUsername(r, sessionSecret, ghTokens)
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]bool{"authenticated": false})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"authenticated": true, "username": username})
	}
}
