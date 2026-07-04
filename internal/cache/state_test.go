package cache

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenFreshState(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	snap := s.Snapshot()
	if snap.Sources == nil || snap.Versions == nil {
		t.Errorf("fresh state has nil maps")
	}
}

func TestUpdatePersistsAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	s1, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := s1.RecordSuccess("ny-nyt", now); err != nil {
		t.Fatalf("RecordSuccess: %v", err)
	}
	if err := s1.SetVersions("ny-nyt", map[string]string{"url30": "etag30"}); err != nil {
		t.Fatalf("SetVersions: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	snap := s2.Snapshot()

	rec, ok := snap.Sources["ny-nyt"]
	if !ok {
		t.Fatalf("source record missing")
	}
	if rec.LastFetchOK == nil || !rec.LastFetchOK.Equal(now) {
		t.Errorf("persisted LastFetchOK = %v, want %v", rec.LastFetchOK, now)
	}
	if got := s2.Versions("ny-nyt")["url30"]; got != "etag30" {
		t.Errorf("persisted version = %q, want etag30", got)
	}
}

func TestOpen_CorruptStateStartsFreshWithBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"sources": {truncated`), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on corrupt state must recover, got: %v", err)
	}
	snap := s.Snapshot()
	if len(snap.Sources) != 0 || len(snap.Versions) != 0 {
		t.Errorf("expected fresh state, got %+v", snap)
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Errorf("corrupt file should be preserved as .corrupt: %v", err)
	}

	// The store must be fully usable (persist works) after recovery.
	if err := s.RecordSuccess("a", time.Now()); err != nil {
		t.Fatalf("RecordSuccess after recovery: %v", err)
	}
}
