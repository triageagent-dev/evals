package judge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/triageagent-dev/agentevals-go/internal/adk"
	"github.com/triageagent-dev/agentevals-go/internal/eval"
)

// scriptedDecider answers every noul question with the next probability
// from nouls (cycling), and records each request.
type scriptedDecider struct {
	nouls  []float64
	calls  int
	states []map[string]any
	qs     []map[string]JevQuestion
}

func (d *scriptedDecider) Generate(ctx context.Context, prompt string) (string, error) {
	return "", errNotTextModel
}

func (d *scriptedDecider) Decide(ctx context.Context, state map[string]any, questions map[string]JevQuestion) (map[string]JevAnswer, error) {
	d.states = append(d.states, state)
	d.qs = append(d.qs, questions)
	answers := map[string]JevAnswer{}
	for name := range questions {
		p := d.nouls[d.calls%len(d.nouls)]
		d.calls++
		answers[name] = JevAnswer{Type: "noul", Noul: &p}
	}
	return answers, nil
}

func TestIsJevModel(t *testing.T) {
	for name, want := range map[string]bool{
		"typesafe/jev-1.13": true,
		"typesafe/jev":      true,
		"gemini-2.5-flash":  false,
		"openai/gpt-4o":     false,
	} {
		if got := IsJevModel(name); got != want {
			t.Errorf("IsJevModel(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestNewModelJevNeedsKey(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "")
	if _, err := NewModel(context.Background(), "", "typesafe/jev-1.13"); err == nil || !strings.Contains(err.Error(), "OPENROUTER_API_KEY") {
		t.Fatalf("NewModel without key: err = %v, want missing-key error", err)
	}
	t.Setenv("OPENROUTER_API_KEY", "k")
	m, err := NewModel(context.Background(), "", "typesafe/jev-1.13")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.(Decider); !ok {
		t.Fatalf("NewModel(typesafe/jev-1.13) = %T, want a Decider", m)
	}
	if _, err := m.Generate(context.Background(), "x"); !errors.Is(err, errNotTextModel) {
		t.Errorf("Generate err = %v, want errNotTextModel", err)
	}
}

func TestJevModelDecideRequestAndRetry(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("Authorization = %q", got)
		}
		var body struct {
			Model     string                 `json:"model"`
			State     map[string]any         `json:"state"`
			Questions map[string]JevQuestion `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Model != "typesafe/jev-1.13" || body.State["user_prompt"] != "hi" || body.Questions["q"].Type != "noul" {
			t.Errorf("unexpected request body: %+v", body)
		}
		w.Write([]byte(`{"answers":{"q":{"type":"noul","noul":0.89}},"model":"typesafe/jev-1.13-20260917"}`))
	}))
	defer srv.Close()

	t.Setenv("JEV_API_URL", srv.URL)
	m, err := NewJevModel("secret", "typesafe/jev-1.13")
	if err != nil {
		t.Fatal(err)
	}
	m.backoff = 0
	answers, err := m.Decide(context.Background(), map[string]any{"user_prompt": "hi"},
		map[string]JevQuestion{"q": {Type: "noul", Instructions: "?", Criteria: map[string]string{"true": "y", "false": "n"}}})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2 (one 429 retry)", attempts)
	}
	if a := answers["q"]; a.Noul == nil || *a.Noul != 0.89 {
		t.Errorf("answer = %+v, want noul 0.89", a)
	}
}

func TestJevModelDecideNoRetryOnClientError(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`bad key`))
	}))
	defer srv.Close()

	t.Setenv("JEV_API_URL", srv.URL)
	m, _ := NewJevModel("secret", "typesafe/jev-1.13")
	m.backoff = 0
	_, err := m.Decide(context.Background(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("err = %v, want HTTP 401", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
}

func TestFinalResponseMatchV2Jev(t *testing.T) {
	d := &scriptedDecider{nouls: []float64{0.9, 0.2}}
	actual := []adk.Invocation{invocation("q1", "a1"), invocation("q2", "a2")}
	expected := []adk.Invocation{invocation("q1", "r1"), invocation("q2", "r2")}

	result, err := FinalResponseMatchV2(context.Background(), d, actual, expected, 5, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if d.calls != 2 {
		t.Errorf("Decide answered %d questions, want 2 (one per invocation, no sampling)", d.calls)
	}
	if result.Score != 0.5 || result.Status != eval.StatusPassed {
		t.Errorf("result = %+v, want score 0.5 PASSED", result)
	}
	if got := result.PerInvocationScores; len(got) != 2 || got[0] != 1 || got[1] != 0 {
		t.Errorf("PerInvocationScores = %v, want [1 0]", got)
	}
	if s := d.states[0]; s["user_prompt"] != "q1" || s["agent_response"] != "a1" || s["reference_response"] != "r1" {
		t.Errorf("state = %v", s)
	}
}

func TestRubricBasedJev(t *testing.T) {
	d := &scriptedDecider{nouls: []float64{0.95}}
	rubrics := RubricsFromStrings([]string{"Answers the question", "Is concise"})
	actual := []adk.Invocation{invocation("q1", "a1")}

	result, err := RubricBasedFinalResponseQualityV1(context.Background(), d, actual, rubrics, 5, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if result.Score != 1 || result.Status != eval.StatusPassed {
		t.Errorf("result = %+v, want score 1 PASSED", result)
	}
	if len(d.qs) != 1 || len(d.qs[0]) != 2 {
		t.Fatalf("questions = %v, want one call with 2 rubric questions", d.qs)
	}
	if q := d.qs[0]["rubric_1"]; !strings.Contains(q.Instructions, "final response") || !strings.Contains(q.Instructions, "Is concise") {
		t.Errorf("rubric_1 instructions = %q", q.Instructions)
	}
	if d.states[0]["final_response"] != "a1" {
		t.Errorf("state = %v", d.states[0])
	}
}

func TestHallucinationsV1RejectsJev(t *testing.T) {
	result, err := HallucinationsV1(context.Background(), &scriptedDecider{nouls: []float64{1}}, []adk.Invocation{invocation("q", "a")}, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Error, "text-generating") {
		t.Errorf("Error = %q, want a clear unsupported-model error", result.Error)
	}
}
