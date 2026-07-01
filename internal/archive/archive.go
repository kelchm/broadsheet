// Package archive is paperboy's durable store of newspaper editions.
//
// It is the source of truth: PDFs (or other artifacts) keyed by source and
// edition date, written atomically, pruned by retention. The rendered PNGs are
// a separate, disposable cache — not the archive. See docs/architecture.md.
package archive

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kelchm/paperboy/internal/source"
)

const dateLayout = "20060102"

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
