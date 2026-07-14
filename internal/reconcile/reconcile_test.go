package reconcile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kelchm/broadsheet/internal/archive"
	"github.com/kelchm/broadsheet/internal/cache"
	"github.com/kelchm/broadsheet/internal/source"
)

type fakeProvider struct {
	editions []source.Edition
	versions map[string]string
	err      error
	calls    int
}

func (f *fakeProvider) Poll(_ context.Context, _ source.Deps, _ map[string]string, _ time.Time) (
	[]source.Edition, map[string]string, error) {
	f.calls++
	return f.editions, f.versions, f.err
}

func newReconciler(t *testing.T, srcs []source.Source) (*Reconciler, *archive.Store, *cache.Store) {
	t.Helper()
	dir := t.TempDir()
	store, err := cache.Open(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	arch := &archive.Store{Root: filepath.Join(dir, "archive")}
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	return &Reconciler{
		Sources:   srcs,
		Archive:   arch,
		Store:     store,
		Retention: 14 * 24 * time.Hour,
		Now:       func() time.Time { return now },
	}, arch, store
}

func TestReconcileOnce_ArchivesAndPersists(t *testing.T) {
	prov := &fakeProvider{
		editions: []source.Edition{{
			Date:  time.Date(2026, 6, 30, 5, 0, 0, 0, time.UTC),
			Media: source.MediaPDF, Data: []byte("PDF"), Version: "e30",
		}},
		versions: map[string]string{"url30": "e30"},
	}
	src := source.Source{ID: "ny-nyt", DisplayName: "The New York Times", Provider: prov}
	r, arch, store := newReconciler(t, []source.Source{src})

	r.ReconcileOnce(context.Background())

	if _, ok := arch.Newest("ny-nyt"); !ok {
		t.Error("expected an archived edition after reconcile")
	}
	if v := store.Versions("ny-nyt"); v["url30"] != "e30" {
		t.Errorf("versions not persisted: %v", v)
	}
	if rec := store.Snapshot().Sources["ny-nyt"]; rec.LastFetchOK == nil {
		t.Error("expected LastFetchOK recorded after storing an edition")
	}
	// The archive is stamped with the display name so its history stays labeled
	// after the paper leaves the catalog.
	if got := arch.Name("ny-nyt"); got != "The New York Times" {
		t.Errorf("archive label = %q, want the source's display name", got)
	}
}

func TestReconcileOnce_PollErrorRecordsFailure(t *testing.T) {
	prov := &fakeProvider{err: errors.New("upstream down")}
	src := source.Source{ID: "ny-nyt", Provider: prov}
	r, arch, store := newReconciler(t, []source.Source{src})

	r.ReconcileOnce(context.Background())

	if _, ok := arch.Newest("ny-nyt"); ok {
		t.Error("no edition should be archived on poll error")
	}
	rec := store.Snapshot().Sources["ny-nyt"]
	if rec.LastFetchError == nil || rec.LastErrorMsg == "" {
		t.Errorf("expected failure recorded, got %+v", rec)
	}
}

func TestReconcileSource_PutFailureRevertsTokenAndRecordsFailure(t *testing.T) {
	// A zero-date edition is rejected by archive.Put. Its fresh ETag must NOT
	// be carried forward (or the next poll 304s past the edition forever), and
	// the failure must be visible in health.
	prov := &fakeProvider{
		editions: []source.Edition{{
			// zero Date -> archive.Put error
			Media: source.MediaPDF, Data: []byte("PDF"), Version: "e-new",
		}},
		versions: map[string]string{"url30": "e-new", "url29": "e29"},
	}
	src := source.Source{ID: "ny-nyt", Provider: prov}
	r, arch, store := newReconciler(t, []source.Source{src})

	// Seed a previously-seen token for url30 so we can observe the revert.
	if err := store.SetVersions("ny-nyt", map[string]string{"url30": "e-old"}); err != nil {
		t.Fatalf("SetVersions: %v", err)
	}

	r.ReconcileOnce(context.Background())

	if _, ok := arch.Newest("ny-nyt"); ok {
		t.Error("zero-date edition must not be archived")
	}
	v := store.Versions("ny-nyt")
	if v["url30"] != "e-old" {
		t.Errorf("url30 token = %q, want reverted e-old (burning e-new loses the edition)", v["url30"])
	}
	if v["url29"] != "e29" {
		t.Errorf("url29 token = %q, want e29 (unrelated tokens still carry forward)", v["url29"])
	}
	rec := store.Snapshot().Sources["ny-nyt"]
	if rec.LastFetchError == nil || rec.LastErrorMsg == "" {
		t.Errorf("expected put failure recorded in health, got %+v", rec)
	}
}

func TestReconcileSource_PutFailureWithNoPriorTokenDropsKey(t *testing.T) {
	prov := &fakeProvider{
		editions: []source.Edition{{
			Media: source.MediaPDF, Data: []byte("PDF"), Version: "e-new",
		}},
		versions: map[string]string{"url30": "e-new"},
	}
	src := source.Source{ID: "ny-nyt", Provider: prov}
	r, _, store := newReconciler(t, []source.Source{src})

	r.ReconcileOnce(context.Background())

	if v := store.Versions("ny-nyt"); v["url30"] != "" {
		t.Errorf("url30 token = %q, want dropped so the next poll re-fetches", v["url30"])
	}
}

func TestReconcileOnce_NoEditionsIsNotFailure(t *testing.T) {
	// All-304 poll: no editions, no error. Health should be untouched (neither
	// success nor failure), versions still carried forward.
	prov := &fakeProvider{versions: map[string]string{"url30": "e30"}}
	src := source.Source{ID: "ny-nyt", Provider: prov}
	r, _, store := newReconciler(t, []source.Source{src})

	r.ReconcileOnce(context.Background())

	rec := store.Snapshot().Sources["ny-nyt"]
	if rec.LastFetchError != nil {
		t.Error("a clean no-op poll must not record a failure")
	}
	if store.Versions("ny-nyt")["url30"] != "e30" {
		t.Error("versions should carry forward on a no-op poll")
	}
}

func TestPruneCache_RemovesAgedRendersOnly(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "ny-nyt")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	files := map[string]bool{ // name -> should survive a 14-day retention
		"20260630.w1600.png": true,  // today
		"20260620.w1600.png": true,  // inside retention
		"20260601.w1600.png": false, // aged out
		"20260601.png":       false, // old (pre-width-key) format, aged out
		".png-abc123.tmp":    true,  // fresh temp litter; leave alone
		".png-orphan.tmp":    false, // day-old temp litter from a crashed write
		"not-a-date.png":     true,  // unparseable; leave alone
	}
	for name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Temp-litter pruning is mtime-based against the injected now (2026-06-30).
	stale := time.Date(2026, 6, 28, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(dir, ".png-orphan.tmp"), stale, stale); err != nil {
		t.Fatal(err)
	}

	removed, err := pruneCache(root, 14*24*time.Hour, now)
	if err != nil {
		t.Fatalf("pruneCache: %v", err)
	}
	if removed != 3 {
		t.Errorf("removed = %d, want 3", removed)
	}
	for name, keep := range files {
		_, err := os.Stat(filepath.Join(dir, name))
		if keep && err != nil {
			t.Errorf("%s should have survived: %v", name, err)
		}
		if !keep && !os.IsNotExist(err) {
			t.Errorf("%s should have been pruned", name)
		}
	}
}

func TestPruneCache_MissingRootIsNotAnError(t *testing.T) {
	if _, err := pruneCache(filepath.Join(t.TempDir(), "nope"), time.Hour, time.Now()); err != nil {
		t.Fatalf("pruneCache on missing root: %v", err)
	}
}

func TestReconcileOnce_PrunesConfiguredCacheDir(t *testing.T) {
	r, _, _ := newReconciler(t, nil)
	cacheDir := t.TempDir()
	r.CacheDir = cacheDir
	dir := filepath.Join(cacheDir, "ny-nyt")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	aged := filepath.Join(dir, "20260601.w1600.png") // reconciler's injected now is 2026-06-30
	if err := os.WriteFile(aged, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	r.ReconcileOnce(context.Background())

	if _, err := os.Stat(aged); !os.IsNotExist(err) {
		t.Error("ReconcileOnce should prune aged renders from CacheDir")
	}
}
