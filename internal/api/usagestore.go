package api

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/triageagent-dev/agentevals-go/internal/adk"
	tracepkg "github.com/triageagent-dev/agentevals-go/internal/trace"
)

// Token usage ledger - additive over Python (agentevals has no usage view).
// One row per LLM span that reported tokens, kept apart from the session
// blobs so GET /api/usage aggregates with one indexed SQL query instead of
// unmarshalling every archived session. Rows are keyed by span ID and
// written with INSERT OR IGNORE, so re-ingesting a span (a reopened
// session, the startup backfill) never counts it twice.

// Span attributes triage-core stamps on every LLM call (core's
// setLLMSpanTenantAttrs); any other producer simply leaves them empty.
const (
	triageTenantAttr   = "triage.tenant"
	triageCallKindAttr = "triage.llm.call_kind"
)

// DefaultUsageRetention is how long usage rows are kept. Rows are a few
// hundred bytes, so this is far longer than a session is worth keeping.
const DefaultUsageRetention = 90 * 24 * time.Hour

type usageRow struct {
	SpanID       string
	TraceID      string
	SessionID    string
	StartMs      int64
	DurationMs   int64
	Service      string
	Tenant       string
	Kind         string
	Model        string
	Name         string
	InputTokens  int64
	OutputTokens int64
	IsError      bool
}

func ensureUsageSchema(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS llm_calls (
		span_id TEXT PRIMARY KEY,
		trace_id TEXT NOT NULL,
		session_id TEXT NOT NULL,
		start_ms INTEGER NOT NULL,
		duration_ms INTEGER NOT NULL,
		service TEXT NOT NULL,
		tenant TEXT NOT NULL,
		kind TEXT NOT NULL,
		model TEXT NOT NULL,
		name TEXT NOT NULL,
		input_tokens INTEGER NOT NULL,
		output_tokens INTEGER NOT NULL,
		is_error INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("creating llm_calls table: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_llm_calls_start ON llm_calls(start_ms)`); err != nil {
		return fmt.Errorf("creating llm_calls.start_ms index: %w", err)
	}
	return nil
}

// usageRowFromSpan returns the ledger row for an LLM span that reported
// tokens. A span that is not an LLM call, or reported no tokens, has no row:
// an AGENT/CHAIN parent repeating its child's counts would double them.
func usageRowFromSpan(span *tracepkg.Span, sessionID string) (usageRow, bool) {
	if !adk.IsLLMSpan(span) {
		return usageRow{}, false
	}
	in, out, model := adk.ExtractTokenUsageFromAttrs(span.Tags)
	if in == 0 && out == 0 {
		return usageRow{}, false
	}
	return usageRow{
		SpanID:       span.SpanID,
		TraceID:      span.TraceID,
		SessionID:    sessionID,
		StartMs:      span.StartTime / 1000,
		DurationMs:   span.Duration / 1000,
		Service:      span.TagString(adk.OtelServiceName),
		Tenant:       span.TagString(triageTenantAttr),
		Kind:         span.TagString(triageCallKindAttr),
		Model:        model,
		Name:         span.OperationName,
		InputTokens:  in,
		OutputTokens: out,
		IsError:      span.TagString("otel.status_code") == "ERROR",
	}, true
}

// InsertUsage writes rows in one transaction, ignoring spans already
// recorded.
func (s *SQLiteStore) InsertUsage(rows []usageRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO llm_calls
		(span_id, trace_id, session_id, start_ms, duration_ms, service, tenant, kind, model, name, input_tokens, output_tokens, is_error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()
	for _, r := range rows {
		if _, err := stmt.Exec(r.SpanID, r.TraceID, r.SessionID, r.StartMs, r.DurationMs, r.Service, r.Tenant,
			r.Kind, r.Model, r.Name, r.InputTokens, r.OutputTokens, r.IsError); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// PruneUsage deletes rows that started before cutoff.
func (s *SQLiteStore) PruneUsage(cutoff time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM llm_calls WHERE start_ms < ?`, cutoff.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// usageBucketDTO is one (time bucket, service, tenant, kind, model) cell of
// GET /api/usage. The UI regroups cells by whichever dimension is selected,
// so the server never needs one query per grouping.
type usageBucketDTO struct {
	Start        int64  `json:"start"`
	Service      string `json:"service"`
	Tenant       string `json:"tenant"`
	Kind         string `json:"kind"`
	Model        string `json:"model"`
	Calls        int64  `json:"calls"`
	Errors       int64  `json:"errors"`
	InputTokens  int64  `json:"inputTokens"`
	OutputTokens int64  `json:"outputTokens"`
	DurationMs   int64  `json:"durationMs"`
}

// UsageSeries aggregates rows in [from, to) into bucketMs-wide cells.
func (s *SQLiteStore) UsageSeries(from, to time.Time, bucketMs int64) ([]usageBucketDTO, error) {
	rows, err := s.db.Query(`SELECT (start_ms / ?) * ? AS bucket, service, tenant, kind, model,
			COUNT(*), SUM(is_error), SUM(input_tokens), SUM(output_tokens), SUM(duration_ms)
		FROM llm_calls WHERE start_ms >= ? AND start_ms < ?
		GROUP BY bucket, service, tenant, kind, model ORDER BY bucket`,
		bucketMs, bucketMs, from.UnixMilli(), to.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []usageBucketDTO{}
	for rows.Next() {
		var b usageBucketDTO
		if err := rows.Scan(&b.Start, &b.Service, &b.Tenant, &b.Kind, &b.Model,
			&b.Calls, &b.Errors, &b.InputTokens, &b.OutputTokens, &b.DurationMs); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// usageCallDTO is one LLM call in GET /api/usage/calls.
type usageCallDTO struct {
	SpanID       string `json:"spanId"`
	TraceID      string `json:"traceId"`
	SessionID    string `json:"sessionId"`
	Start        int64  `json:"start"`
	DurationMs   int64  `json:"durationMs"`
	Service      string `json:"service"`
	Tenant       string `json:"tenant"`
	Kind         string `json:"kind"`
	Model        string `json:"model"`
	Name         string `json:"name"`
	InputTokens  int64  `json:"inputTokens"`
	OutputTokens int64  `json:"outputTokens"`
	IsError      bool   `json:"isError"`
}

// TopUsageCalls returns the calls in [from, to) with the most total tokens.
func (s *SQLiteStore) TopUsageCalls(from, to time.Time, limit int) ([]usageCallDTO, error) {
	rows, err := s.db.Query(`SELECT span_id, trace_id, session_id, start_ms, duration_ms, service, tenant, kind, model,
			name, input_tokens, output_tokens, is_error
		FROM llm_calls WHERE start_ms >= ? AND start_ms < ?
		ORDER BY input_tokens + output_tokens DESC LIMIT ?`, from.UnixMilli(), to.UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []usageCallDTO{}
	for rows.Next() {
		var c usageCallDTO
		if err := rows.Scan(&c.SpanID, &c.TraceID, &c.SessionID, &c.Start, &c.DurationMs, &c.Service, &c.Tenant,
			&c.Kind, &c.Model, &c.Name, &c.InputTokens, &c.OutputTokens, &c.IsError); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
