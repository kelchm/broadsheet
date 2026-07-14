// Package archive is broadsheet's durable store of newspaper editions.
//
// It is the source of truth: PDFs (or other artifacts) keyed by source and
// edition date, written atomically, pruned by retention. The rendered PNGs are
// a separate, disposable cache — not the archive. See docs/architecture.md.
package archive

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kelchm/broadsheet/internal/source"
)

const dateLayout = "20060102"

// metaFile is the per-source sidecar that makes the archive self-describing: it
// holds metadata captured while a source was archiving (currently just the
// display name) so a paper's history stays labeled with its real name even after
// it leaves the catalog and its store row is gone. JSON so fields can be added
// without a format change; the leading dot and non-date name keep it out of
// edition listings (list parses <date>.<ext> and skips everything else).
const metaFile = ".meta.json"

// sourceMeta is the archive's self-description for one source. Additive only —
// an older binary ignores unknown fields, a newer one defaults missing ones.
type sourceMeta struct {
	// Name is the source's display name at the time it last archived.
	Name string `json:"name,omitempty"`
}

// Store is an on-disk archive rooted at a directory. Layout:
//
//	<Root>/<sourceID>/<YYYYMMDD>.<ext>
type Store struct {
	Root string
}

// Entry is one archived edition on disk.
type Entry struct {
	SourceID string
	Date     time.Time // day precision, UTC
	Media    source.MediaType
	Path     string
}

func ext(m source.MediaType) string {
	if m == source.MediaImage {
		return ".png"
	}
	return ".pdf"
}

func mediaFromExt(e string) source.MediaType {
	switch e {
	case ".png", ".jpg", ".jpeg":
		return source.MediaImage
	default:
		return source.MediaPDF
	}
}

// Put writes an edition to the archive atomically and returns its entry. An
// edition on a day we already hold is overwritten (a re-posted/corrected
// edition wins).
func (s *Store) Put(sourceID string, ed source.Edition) (Entry, error) {
	if ed.Date.IsZero() {
		return Entry{}, fmt.Errorf("archive: edition for %s has zero date", sourceID)
	}
	dir := filepath.Join(s.Root, sourceID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return Entry{}, fmt.Errorf("archive: mkdir %s: %w", dir, err)
	}
	dst := filepath.Join(dir, ed.Date.UTC().Format(dateLayout)+ext(ed.Media))
	if err := writeAtomic(dst, ed.Data); err != nil {
		return Entry{}, err
	}
	return Entry{SourceID: sourceID, Date: dayUTC(ed.Date), Media: ed.Media, Path: dst}, nil
}

// SetName records a source's display name in its archive metadata, so the
// archive can label itself once the catalog no longer can. Idempotent; a blank
// id or name is a no-op. Read-modify-write preserves any other metadata fields.
// Written atomically like editions.
func (s *Store) SetName(sourceID, name string) error {
	if sourceID == "" || name == "" {
		return nil
	}
	dir := filepath.Join(s.Root, sourceID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("archive: mkdir %s: %w", dir, err)
	}
	m := s.meta(sourceID)
	if m.Name == name {
		return nil // already current — skip the rewrite
	}
	m.Name = name
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("archive: marshal meta for %s: %w", sourceID, err)
	}
	return writeAtomic(filepath.Join(dir, metaFile), b)
}

// Name returns the display name recorded for a source, or "" if none was ever
// written (a pre-metadata archive, or a source that never archived).
func (s *Store) Name(sourceID string) string {
	return strings.TrimSpace(s.meta(sourceID).Name)
}

// meta reads a source's archive metadata, returning the zero value when absent
// or unreadable (a pre-metadata archive is simply unlabeled, not an error).
func (s *Store) meta(sourceID string) sourceMeta {
	b, err := os.ReadFile(filepath.Join(s.Root, sourceID, metaFile)) //nolint:gosec // G304: archive path rooted at s.Root with an internal source id, not user input
	if err != nil {
		return sourceMeta{}
	}
	var m sourceMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return sourceMeta{}
	}
	return m
}

// Has reports whether an edition for (sourceID, date's day) is already stored.
func (s *Store) Has(sourceID string, date time.Time) bool {
	if date.IsZero() {
		return false
	}
	base := filepath.Join(s.Root, sourceID, date.UTC().Format(dateLayout))
	for _, e := range []string{".pdf", ".png"} {
		if fi, err := os.Stat(base + e); err == nil && fi.Size() > 0 {
			return true
		}
	}
	return false
}

// SourceIDs returns every source directory present in the archive, sorted.
func (s *Store) SourceIDs() []string {
	dirs, err := os.ReadDir(s.Root)
	if err != nil {
		return nil
	}
	var out []string
	for _, d := range dirs {
		if d.IsDir() {
			out = append(out, d.Name())
		}
	}
	sort.Strings(out)
	return out
}

// List returns a source's archived editions, oldest first.
func (s *Store) List(sourceID string) []Entry {
	return s.list(sourceID)
}

// Get returns the archived edition for (sourceID, date's day), if stored.
func (s *Store) Get(sourceID string, date time.Time) (Entry, bool) {
	if date.IsZero() {
		return Entry{}, false
	}
	base := filepath.Join(s.Root, sourceID, date.UTC().Format(dateLayout))
	for _, e := range []string{".pdf", ".png"} {
		if fi, err := os.Stat(base + e); err == nil && fi.Size() > 0 {
			return Entry{
				SourceID: sourceID, Date: dayUTC(date), Media: mediaFromExt(e),
				Path: base + e,
			}, true
		}
	}
	return Entry{}, false
}

// Newest returns the newest stored edition for a source.
func (s *Store) Newest(sourceID string) (Entry, bool) {
	entries := s.list(sourceID)
	if len(entries) == 0 {
		return Entry{}, false
	}
	return entries[len(entries)-1], true // list is ascending by date
}

// NewestAny returns the newest edition across all sources (the stale fallback).
func (s *Store) NewestAny() (Entry, bool) {
	dirs, err := os.ReadDir(s.Root)
	if err != nil {
		return Entry{}, false
	}
	var best Entry
	found := false
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		if e, ok := s.Newest(d.Name()); ok {
			if !found || e.Date.After(best.Date) {
				best, found = e, true
			}
		}
	}
	return best, found
}

// Prune removes editions older than retention (relative to now). Returns the
// number of files removed.
func (s *Store) Prune(retention time.Duration, now time.Time) (int, error) {
	cutoff := dayUTC(now.Add(-retention))
	dirs, err := os.ReadDir(s.Root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("archive: read root: %w", err)
	}
	removed := 0
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		for _, e := range s.list(d.Name()) {
			if e.Date.Before(cutoff) {
				if err := os.Remove(e.Path); err == nil {
					removed++
				}
			}
		}
		// Reclaim a source directory once no editions remain — e.g. a paper the
		// catalog dropped, whose editions have all aged out. RemoveAll also clears
		// the display-name label and any write litter; an active source that Puts
		// again just re-creates the directory (and re-writes its label).
		if len(s.list(d.Name())) == 0 {
			_ = os.RemoveAll(filepath.Join(s.Root, d.Name()))
		}
	}
	return removed, nil
}

// list returns a source's entries sorted ascending by date.
func (s *Store) list(sourceID string) []Entry {
	dir := filepath.Join(s.Root, sourceID)
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Entry
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		e := filepath.Ext(f.Name())
		day, err := time.Parse(dateLayout, strings.TrimSuffix(f.Name(), e))
		if err != nil {
			continue
		}
		fi, err := f.Info()
		if err != nil || fi.Size() == 0 {
			continue
		}
		out = append(out, Entry{
			SourceID: sourceID, Date: day.UTC(), Media: mediaFromExt(e),
			Path: filepath.Join(dir, f.Name()),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date.Before(out[j].Date) })
	return out
}

func dayUTC(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func writeAtomic(dst string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return fmt.Errorf("archive: tmp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("archive: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("archive: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("archive: close: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("archive: rename %s: %w", dst, err)
	}
	return nil
}
