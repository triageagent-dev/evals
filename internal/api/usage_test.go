package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// llmSpanBody is one OTLP/HTTP JSON export with a triage-core style LLM span
// (OpenInference token counts, tenant and call kind) under an AGENT parent
// that repeats no counts.
func llmSpanBody(traceID, spanID string, startMs int64, prompt, completion int, statusCode int) map[string]any {
	ns := func(ms int64) string { return strconvI(ms * 1_000_000) }
	attr := func(k string, v map[string]any) map[string]any { return map[string]any{"key": k, "value": v} }
	str := func(s string) map[string]any { return map[string]any{"stringValue": s} }
	num := func(n int) map[string]any { return map[string]any{"intValue": strconvI(int64(n))} }
	return map[string]any{"resourceSpans": []any{map[string]any{
		"resource": map[string]any{"attributes": []any{attr("service.name", str("triage-core"))}},
		"scopeSpans": []any{map[string]any{"scope": map[string]any{}, "spans": []any{
			map[string]any{
				"traceId": traceID, "spanId": "agent-" + spanID, "name": "agent.reflection",
				"startTimeUnixNano": ns(startMs), "endTimeUnixNano": ns(startMs + 3000),
				"attributes": []any{attr("openinference.span.kind", str("AGENT"))},
			},
			map[string]any{
				"traceId": traceID, "spanId": spanID, "parentSpanId": "agent-" + spanID, "name": "llm.chat.completions",
				"startTimeUnixNano": ns(startMs + 10), "endTimeUnixNano": ns(startMs + 2010),
				"status": map[string]any{"code": float64(statusCode)},
				"attributes": []any{
					attr("openinference.span.kind", str("LLM")),
					attr("llm.model_name", str("gemini-2.5-flash")),
					attr("llm.token_count.prompt", num(prompt)),
					attr("llm.token_count.completion", num(completion)),
					attr("triage.tenant", str("otel-demo")),
					attr("triage.llm.call_kind", str("reflection")),
				},
			},
		}}},
	}}}
}

func strconvI(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestUsageLedger_IngestFlushAndQuery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	archive, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	store := NewSessionStoreWithArchive(nil, archive)
	now := time.Now().Add(-time.Hour).UnixMilli()

	store.Ingest(llmSpanBody("aaaaaaaaaaaa0001", "llm1", now, 1000, 100, 1))
	store.Ingest(llmSpanBody("aaaaaaaaaaaa0002", "llm2", now+60_000, 500, 50, 2))
	// The same span again (a retried export) must not count twice.
	store.Ingest(llmSpanBody("aaaaaaaaaaaa0001", "llm1", now, 1000, 100, 1))
	if err := store.flushPersist(); err != nil {
		t.Fatal(err)
	}

	cells, err := archive.UsageSeries(time.Now().Add(-24*time.Hour), time.Now(), int64(24*time.Hour/time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	var calls, errs, in, out int64
	for _, c := range cells {
		if c.Kind != "reflection" || c.Tenant != "otel-demo" || c.Model != "gemini-2.5-flash" || c.Service != "triage-core" {
			t.Errorf("unexpected cell dimensions: %+v", c)
		}
		calls += c.Calls
		errs += c.Errors
		in += c.InputTokens
		out += c.OutputTokens
	}
	if calls != 2 || in != 1500 || out != 150 || errs != 1 {
		t.Errorf("got calls=%d in=%d out=%d errors=%d, want 2/1500/150/1 (AGENT parent not counted, duplicate ignored)", calls, in, out, errs)
	}

	top, err := archive.TopUsageCalls(time.Now().Add(-24*time.Hour), time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 2 || top[0].SpanID != "llm1" || top[0].SessionID != "otlp-aaaaaaaaaaaa" {
		t.Errorf("top calls = %+v, want llm1 first in session otlp-aaaaaaaaaaaa", top)
	}
	store.Close()

	// A restart backfills from the archive without duplicating rows.
	archive2, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	store2 := NewSessionStoreWithArchive(nil, archive2)
	if err := store2.LoadPersisted(); err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	top, _ = archive2.TopUsageCalls(time.Now().Add(-24*time.Hour), time.Now(), 10)
	if len(top) != 2 {
		t.Errorf("after restart got %d rows, want 2", len(top))
	}
}

func TestUsageLedger_BackfillsArchiveWithoutLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	archive, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	store := NewSessionStore(nil) // no archive: spans are not ledgered on ingest
	store.Ingest(llmSpanBody("bbbbbbbbbbbb0001", "llm1", time.Now().Add(-time.Hour).UnixMilli(), 10, 5, 1))
	if err := archive.SaveAll(store.sessions); err != nil {
		t.Fatal(err)
	}

	restored := NewSessionStoreWithArchive(nil, archive)
	if err := restored.LoadPersisted(); err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	top, _ := archive.TopUsageCalls(time.Now().Add(-24*time.Hour), time.Now(), 10)
	if len(top) != 1 || top[0].InputTokens != 10 {
		t.Errorf("backfill rows = %+v, want one row with 10 input tokens", top)
	}
}

func TestUsageHandler_WindowAndStorage(t *testing.T) {
	rec := httptest.NewRecorder()
	usageHandler(nil)(rec, httptest.NewRequest(http.MethodGet, "/api/usage", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no archive: status %d, want 503", rec.Code)
	}

	archive, err := NewSQLiteStore(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	usageHandler(archive)(rec, httptest.NewRequest(http.MethodGet, "/api/usage?hours=168", nil))
	var body struct {
		Data struct {
			From, To, BucketMs int64
			Buckets            []usageBucketDTO
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Data.BucketMs != int64(24*time.Hour/time.Millisecond) {
		t.Errorf("7-day window bucket = %d ms, want one day", body.Data.BucketMs)
	}
	if body.Data.To-body.Data.From != int64(168*time.Hour/time.Millisecond) {
		t.Errorf("window = %d ms, want 168h", body.Data.To-body.Data.From)
	}
	if body.Data.Buckets == nil {
		t.Error("buckets must be [] not null when empty")
	}
}
