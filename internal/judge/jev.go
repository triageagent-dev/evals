package judge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Jev support is additive over Python (agentevals has no equivalent):
// TypeSafe's Jev (typesafe/jev-*) is a decision model served through
// OpenRouter's Decisions API. Unlike a text judge it takes a structured
// "state" object plus typed questions and returns typed answers with
// probabilities - no prompt text, no free-text output. So instead of
// sending the google-adk prompt templates and parsing the reply, the judge
// metrics that reduce to yes/no verdicts (final_response_match_v2, the
// rubric_based_*_v1 pair) ask Jev those verdicts directly as "noul"
// questions; see jevFinalResponseScore and jevRubricScores. The request
// shape matches jev-bench's run_jev.py.

// JevModelPrefix selects JevModel in NewModel: any judge model name
// starting with it (e.g. "typesafe/jev-1.13") is routed to the Decisions
// API instead of google.golang.org/genai.
const JevModelPrefix = "typesafe/jev"

// DefaultJevURL is OpenRouter's Decisions API endpoint.
const DefaultJevURL = "https://openrouter.ai/api/alpha/decisions"

// jevYesThreshold is the noul probability at or above which a yes/no
// question counts as "yes" - the same 0.5 cut-off a binary label uses.
const jevYesThreshold = 0.5

// IsJevModel reports whether a judge model name is served by JevModel.
func IsJevModel(name string) bool {
	return strings.HasPrefix(name, JevModelPrefix)
}

// NewModel builds the judge.Model for a model name: JevModel for
// typesafe/jev-* names, GenAIModel (Gemini) for everything else. apiKey is
// the explicit key for whichever provider the name selects (empty falls
// back to that provider's environment: OPENROUTER_API_KEY for Jev, see
// NewGenAIModel for Gemini).
func NewModel(ctx context.Context, apiKey, model string) (Model, error) {
	if IsJevModel(model) {
		return NewJevModel(apiKey, model)
	}
	return NewGenAIModel(ctx, apiKey, model)
}

// JevQuestion is one typed question in a Decisions API request. Type is
// "noul" (a probability that the answer is true, criteria keyed "true" and
// "false") or "choice" (one of the criteria keys).
type JevQuestion struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria"`
}

// JevAnswer is one typed answer in a Decisions API response. Noul is set
// for "noul" questions; Choice, Probabilities and Confidence for "choice".
type JevAnswer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul,omitempty"`
	Choice        string             `json:"choice,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Confidence    float64            `json:"confidence,omitempty"`
}

// Decider is a judge model that answers typed questions about a state
// object instead of generating text. Judge metrics check for it and, when
// present, ask their verdicts directly rather than prompting and parsing.
type Decider interface {
	Decide(ctx context.Context, state map[string]any, questions map[string]JevQuestion) (map[string]JevAnswer, error)
}

// errNotTextModel is returned by JevModel.Generate: Jev has no free-text
// output, so a metric that needs one (hallucinations_v1's sentence
// segmenter) cannot run on it.
var errNotTextModel = errors.New("judge model answers typed questions only and cannot generate free text")

// JevModel is a Decider backed by OpenRouter's Decisions API. It also
// satisfies Model so it can travel through the same call sites as
// GenAIModel; its Generate always fails with errNotTextModel.
type JevModel struct {
	httpClient *http.Client
	url        string
	apiKey     string
	model      string
	// backoff is the delay before retry n (doubling); a field so tests
	// don't sleep.
	backoff time.Duration
}

// NewJevModel creates a Jev client. apiKey falls back to
// OPENROUTER_API_KEY; the endpoint can be overridden with JEV_API_URL.
func NewJevModel(apiKey, model string) (*JevModel, error) {
	if apiKey == "" {
		apiKey = os.Getenv("OPENROUTER_API_KEY")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("judge model %s needs an OpenRouter API key: set OPENROUTER_API_KEY or pass --judge-api-key", model)
	}
	url := os.Getenv("JEV_API_URL")
	if url == "" {
		url = DefaultJevURL
	}
	return &JevModel{
		httpClient: &http.Client{Timeout: 60 * time.Second},
		url:        url,
		apiKey:     apiKey,
		model:      model,
		backoff:    time.Second,
	}, nil
}

// Generate implements Model; see errNotTextModel.
func (m *JevModel) Generate(ctx context.Context, prompt string) (string, error) {
	return "", fmt.Errorf("%s: %w", m.model, errNotTextModel)
}

// jevMaxAttempts bounds retries on rate limits, server errors and network
// failures, matching run_jev.py's retry set (429/5xx).
const jevMaxAttempts = 5

// Decide sends one Decisions API request and returns its answers keyed by
// question name.
func (m *JevModel) Decide(ctx context.Context, state map[string]any, questions map[string]JevQuestion) (map[string]JevAnswer, error) {
	body, err := json.Marshal(map[string]any{"model": m.model, "state": state, "questions": questions})
	if err != nil {
		return nil, fmt.Errorf("encoding Jev request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt < jevMaxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(m.backoff << (attempt - 1)):
			}
		}
		answers, retry, err := m.decideOnce(ctx, body)
		if err == nil {
			return answers, nil
		}
		lastErr = err
		if !retry {
			break
		}
	}
	return nil, lastErr
}

func (m *JevModel) decideOnce(ctx context.Context, body []byte) (answers map[string]JevAnswer, retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.url, bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Authorization", "Bearer "+m.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Title", "agentevals-go")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, ctx.Err() == nil, fmt.Errorf("calling Jev: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, true, fmt.Errorf("reading Jev response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		retry := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		snippet := raw
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return nil, retry, fmt.Errorf("Jev returned HTTP %d: %s", resp.StatusCode, snippet)
	}

	var decoded struct {
		Answers map[string]JevAnswer `json:"answers"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, false, fmt.Errorf("decoding Jev response: %w", err)
	}
	return decoded.Answers, false, nil
}

// jevYes converts a noul answer to a 1.0/0.0 verdict, or nil when the
// answer is missing or not a noul (NOT_EVALUATED, like an unparseable
// text-judge reply).
func jevYes(a JevAnswer, ok bool) *float64 {
	if !ok || a.Noul == nil {
		return nil
	}
	v := 0.0
	if *a.Noul >= jevYesThreshold {
		v = 1.0
	}
	return &v
}

// finalResponseValidQuestion condenses final_response_match_v2's rating
// constitution into Jev instructions: same task (is the agent response
// valid against the human reference), same leniency rules.
var finalResponseValidQuestion = JevQuestion{
	Type: "noul",
	Instructions: "Is the agent response a valid answer to the user prompt, judged against the reference response written by a human rater? " +
		"The reference is high quality but may contain only the key entities; the agent may include more information or use a different format, " +
		"structure, abbreviation, number formatting or rounding, as long as the key entities and the final answer match. " +
		"Units must match. Trust the reference for maths, dates and table data. " +
		"Claiming it cannot answer when the reference found an answer is invalid.",
	Criteria: map[string]string{
		"true":  "Valid: the agent response contains the correct final answer and key entities from the reference",
		"false": "Invalid: wrong, missing or contradicting the key entities or final answer, wrong units, or an unwarranted refusal",
	},
}

// jevFinalResponseScore asks Jev final_response_match_v2's verdict for one
// invocation (one call, no sampling: Jev returns a probability, not a
// sampled label).
func jevFinalResponseScore(ctx context.Context, d Decider, userPrompt, response, reference string) (*float64, error) {
	state := map[string]any{
		"user_prompt":        userPrompt,
		"agent_response":     response,
		"reference_response": reference,
	}
	answers, err := d.Decide(ctx, state, map[string]JevQuestion{"is_the_agent_response_valid": finalResponseValidQuestion})
	if err != nil {
		return nil, err
	}
	a, ok := answers["is_the_agent_response_valid"]
	return jevYes(a, ok), nil
}

// jevRubricScores asks one noul question per rubric, keyed by rubric ID,
// and returns the verdicts in rubric order. subject names what the rubric
// is judged against ("final response" or "tool usage").
func jevRubricScores(ctx context.Context, d Decider, state map[string]any, rubrics []Rubric, subject string) ([]rubricScore, error) {
	questions := make(map[string]JevQuestion, len(rubrics))
	for _, r := range rubrics {
		questions[r.ID] = JevQuestion{
			Type:         "noul",
			Instructions: fmt.Sprintf("Does the agent's %s satisfy this property? %s", subject, r.Text),
			Criteria: map[string]string{
				"true":  "The property holds",
				"false": "The property does not hold",
			},
		}
	}
	answers, err := d.Decide(ctx, state, questions)
	if err != nil {
		return nil, err
	}
	scores := make([]rubricScore, len(rubrics))
	for i, r := range rubrics {
		a, ok := answers[r.ID]
		scores[i] = rubricScore{RubricID: r.ID, Score: jevYes(a, ok)}
	}
	return scores, nil
}
