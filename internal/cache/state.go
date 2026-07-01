// Package cache owns paperboy's small persistent state file.
//
// State is a single JSON file written atomically (tmp + rename). It holds
// per-source health and each provider's opaque change tokens (ETags), so
// conditional fetches survive a restart. The archived editions and rendered
// PNGs live elsewhere on the filesystem (see internal/archive); this file only
// tracks health and version tokens.
package cache

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// State is the persistent state for a paperboy instance.
//
// Rotation is deterministic from the clock and needs no stored index, so state
// is just per-source health plus each provider's opaque change tokens.
type State struct {
	Sources map[string]SourceRecord `json:"sources"`
	// Versions holds each provider's opaque change tokens (ETags), keyed by
	// source ID then by the provider's own key, so conditional fetches survive
	// a restart. The engine persists whatever a provider returns and never
	// interprets it.
	Versions map[string]map[string]string `json:"versions,omitempty"`
}

// SourceRecord captures the recent health of a single source.
type SourceRecord struct {
	LastFetchOK    *time.Time `json:"last_fetch_ok,omitempty"`
	LastFetchError *time.Time `json:"last_fetch_err,omitempty"`
	LastErrorMsg   string     `json:"last_error_msg,omitempty"`
}

// Store wraps a State with concurrent-safe access and atomic persistence.
type Store struct {
	path string
	mu   sync.Mutex
	st   State
}

// Open loads (or initializes) state at the given file path.
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	s.st = State{
		Sources:  map[string]SourceRecord{},
		Versions: map[string]map[string]string{},
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: internal state-file path (DataDir/state.json), not user input
	switch {
	case errors.Is(err, os.ErrNotExist):
		// fresh state — leave defaults
	case err != nil:
		return nil, fmt.Errorf("cache: read state: %w", err)
	default:
		if err := json.Unmarshal(data, &s.st); err != nil {
			return nil, fmt.Errorf("cache: parse state: %w", err)
		}
		if s.st.Sources == nil {
			s.st.Sources = map[string]SourceRecord{}
		}
		if s.st.Versions == nil {
			s.st.Versions = map[string]map[string]string{}
		}
	}
	return s, nil
}

// Snapshot returns a deep copy of the current state.
// Safe to read without further locking.
func (s *Store) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneState(s.st)
}

// Update applies a mutation function under lock and persists atomically.
func (s *Store) Update(fn func(*State)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.st)
	return s.persistLocked()
}

func (s *Store) persistLocked() error {
	data, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return fmt.Errorf("cache: marshal state: %w", err)
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("cache: mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".state.*.json.tmp")
	if err != nil {
		return fmt.Errorf("cache: tmp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op if rename succeeded

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("cache: write tmp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("cache: sync tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cache: close tmp: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("cache: rename: %w", err)
	}
	return nil
}

func cloneState(in State) State {
	out := State{
		Sources:  make(map[string]SourceRecord, len(in.Sources)),
		Versions: make(map[string]map[string]string, len(in.Versions)),
	}
	for k, v := range in.Sources {
		out.Sources[k] = v
	}
	for id, vers := range in.Versions {
		cp := make(map[string]string, len(vers))
		for k, v := range vers {
			cp[k] = v
		}
		out.Versions[id] = cp
	}
	return out
}

// Versions returns a copy of the persisted provider version tokens for a source.
func (s *Store) Versions(sourceID string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.st.Versions[sourceID]))
	for k, v := range s.st.Versions[sourceID] {
		out[k] = v
	}
	return out
}

// SetVersions replaces the persisted version tokens for a source.
func (s *Store) SetVersions(sourceID string, versions map[string]string) error {
	return s.Update(func(st *State) {
		if st.Versions == nil {
			st.Versions = map[string]map[string]string{}
		}
		cp := make(map[string]string, len(versions))
		for k, v := range versions {
			cp[k] = v
		}
		st.Versions[sourceID] = cp
	})
}

// RecordSuccess marks a source as having successfully acquired an edition.
func (s *Store) RecordSuccess(sourceID string, when time.Time) error {
	return s.Update(func(st *State) {
		rec := st.Sources[sourceID]
		t := when.UTC()
		rec.LastFetchOK = &t
		rec.LastErrorMsg = ""
		st.Sources[sourceID] = rec
	})
}

// RecordFailure marks a source as having failed to reach upstream.
func (s *Store) RecordFailure(sourceID, msg string, when time.Time) error {
	return s.Update(func(st *State) {
		rec := st.Sources[sourceID]
		t := when.UTC()
		rec.LastFetchError = &t
		rec.LastErrorMsg = msg
		st.Sources[sourceID] = rec
	})
}
