// Package cache owns the on-disk state and image cache.
//
// State is a single JSON file written atomically (tmp + rename). It holds the
// rotation index, per-source health, and the index of cached images. The cache
// itself (the actual PNG/PDF bytes) lives next to it on the filesystem.
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
type State struct {
	Rotation Rotation                `json:"rotation"`
	Sources  map[string]SourceRecord `json:"sources"`
	Cache    map[string]CacheRecord  `json:"cache"`
}

// Rotation tracks which source is next.
type Rotation struct {
	NextIndex int `json:"next_index"`
}

// SourceRecord captures the recent health of a single source.
type SourceRecord struct {
	LastFetchOK    *time.Time `json:"last_fetch_ok,omitempty"`
	LastFetchError *time.Time `json:"last_fetch_err,omitempty"`
	LastErrorMsg   string     `json:"last_error_msg,omitempty"`
}

// CacheRecord is the per-source cache index keyed by YYYYMMDD.
type CacheRecord struct {
	Images map[string]CacheEntry `json:"images"`
}

// CacheEntry is one cached image on disk.
type CacheEntry struct {
	Path      string    `json:"path"`
	CreatedAt time.Time `json:"created_at"`
	Bytes     int64     `json:"bytes"`
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
		Sources: map[string]SourceRecord{},
		Cache:   map[string]CacheRecord{},
	}
	data, err := os.ReadFile(path)
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
		if s.st.Cache == nil {
			s.st.Cache = map[string]CacheRecord{}
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
	if err := os.MkdirAll(dir, 0o755); err != nil {
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
		Rotation: in.Rotation,
		Sources:  make(map[string]SourceRecord, len(in.Sources)),
		Cache:    make(map[string]CacheRecord, len(in.Cache)),
	}
	for k, v := range in.Sources {
		out.Sources[k] = v
	}
	for k, v := range in.Cache {
		images := make(map[string]CacheEntry, len(v.Images))
		for ik, iv := range v.Images {
			images[ik] = iv
		}
		out.Cache[k] = CacheRecord{Images: images}
	}
	return out
}
