package cache

import (
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
	if snap.Rotation.NextIndex != 0 {
		t.Errorf("fresh NextIndex = %d, want 0", snap.Rotation.NextIndex)
	}
	if snap.Sources == nil || snap.Cache == nil {
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
	if err := s1.Update(func(st *State) {
		st.Rotation.NextIndex = 5
		st.Sources["ny-nyt"] = SourceRecord{LastFetchOK: &now}
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	snap := s2.Snapshot()
	if snap.Rotation.NextIndex != 5 {
		t.Errorf("persisted NextIndex = %d, want 5", snap.Rotation.NextIndex)
	}
	rec, ok := snap.Sources["ny-nyt"]
	if !ok {
		t.Fatalf("source record missing")
	}
	if rec.LastFetchOK == nil || !rec.LastFetchOK.Equal(now) {
		t.Errorf("persisted LastFetchOK = %v, want %v", rec.LastFetchOK, now)
	}
}
