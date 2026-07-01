package rotation

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/kelchm/paperboy/internal/cache"
	"github.com/kelchm/paperboy/internal/fetch"
	"github.com/kelchm/paperboy/internal/source"
)

// fakeFetcher returns a fixed result per (sourceID, daysAgo).
type fakeFetcher struct {
	results map[string]map[int]fakeResult
}

type fakeResult struct {
	path string
	at   time.Time
	err  error
}

func (f *fakeFetcher) FetchAndRender(_ context.Context, src source.Source, daysAgo int) (string, time.Time, error) {
	if byDay, ok := f.results[src.ID]; ok {
		if r, ok := byDay[daysAgo]; ok {
			return r.path, r.at, r.err
		}
	}
	return "", time.Time{}, fetch.ErrNotFound
}

type fakeLookup struct {
	sourceID  string
	path      string
	fetchedAt time.Time
	ok        bool
}

func (l *fakeLookup) MostRecent() (string, string, time.Time, bool) {
	return l.sourceID, l.path, l.fetchedAt, l.ok
}

func newStore(t *testing.T) *cache.Store {
	t.Helper()
	s, err := cache.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	return s
}

func TestPickNext_TodaySucceeds(t *testing.T) {
	srcs := []source.Source{{ID: "a"}, {ID: "b"}}
	now := time.Now()
	p := &Picker{
		Sources: srcs,
		Store:   newStore(t),
		Fetcher: &fakeFetcher{results: map[string]map[int]fakeResult{
			"a": {0: {path: "/cache/a/today.png", at: now}},
		}},
	}
	res, err := p.PickNext(context.Background())
	if err != nil {
		t.Fatalf("PickNext: %v", err)
	}
	if res.SourceID != "a" || res.DaysOld != 0 || res.Stale {
		t.Errorf("got %+v, want a/0/!stale", res)
	}
}

func TestPickNext_FallsBackToYesterday(t *testing.T) {
	srcs := []source.Source{{ID: "a"}}
	now := time.Now()
	p := &Picker{
		Sources: srcs,
		Store:   newStore(t),
		Fetcher: &fakeFetcher{results: map[string]map[int]fakeResult{
			"a": {
				0: {err: fetch.ErrNotFound},
				1: {path: "/cache/a/yesterday.png", at: now},
			},
		}},
	}
	res, err := p.PickNext(context.Background())
	if err != nil {
		t.Fatalf("PickNext: %v", err)
	}
	if res.SourceID != "a" || res.DaysOld != 1 {
		t.Errorf("got %+v, want a/1", res)
	}
}

func TestPickNext_FallsAcrossSources(t *testing.T) {
	srcs := []source.Source{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	now := time.Now()
	p := &Picker{
		Sources: srcs,
		Store:   newStore(t),
		Fetcher: &fakeFetcher{results: map[string]map[int]fakeResult{
			// "a" has nothing
			// "b" has today
			"b": {0: {path: "/cache/b/today.png", at: now}},
		}},
	}
	res, err := p.PickNext(context.Background())
	if err != nil {
		t.Fatalf("PickNext: %v", err)
	}
	if res.SourceID != "b" {
		t.Errorf("expected fallback to source b, got %s", res.SourceID)
	}
}

func TestPickNext_AllFail_ServesCachedFallback(t *testing.T) {
	srcs := []source.Source{{ID: "a"}, {ID: "b"}}
	cachedAt := time.Now().Add(-24 * time.Hour)
	p := &Picker{
		Sources: srcs,
		Store:   newStore(t),
		Fetcher: &fakeFetcher{},
		Lookup: &fakeLookup{
			sourceID: "a", path: "/cache/a/yesterday.png", fetchedAt: cachedAt, ok: true,
		},
	}
	res, err := p.PickNext(context.Background())
	if err != nil {
		t.Fatalf("PickNext: %v", err)
	}
	if !res.Stale || res.SourceID != "a" {
		t.Errorf("expected stale fallback to a, got %+v", res)
	}
}

func TestPickNext_AllFailAndNoCache_ReturnsError(t *testing.T) {
	srcs := []source.Source{{ID: "a"}}
	p := &Picker{
		Sources: srcs,
		Store:   newStore(t),
		Fetcher: &fakeFetcher{},
		Lookup:  &fakeLookup{ok: false},
	}
	_, err := p.PickNext(context.Background())
	if !errors.Is(err, ErrNoneAvailable) {
		t.Errorf("expected ErrNoneAvailable, got %v", err)
	}
}

func TestPickFor_RecordsSuccessAndDoesNotAdvance(t *testing.T) {
	srcs := []source.Source{{ID: "a"}, {ID: "b"}}
	now := time.Now()
	store := newStore(t)
	p := &Picker{
		Sources: srcs,
		Store:   store,
		Fetcher: &fakeFetcher{results: map[string]map[int]fakeResult{
			"b": {0: {path: "/cache/b/today.png", at: now}},
		}},
	}
	res, err := p.PickFor(context.Background(), "b")
	if err != nil {
		t.Fatalf("PickFor: %v", err)
	}
	if res.SourceID != "b" || res.Stale {
		t.Errorf("got %+v, want b/!stale", res)
	}
	snap := store.Snapshot()
	if snap.Rotation.NextIndex != 0 {
		t.Errorf("PickFor advanced rotation to %d, want 0 (must not advance)", snap.Rotation.NextIndex)
	}
	if snap.Sources["b"].LastFetchOK == nil {
		t.Errorf("PickFor did not record success health for b")
	}
}

func TestPickFor_NoPaper_RecordsFailure(t *testing.T) {
	store := newStore(t)
	p := &Picker{
		Sources: []source.Source{{ID: "a"}},
		Store:   store,
		Fetcher: &fakeFetcher{}, // "a" has no paper
	}
	if _, err := p.PickFor(context.Background(), "a"); err == nil {
		t.Fatal("expected error when no paper is available")
	}
	if store.Snapshot().Sources["a"].LastFetchError == nil {
		t.Errorf("PickFor did not record failure health for a")
	}
}

func TestPickFor_UnknownSource(t *testing.T) {
	p := &Picker{
		Sources: []source.Source{{ID: "a"}},
		Store:   newStore(t),
		Fetcher: &fakeFetcher{},
	}
	if _, err := p.PickFor(context.Background(), "nope"); err == nil {
		t.Fatal("expected error for unknown source")
	}
}

func TestPickNext_AdvancesRotation(t *testing.T) {
	srcs := []source.Source{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	now := time.Now()
	store := newStore(t)
	p := &Picker{
		Sources: srcs,
		Store:   store,
		Fetcher: &fakeFetcher{results: map[string]map[int]fakeResult{
			"a": {0: {path: "p", at: now}},
			"b": {0: {path: "p", at: now}},
			"c": {0: {path: "p", at: now}},
		}},
	}
	first, _ := p.PickNext(context.Background())
	second, _ := p.PickNext(context.Background())
	if first.SourceID == second.SourceID {
		t.Errorf("rotation didn't advance: %s, %s", first.SourceID, second.SourceID)
	}
}
