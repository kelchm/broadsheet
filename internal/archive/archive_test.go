package archive

import (
	"os"
	"testing"
	"time"

	"github.com/kelchm/paperboy/internal/source"
)

func ed(date string, body string) source.Edition {
	d, _ := time.Parse("2006-01-02", date)
	return source.Edition{Date: d.UTC(), Media: source.MediaPDF, Data: []byte(body)}
}

func TestPutNewestHas(t *testing.T) {
	s := &Store{Root: t.TempDir()}

	if _, ok := s.Newest("ny-nyt"); ok {
		t.Fatal("empty archive should have no newest")
	}

	if _, err := s.Put("ny-nyt", ed("2026-06-29", "old")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	e30, err := s.Put("ny-nyt", ed("2026-06-30", "new"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, ok := s.Newest("ny-nyt")
	if !ok || got.Date.Format(dateLayout) != "20260630" || got.Media != source.MediaPDF {
		t.Errorf("Newest = %+v, want 20260630/PDF", got)
	}
	if got.Path != e30.Path {
		t.Errorf("Newest path = %s, want %s", got.Path, e30.Path)
	}

	if !s.Has("ny-nyt", mustDay("2026-06-30")) {
		t.Error("Has(20260630) = false, want true")
	}
	if s.Has("ny-nyt", mustDay("2026-06-28")) {
		t.Error("Has(20260628) = true, want false")
	}

	// The on-disk bytes are the ones we wrote.
	b, _ := os.ReadFile(got.Path)
	if string(b) != "new" {
		t.Errorf("stored bytes = %q, want new", b)
	}
}

func TestPutZeroDateRejected(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	if _, err := s.Put("ny-nyt", source.Edition{Media: source.MediaPDF, Data: []byte("x")}); err == nil {
		t.Error("Put with zero date should error")
	}
}

func TestNewestAny(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	_, _ = s.Put("ny-nyt", ed("2026-06-29", "nyt"))
	_, _ = s.Put("dc-wp", ed("2026-06-30", "wp"))

	got, ok := s.NewestAny()
	if !ok || got.SourceID != "dc-wp" {
		t.Errorf("NewestAny = %+v, want dc-wp (newest across sources)", got)
	}
}

func TestPrune(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	_, _ = s.Put("ny-nyt", ed("2026-06-10", "ancient"))
	_, _ = s.Put("ny-nyt", ed("2026-06-29", "recent"))
	now := mustDay("2026-06-30")

	removed, err := s.Prune(14*24*time.Hour, now)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if s.Has("ny-nyt", mustDay("2026-06-10")) {
		t.Error("ancient edition should have been pruned")
	}
	if !s.Has("ny-nyt", mustDay("2026-06-29")) {
		t.Error("recent edition should survive pruning")
	}
}

func mustDay(s string) time.Time {
	d, _ := time.Parse("2006-01-02", s)
	return d.UTC()
}
