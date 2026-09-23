package api

import (
	"path/filepath"
	"testing"
	"time"
)

// TestSessionPersistence_RoundTrip proves the actual restart scenario the
// archive exists for: ingest into one store backed by a SQLite file, close
// it, open a fresh store against the same file, and confirm the session
// (including its spans, not just its summary) comes back.
func TestSessionPersistence_RoundTrip(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")

	archive1, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening archive: %v", err)
	}
	store1 := NewSessionStoreWithArchive(nil, archive1)

	store1.Ingest(sampleTracesBody("persisted-sess", "aaaa", "span-a"))
	if err := store1.flushPersist(); err != nil {
		t.Fatalf("flushPersist: %v", err)
	}
	if err := store1.Close(); err != nil {
		t.Fatalf("closing store1: %v", err)
	}

	archive2, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("reopening archive: %v", err)
	}
	defer archive2.Close()
	store2 := NewSessionStoreWithArchive(nil, archive2)
	if err := store2.LoadPersisted(); err != nil {
		t.Fatalf("LoadPersisted: %v", err)
	}

	sessions := store2.List()
	if len(sessions) != 1 {
		t.Fatalf("got %d restored sessions, want 1", len(sessions))
	}
	if sessions[0].SessionID != "persisted-sess" {
		t.Errorf("session ID = %q, want %q", sessions[0].SessionID, "persisted-sess")
	}
	if sessions[0].SpanCount != 1 {
		t.Errorf("span count = %d, want 1", sessions[0].SpanCount)
	}

	tr, ok := store2.Trace("persisted-sess")
	if !ok {
		t.Fatal("expected restored session's trace to be retrievable")
	}
	if len(tr.AllSpans) != 1 || tr.AllSpans[0].SpanID != "span-a" {
		t.Errorf("restored spans = %v, want [span-a]", tr.AllSpans)
	}
}

// TestSessionPersistence_RestoredIncompleteSessionEventuallyCompletes
// reproduces a bug found via a live side-by-side comparison: a session
// that was still active (IsComplete=false) when persisted, and whose
// producer had already stopped sending spans before the process
// restarted, came back from the archive with no timer driving it toward
// completion - its duration ticked in the UI forever, across further
// restarts, since nothing would ever call resetIdleTimerLocked again.
// LoadPersisted must kick off a fresh idle timer for exactly this case.
func TestSessionPersistence_RestoredIncompleteSessionEventuallyCompletes(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")

	archive1, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening archive: %v", err)
	}
	store1 := NewSessionStoreWithArchive(nil, archive1)
	store1.completionGrace = time.Hour // never fires: persist while still "active"

	store1.Ingest(sampleTracesBody("stale-sess", "aaaa", "span-a"))
	if store1.List()[0].IsComplete {
		t.Fatal("test setup: session must be incomplete when persisted")
	}
	if err := store1.flushPersist(); err != nil {
		t.Fatalf("flushPersist: %v", err)
	}
	if err := store1.Close(); err != nil {
		t.Fatalf("closing store1: %v", err)
	}

	archive2, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("reopening archive: %v", err)
	}
	defer archive2.Close()
	store2 := NewSessionStoreWithArchive(nil, archive2)
	store2.idleTimeout = 10 * time.Millisecond
	if err := store2.LoadPersisted(); err != nil {
		t.Fatalf("LoadPersisted: %v", err)
	}

	waitForCondition(t, time.Second, func() bool {
		return store2.List()[0].IsComplete
	})
}

// TestSQLiteStore_Delete confirms a deleted session no longer loads back.
func TestSQLiteStore_Delete(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	archive, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening archive: %v", err)
	}
	defer archive.Close()

	store := NewSessionStoreWithArchive(nil, archive)
	store.Ingest(sampleTracesBody("to-delete", "aaaa", "span-a"))
	if err := store.flushPersist(); err != nil {
		t.Fatalf("flushPersist: %v", err)
	}

	if err := archive.Delete("to-delete"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	snapshots, err := archive.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if _, ok := snapshots["to-delete"]; ok {
		t.Error("deleted session still present in archive")
	}
}

// TestSessionStore_NoArchiveIsNoop confirms the store works exactly as
// before when no SQLite path is configured (LoadPersisted/StartPersistence/
// Close all become no-ops).
func TestSessionStore_NoArchiveIsNoop(t *testing.T) {
	store := NewSessionStore(nil)
	if err := store.LoadPersisted(); err != nil {
		t.Errorf("LoadPersisted with no archive: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Errorf("Close with no archive: %v", err)
	}
}
