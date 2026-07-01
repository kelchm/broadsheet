package reconcile

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/kelchm/paperboy/internal/archive"
	"github.com/kelchm/paperboy/internal/cache"
	"github.com/kelchm/paperboy/internal/source"
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
	src := source.Source{ID: "ny-nyt", Provider: prov}
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
