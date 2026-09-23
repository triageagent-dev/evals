// Package judge implements agentevals' LLM-as-judge metrics natively in Go,
// via google.golang.org/genai (Google's official Go SDK) instead of
// litellm/Python. Ported from google-adk's llm_as_judge.py and its metric
// subclasses. Only final_response_match_v2 is ported so far; see
// README.md for the rest (hallucinations_v1, rubric_based_*_v1,
// per_turn_user_simulator_quality_v1).
package judge

import (
	"context"
	"fmt"

	"google.golang.org/genai"
)

// DefaultModel and DefaultNumSamples match google-adk's JudgeModelOptions
// defaults (eval_metrics.py): gemini-2.5-flash, 5 samples per invocation
// aggregated by majority vote.
const (
	DefaultModel      = "gemini-2.5-flash"
	DefaultNumSamples = 5
)

// Model is a single-turn text-in/text-out judge model call, abstracted so
// FinalResponseMatchV2 (and future judge metrics) can be tested against a
// fake without a live API key.
type Model interface {
	Generate(ctx context.Context, prompt string) (string, error)
}

// GenAIModel is a Model backed by the Gemini Developer API via
// google.golang.org/genai. Mirrors builtin_metrics.py's
// _build_judge_model for the Gemini-native (non-litellm) path - the only
// path this port needs, since Go has no equivalent to ADK Python's
// LiteLlm escape hatch for non-Gemini providers.
type GenAIModel struct {
	client *genai.Client
	model  string
}

// NewGenAIModel creates a judge model client. apiKey may be empty, in
// which case the underlying SDK auto-selects its backend from the
// environment: GOOGLE_GENAI_USE_VERTEXAI=true routes to Vertex AI using
// GOOGLE_CLOUD_PROJECT/GOOGLE_CLOUD_LOCATION and Application Default
// Credentials (e.g. a GKE Workload Identity service account - no API key
// anywhere), otherwise it falls back to the Gemini Developer API via
// GEMINI_API_KEY/GOOGLE_API_KEY. This mirrors ADK Python's default Gemini
// model class, which likewise builds a bare google.genai.Client() and lets
// the SDK's own env-based detection pick the backend (see
// builtin_metrics.py's _build_judge_model: explicit credential injection
// only happens when an api_key is actually resolved).
//
// An explicit apiKey (e.g. the CLI's --judge-api-key flag) always forces
// the Gemini Developer API backend, since a caller who hands us a key
// clearly wants it used rather than silently ignored in favor of ADC.
func NewGenAIModel(ctx context.Context, apiKey, model string) (*GenAIModel, error) {
	if model == "" {
		model = DefaultModel
	}
	cfg := &genai.ClientConfig{APIKey: apiKey}
	if apiKey != "" {
		cfg.Backend = genai.BackendGeminiAPI
	}
	client, err := genai.NewClient(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("creating genai client: %w", err)
	}
	return &GenAIModel{client: client, model: model}, nil
}

// Generate sends prompt as a single user-role turn and returns the
// response's concatenated text. Mirrors llm_as_judge.py's
// evaluate_invocations: one Content{role: "user", parts: [{text: prompt}]}
// request, non-streaming, default GenerateContentConfig.
func (m *GenAIModel) Generate(ctx context.Context, prompt string) (string, error) {
	resp, err := m.client.Models.GenerateContent(ctx, m.model,
		[]*genai.Content{genai.NewContentFromText(prompt, genai.RoleUser)},
		nil,
	)
	if err != nil {
		return "", fmt.Errorf("judge model %s: %w", m.model, err)
	}
	return resp.Text(), nil
}
