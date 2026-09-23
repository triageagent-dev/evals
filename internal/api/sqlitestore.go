package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	tracepkg "github.com/triageagent-dev/agentevals-go/internal/trace"
	_ "modernc.org/sqlite" // pure-Go driver, no CGO - registers "sqlite" with database/sql
)

// SQLiteStore is a durable archive of Sessions, backed by a single SQLite
// file: one row per session, the whole session serialized as a JSON blob,
// so a field added to Session never needs a migration. Ported from
// storage/session_store.py's SqliteSessionStore - deliberately stdlib/
// single-purpose, not the (also not-yet-ported) Postgres-or-memory
// storage/repos/* abstraction behind /api/runs.
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore opens (creating if needed) the archive at path, in WAL
// mode so the periodic snapshot writer and a startup load never block each
// other.
func NewSQLiteStore(path string) (*SQLiteStore, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("creating session archive directory: %w", err)
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening session archive: %w", err)
	}
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA synchronous=NORMAL"} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("setting %q: %w", pragma, err)
		}
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS sessions (
		session_id TEXT PRIMARY KEY,
		data TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("creating sessions table: %w", err)
	}
	if err := ensureRunsSchema(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := ensureEvalSetsSchema(db); err != nil {
		db.Close()
		return nil, err
	}

	log.Printf("session archive: %s", path)
	return &SQLiteStore{db: db}, nil
}

// sessionSnapshot is Session's on-disk shape: every field needed to
// reconstruct a session's content and metadata, but not its live
// incremental.Extractor (dedup state that's cheap to rebuild from spans by
// simply not restoring it - a restored session's extractor starts fresh,
// so a span it already saw pre-restart could in principle re-trigger a
// user_input/agent_response broadcast once more spans arrive for it; this
// matches Python's port, which also excludes the extractor from
// TraceSession's persisted fields).
type sessionSnapshot struct {
	SessionID   string           `json:"session_id"`
	TraceID     string           `json:"trace_id"`
	EvalSetID   string           `json:"eval_set_id,omitempty"`
	Metadata    map[string]any   `json:"metadata,omitempty"`
	TraceIDs    []string         `json:"trace_ids,omitempty"`
	Spans       []*tracepkg.Span `json:"spans,omitempty"`
	StartedAt   time.Time        `json:"started_at"`
	HasRootSpan bool             `json:"has_root_span,omitempty"`
	IsComplete  bool             `json:"is_complete,omitempty"`
	CompletedAt *time.Time       `json:"completed_at,omitempty"`
}

func snapshotOf(s *Session) sessionSnapshot {
	traceIDs := make([]string, 0, len(s.TraceIDs))
	for id := range s.TraceIDs {
		traceIDs = append(traceIDs, id)
	}
	// Children are rebuilt from the flat span list on every otlp.BuildTrace
	// call regardless; stripping them here keeps the persisted blob to one
	// copy of each span instead of two (flat entry + reachable via a
	// sibling's Children).
	spans := make([]*tracepkg.Span, len(s.Spans))
	for i, sp := range s.Spans {
		cp := *sp
		cp.Children = nil
		spans[i] = &cp
	}
	return sessionSnapshot{
		SessionID:   s.ID,
		TraceID:     s.PrimaryTraceID,
		EvalSetID:   s.EvalSetID,
		Metadata:    s.Metadata,
		TraceIDs:    traceIDs,
		Spans:       spans,
		StartedAt:   s.StartedAt,
		HasRootSpan: s.HasRootSpan,
		IsComplete:  s.IsComplete,
		CompletedAt: s.CompletedAt,
	}
}

// SaveAll upserts every session's current snapshot. A session that fails
// to serialize is logged and skipped rather than aborting the whole batch.
func (s *SQLiteStore) SaveAll(sessions map[string]*Session) error {
	rows := make([]struct {
		id, data, updatedAt string
	}, 0, len(sessions))

	now := time.Now().UTC().Format(time.RFC3339)
	for id, session := range sessions {
		data, err := json.Marshal(snapshotOf(session))
		if err != nil {
			log.Printf("session archive: failed to serialize session %s: %v", id, err)
			continue
		}
		rows = append(rows, struct{ id, data, updatedAt string }{id, string(data), now})
	}
	if len(rows) == 0 {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT INTO sessions (session_id, data, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET data = excluded.data, updated_at = excluded.updated_at`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, r := range rows {
		if _, err := stmt.Exec(r.id, r.data, r.updatedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Delete removes one session's archived row.
func (s *SQLiteStore) Delete(sessionID string) error {
	_, err := s.db.Exec("DELETE FROM sessions WHERE session_id = ?", sessionID)
	return err
}

// LoadAll returns every archived session's snapshot. A row that fails to
// deserialize is logged and skipped rather than aborting the whole load.
func (s *SQLiteStore) LoadAll() (map[string]sessionSnapshot, error) {
	rows, err := s.db.Query("SELECT session_id, data FROM sessions")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := map[string]sessionSnapshot{}
	for rows.Next() {
		var id, data string
		if err := rows.Scan(&id, &data); err != nil {
			return nil, err
		}
		var snap sessionSnapshot
		if err := json.Unmarshal([]byte(data), &snap); err != nil {
			log.Printf("session archive: failed to deserialize session %s: %v", id, err)
			continue
		}
		result[id] = snap
	}
	return result, rows.Err()
}

// Close closes the underlying database handle.
func (s *SQLiteStore) Close() error {
	return s.db.Close()
}
