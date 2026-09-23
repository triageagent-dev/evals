package api

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/triageagent-dev/agentevals-go/internal/incremental"
	"github.com/triageagent-dev/agentevals-go/internal/otlp"
	tracepkg "github.com/triageagent-dev/agentevals-go/internal/trace"
)

const (
	agentevalsSessionName = "agentevals.session_name"
	agentevalsEvalSetID   = "agentevals.eval_set_id"
	genAIConversationID   = "gen_ai.conversation.id"

	maxSpansPerSession = 10000

	// defaultIdleTimeout / defaultCompletionGrace match
	// StreamingTraceManager's idle_timeout_seconds / completion_grace_seconds
	// defaults (ws_server.py). idleTimeout completes a session after this
	// long with no new span/log (agent crash, no root span ever emitted);
	// completionGrace completes a session this long after a root span
	// arrives, giving late child spans from the same OTLP batch time to
	// land first.
	defaultIdleTimeout     = 30 * time.Second
	defaultCompletionGrace = 3 * time.Second
)

// Session is one live OTLP-ingested session: the aggregated spans of every
// trace grouped under one session identity. Ported from
// streaming/session.py's TraceSession, including the completion state
// (IsComplete/CompletedAt), driven by SessionStore's idle/completion timers
// (see ws_server.py's reset_idle_timer/schedule_session_completion).
type Session struct {
	ID             string
	PrimaryTraceID string
	EvalSetID      string
	Metadata       map[string]any
	TraceIDs       map[string]struct{}
	Spans          []*tracepkg.Span
	StartedAt      time.Time
	HasRootSpan    bool
	IsComplete     bool
	CompletedAt    *time.Time
	Invocations    []InvocationDTO
	extractor      *incremental.Extractor

	// changeSeq is this session's own stamp from SessionStore.changeSeq as
	// of its last mutation - only ever set with SessionStore.mu held. A
	// zero-value session (freshly restored from the archive, not yet
	// mutated again) is never dirty until something changes it again;
	// compared against SessionStore.persistedSeq[id] to let flushPersist
	// skip re-marshaling/re-saving a session that hasn't changed since it
	// was last durably written, even when other sessions in the same
	// SessionStore have.
	changeSeq uint64
}

// SessionSummary is the JSON shape returned by GET /api/streaming/sessions
// and the session_started SSE event's "session" field. Field names/casing
// match agentevals' SessionInfo (CamelModel) so the existing React UI reads
// it unmodified. Invocations mirrors SessionInfo.invocations: nil unless
// the session is complete and has at least one real invocation (populated
// by completeSession's batch GenAI-semconv conversion).
type SessionSummary struct {
	SessionID   string          `json:"sessionId"`
	TraceID     string          `json:"traceId"`
	EvalSetID   *string         `json:"evalSetId"`
	SpanCount   int             `json:"spanCount"`
	IsComplete  bool            `json:"isComplete"`
	StartedAt   string          `json:"startedAt"`
	CompletedAt *string         `json:"completedAt"`
	Metadata    map[string]any  `json:"metadata"`
	Invocations []InvocationDTO `json:"invocations,omitempty"`
}

func summarize(s *Session) SessionSummary {
	var evalSetID *string
	if s.EvalSetID != "" {
		evalSetID = &s.EvalSetID
	}
	metadata := s.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	var completedAt *string
	if s.CompletedAt != nil {
		formatted := s.CompletedAt.Format(time.RFC3339)
		completedAt = &formatted
	}
	var invocations []InvocationDTO
	if s.IsComplete && len(s.Invocations) > 0 {
		invocations = s.Invocations
	}
	return SessionSummary{
		SessionID:   s.ID,
		TraceID:     s.PrimaryTraceID,
		EvalSetID:   evalSetID,
		SpanCount:   len(s.Spans),
		IsComplete:  s.IsComplete,
		StartedAt:   s.StartedAt.Format(time.RFC3339),
		CompletedAt: completedAt,
		Metadata:    metadata,
		Invocations: invocations,
	}
}

// sessionStartedEvent / spanReceivedEvent mirror WSSessionStartedEvent /
// WSSpanReceivedEvent (api/models.py).
type sessionStartedEvent struct {
	Type    string         `json:"type"`
	Session SessionSummary `json:"session"`
}

type spanReceivedEvent struct {
	Type      string  `json:"type"`
	SessionID string  `json:"sessionId"`
	Span      spanDTO `json:"span"`
}

// sessionCompleteEvent mirrors WSSessionCompleteEvent (api/models.py),
// broadcast once a session's completion timer fires. Invocations is the
// batch-converted GenAI-semconv/ADK invocation list (see
// extractInvocationsForUI): non-nil only when the conversion produced at
// least one invocation, matching LiveStreamingView.tsx's
// `data.invocations?.length` check (an empty/absent list leaves whatever
// internal/incremental already broadcast live untouched).
type sessionCompleteEvent struct {
	Type        string          `json:"type"`
	SessionID   string          `json:"sessionId"`
	Invocations []InvocationDTO `json:"invocations"`
	CompletedAt *string         `json:"completedAt"`
}

// SessionStore holds every session ingested since process start, in memory,
// optionally archived to a SQLiteStore so sessions survive a restart.
type SessionStore struct {
	mu               sync.Mutex
	sessions         map[string]*Session
	order            []string
	hub              *sseHub
	store            *SQLiteStore
	idleTimeout      time.Duration
	completionGrace  time.Duration
	idleTimers       map[string]*time.Timer
	completionTimers map[string]*time.Timer

	// changeSeq/lastPersistedSeq let flushPersist skip its snapshot+DB
	// round trip entirely when nothing has changed since the last
	// successful persist (e.g. an idle deployment with no active OTLP
	// traffic, ticking every persistInterval for no reason otherwise).
	// persistedSeq additionally lets it skip re-marshaling/re-saving
	// individual sessions that haven't changed even when *some* other
	// session has (e.g. one active session amid many long-completed
	// ones that will never mutate again). All three are only ever
	// touched with mu held. changeSeq is bumped by every mutation to a
	// Session's persisted fields (span ingestion, completion, reopening),
	// and that new value is stamped onto the mutated Session's own
	// changeSeq; lastPersistedSeq/persistedSeq only ever advance to a
	// changeSeq value flushPersist has confirmed was durably written -
	// never on a failed write, so a failed persist is retried next tick
	// exactly as before this optimization, not silently dropped.
	changeSeq        uint64
	lastPersistedSeq uint64
	persistedSeq     map[string]uint64
}

func NewSessionStore(hub *sseHub) *SessionStore {
	return &SessionStore{
		sessions:         map[string]*Session{},
		hub:              hub,
		idleTimeout:      defaultIdleTimeout,
		completionGrace:  defaultCompletionGrace,
		idleTimers:       map[string]*time.Timer{},
		persistedSeq:     map[string]uint64{},
		completionTimers: map[string]*time.Timer{},
	}
}

// markChangedLocked bumps the store-wide change counter and stamps it onto
// session, marking it (and only it) dirty for the next flushPersist. Must
// be called with s.mu held, at every point a Session's persisted fields
// (Spans/HasRootSpan/IsComplete/CompletedAt/EvalSetID/Invocations) change.
func (s *SessionStore) markChangedLocked(session *Session) {
	s.changeSeq++
	session.changeSeq = s.changeSeq
}

// NewSessionStoreWithArchive is NewSessionStore plus a SQLiteStore to
// persist to. Call LoadPersisted once at startup and StartPersistence to
// begin periodic snapshots; ported from ws_server.py's
// StreamingTraceManager(sqlite_path=...) + load_persisted_sessions +
// start_persistence_task.
func NewSessionStoreWithArchive(hub *sseHub, store *SQLiteStore) *SessionStore {
	s := NewSessionStore(hub)
	s.store = store
	return s
}

// LoadPersisted restores every archived session, if a store is configured.
// Each restored session gets a fresh incremental.Extractor - see
// sessionSnapshot's doc comment for why that's fine. Call once at startup,
// before serving traffic.
//
// Deliberately diverges from ws_server.py's load_persisted_sessions here:
// Python restores is_complete/completed_at as-is and starts no timer,
// meaning a session whose producer already stopped sending before restart
// (idle timer never got to fire) stays "active" forever after a restart -
// reproduced live: a session's duration kept ticking in the UI for 2+
// hours across a redeploy. A restored, not-yet-complete session gets a
// fresh idle timer here so it still completes once idleTimeout passes
// with no new span, matching what would have happened had the process
// never restarted.
func (s *SessionStore) LoadPersisted() error {
	if s.store == nil {
		return nil
	}
	snapshots, err := s.store.LoadAll()
	if err != nil {
		return fmt.Errorf("loading session archive: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for id, snap := range snapshots {
		traceIDs := make(map[string]struct{}, len(snap.TraceIDs))
		for _, tid := range snap.TraceIDs {
			traceIDs[tid] = struct{}{}
		}
		s.sessions[id] = &Session{
			ID:             snap.SessionID,
			PrimaryTraceID: snap.TraceID,
			EvalSetID:      snap.EvalSetID,
			Metadata:       snap.Metadata,
			TraceIDs:       traceIDs,
			Spans:          snap.Spans,
			StartedAt:      snap.StartedAt,
			HasRootSpan:    snap.HasRootSpan,
			IsComplete:     snap.IsComplete,
			CompletedAt:    snap.CompletedAt,
			extractor:      incremental.New(),
		}
		if snap.IsComplete {
			// Persisted snapshots predate this converter (or were archived
			// before invocations existed) - recompute rather than leaving
			// completed sessions permanently without detail after restart.
			trace := otlp.BuildTrace(id, snap.Spans)
			s.sessions[id].Invocations = extractInvocationsForUI(trace)
		}
		s.order = append(s.order, id)
		if !snap.IsComplete {
			s.resetIdleTimerLocked(id)
		}
	}
	if len(snapshots) > 0 {
		log.Printf("restored %d session(s) from archive", len(snapshots))
	}
	return nil
}

// StartPersistence begins a background loop snapshotting every session to
// the archive every interval, until stop is closed. A no-op if no store is
// configured.
func (s *SessionStore) StartPersistence(interval time.Duration, stop <-chan struct{}) {
	if s.store == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := s.flushPersist(); err != nil {
					log.Printf("session archive: periodic snapshot failed: %v", err)
				}
			case <-stop:
				return
			}
		}
	}()
}

// flushPersist snapshots and saves only the sessions that changed since
// the last snapshot this function itself confirmed was durably saved -
// both as a whole (the changeSeq/lastPersistedSeq fast path skips the
// entire cycle when nothing anywhere changed, e.g. an idle deployment
// ticking every persistInterval for no reason) and individually (a
// session whose own changeSeq stamp is no newer than what persistedSeq[id]
// already reflects is left out of the batch, so one active session amid
// many long-completed ones that will never mutate again doesn't cause
// every one of them to be re-marshaled and rewritten every tick). See
// changeSeq's doc comment on SessionStore. A failed SaveAll leaves
// lastPersistedSeq/persistedSeq untouched, so the same sessions are
// retried next tick, exactly as before this optimization existed (when
// every tick unconditionally retried everything).
func (s *SessionStore) flushPersist() error {
	s.mu.Lock()
	seq := s.changeSeq
	if seq == s.lastPersistedSeq {
		s.mu.Unlock()
		return nil
	}
	snapshot := make(map[string]*Session)
	captured := make(map[string]uint64, len(s.sessions))
	for id, sess := range s.sessions {
		if sess.changeSeq > s.persistedSeq[id] {
			snapshot[id] = sess
			captured[id] = sess.changeSeq
		}
	}
	s.mu.Unlock()

	if len(snapshot) > 0 {
		if err := s.store.SaveAll(snapshot); err != nil {
			return err
		}
	}

	s.mu.Lock()
	for id, v := range captured {
		if v > s.persistedSeq[id] {
			s.persistedSeq[id] = v
		}
	}
	if seq > s.lastPersistedSeq {
		s.lastPersistedSeq = seq
	}
	s.mu.Unlock()
	return nil
}

// Close flushes a final snapshot and closes the archive, if configured.
// Also stops every pending idle/completion timer so no goroutine fires
// after the store is done.
func (s *SessionStore) Close() error {
	s.mu.Lock()
	for _, t := range s.idleTimers {
		t.Stop()
	}
	for _, t := range s.completionTimers {
		t.Stop()
	}
	s.mu.Unlock()

	if s.store == nil {
		return nil
	}
	if err := s.flushPersist(); err != nil {
		log.Printf("session archive: final snapshot failed: %v", err)
	}
	return s.store.Close()
}

// Ingest decodes one OTLP export body (resourceSpans) and merges its spans
// into the matching session(s), resolving session identity the same way as
// ws_server.py's get_or_create_otlp_session: agentevals.session_name
// resource attribute, falling back to gen_ai.conversation.id, falling back
// to a per-trace-ID synthetic name. Each span is also run through that
// session's incremental extractor, and every resulting update - plus the
// span itself, plus a session_started event on first sight of a session -
// is broadcast to connected /stream/ui-updates clients.
func (s *SessionStore) Ingest(body map[string]any) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	ingested := 0
	for _, rs := range asSlice(body["resourceSpans"]) {
		resourceSpan := asMap(rs)
		resource := asMap(resourceSpan["resource"])
		resourceAttrs := otlp.DecodeAttributes(asSlice(resource["attributes"]))

		sessionName, _ := resourceAttrs[agentevalsSessionName].(string)
		conversationID, _ := resourceAttrs[genAIConversationID].(string)
		evalSetID, _ := resourceAttrs[agentevalsEvalSetID].(string)

		// A resourceSpans batch can contain multiple scopes; if none carry a
		// session_name/conversation_id directly, pre-scan every span's own
		// attributes for gen_ai.conversation.id (ported from
		// otlp_processing.py's _prescan_conversation_id) so the whole batch
		// still routes to one session.
		if sessionName == "" && conversationID == "" {
			conversationID = prescanConversationID(resourceSpan)
		}

		singleBody := map[string]any{"resourceSpans": []any{rs}}
		traces := otlp.ParseExportRequest(singleBody)

		for _, tr := range traces {
			name := sessionName
			if name == "" {
				name = conversationID
			}
			if name == "" {
				name = fmt.Sprintf("otlp-%s", shortID(tr.TraceID))
			}

			session, isNew := s.getOrCreateSession(name, tr.TraceID, evalSetID, resourceAttrs)

			// session_started must reach clients before any span/update
			// event for this session, so the UI has a session to attach
			// those events to (LiveStreamingView.tsx's span_received/
			// user_input/etc. handlers all no-op on an unknown sessionId).
			if isNew && s.hub != nil {
				s.hub.broadcast(sessionStartedEvent{Type: "session_started", Session: summarize(session)})
			}

			for _, span := range tr.AllSpans {
				if len(session.Spans) >= maxSpansPerSession {
					break
				}
				session.Spans = append(session.Spans, span)
				s.markChangedLocked(session)
				ingested++

				if s.hub != nil {
					s.hub.broadcast(spanReceivedEvent{Type: "span_received", SessionID: session.ID, Span: spanToDTO(span)})
					for _, u := range session.extractor.ProcessSpan(span) {
						s.hub.broadcast(sessionScopedUpdate{SessionID: session.ID, Update: u})
					}
				}

				// Ported from otlp_processing.py's process_traces: every
				// span pushes back the idle deadline, and a span with no
				// parent (a root span) (re)starts the completion grace
				// timer. Both timers ultimately call completeSession.
				s.resetIdleTimerLocked(session.ID)
				if span.ParentSpanID == "" {
					session.HasRootSpan = true
					s.scheduleCompletionLocked(session.ID)
				}
			}
		}
	}
	return ingested
}

func (s *SessionStore) getOrCreateSession(name, traceID, evalSetID string, resourceAttrs map[string]any) (*Session, bool) {
	session := s.sessions[name]
	isNew := false
	if session == nil {
		session = &Session{
			ID:             name,
			PrimaryTraceID: traceID,
			EvalSetID:      evalSetID,
			Metadata:       nonAgentevalsAttrs(resourceAttrs),
			TraceIDs:       map[string]struct{}{},
			StartedAt:      time.Now().UTC(),
			extractor:      incremental.New(),
		}
		s.sessions[name] = session
		s.order = append(s.order, name)
		s.markChangedLocked(session)
		isNew = true
	} else {
		if evalSetID != "" {
			session.EvalSetID = evalSetID
			s.markChangedLocked(session)
		}
		// Ported from ws_server.py's _reopen_session: a trace already
		// bound to this session can still emit more spans after it was
		// marked complete (an OTLP BatchSpanProcessor flushed the root
		// span and some children before the grace period fired, the rest
		// after). Reopen rather than leaving the session stuck complete.
		if session.IsComplete {
			session.IsComplete = false
			session.CompletedAt = nil
			s.markChangedLocked(session)
		}
	}
	session.TraceIDs[traceID] = struct{}{}
	return session, isNew
}

// resetIdleTimerLocked (re)starts the idle-completion timer for a session.
// Must be called with s.mu held; the timer's own callback (completeSession)
// acquires s.mu itself when it eventually fires. Ported from
// ws_server.py's reset_idle_timer.
func (s *SessionStore) resetIdleTimerLocked(sessionID string) {
	if t, ok := s.idleTimers[sessionID]; ok {
		t.Stop()
	}
	s.idleTimers[sessionID] = time.AfterFunc(s.idleTimeout, func() {
		s.completeSession(sessionID)
	})
}

// scheduleCompletionLocked (re)starts the post-root-span grace timer for a
// session. Must be called with s.mu held. Ported from ws_server.py's
// schedule_session_completion.
func (s *SessionStore) scheduleCompletionLocked(sessionID string) {
	if t, ok := s.completionTimers[sessionID]; ok {
		t.Stop()
	}
	s.completionTimers[sessionID] = time.AfterFunc(s.completionGrace, func() {
		s.completeSession(sessionID)
	})
}

// completeSession marks a session complete and broadcasts session_complete,
// idempotently. Runs as an idle/completion timer callback, so it acquires
// s.mu itself rather than assuming it's held. Ported from ws_server.py's
// _complete_otlp_session.
func (s *SessionStore) completeSession(sessionID string) {
	s.mu.Lock()
	session, ok := s.sessions[sessionID]
	if !ok || session.IsComplete {
		s.mu.Unlock()
		return
	}

	now := time.Now().UTC()
	session.IsComplete = true
	session.CompletedAt = &now
	s.markChangedLocked(session)

	// Batch-convert the session's full span set into real invocations
	// (tool calls, user/agent text, model info) now that it's done -
	// mirrors ws_server.py's _complete_otlp_session calling
	// _extract_invocations. Built from otlp.BuildTrace's merged
	// parent/child tree, same as the Trace() method.
	trace := otlp.BuildTrace(sessionID, session.Spans)
	session.Invocations = extractInvocationsForUI(trace)

	if t, ok := s.idleTimers[sessionID]; ok {
		t.Stop()
		delete(s.idleTimers, sessionID)
	}
	if t, ok := s.completionTimers[sessionID]; ok {
		t.Stop()
		delete(s.completionTimers, sessionID)
	}

	completedAt := now.Format(time.RFC3339)
	invocations := session.Invocations
	hub := s.hub
	s.mu.Unlock()

	if hub != nil {
		hub.broadcast(sessionCompleteEvent{
			Type:        "session_complete",
			SessionID:   sessionID,
			Invocations: invocations,
			CompletedAt: &completedAt,
		})
	}
}

// sessionScopedUpdate wraps an incremental.Update with its session ID for
// the SSE wire format, matching WSUserInputEvent/WSToolCallEvent/etc.'s
// flat {type, sessionId, ...} shape (api/models.py) rather than nesting.
type sessionScopedUpdate struct {
	SessionID string `json:"sessionId"`
	incremental.Update
}

// List returns every session's summary, oldest first.
func (s *SessionStore) List() []SessionSummary {
	s.mu.Lock()
	defer s.mu.Unlock()

	summaries := make([]SessionSummary, 0, len(s.order))
	for _, id := range s.order {
		summaries = append(summaries, summarize(s.sessions[id]))
	}
	return summaries
}

// Trace rebuilds the merged parent/child span tree across every span in
// the named session, regardless of which OTLP trace ID it originally
// arrived under (mirrors how a session can span multiple traces).
func (s *SessionStore) Trace(sessionID string) (*tracepkg.Trace, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	session, ok := s.sessions[sessionID]
	if !ok {
		return nil, false
	}
	return otlp.BuildTrace(sessionID, session.Spans), true
}

// nonAgentevalsAttrs returns resourceAttrs minus any agentevals.*-prefixed
// key. Ported from ws_server.py's TraceSession construction (the
// dict comprehension at get_or_create_otlp_session's call site): the
// session's own routing/control attributes (agentevals.session_name,
// agentevals.eval_set_id) are metadata about the session, not part of
// the user-visible metadata surfaced to the UI/API.
func nonAgentevalsAttrs(resourceAttrs map[string]any) map[string]any {
	out := make(map[string]any, len(resourceAttrs))
	for k, v := range resourceAttrs {
		if !strings.HasPrefix(k, "agentevals.") {
			out[k] = v
		}
	}
	return out
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func prescanConversationID(resourceSpan map[string]any) string {
	for _, ss := range asSlice(resourceSpan["scopeSpans"]) {
		scopeSpan := asMap(ss)
		for _, sd := range asSlice(scopeSpan["spans"]) {
			spanData := asMap(sd)
			for _, a := range asSlice(spanData["attributes"]) {
				attr := asMap(a)
				if attr["key"] == genAIConversationID {
					if v, ok := asMap(attr["value"])["stringValue"].(string); ok {
						return v
					}
				}
			}
		}
	}
	return ""
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}
