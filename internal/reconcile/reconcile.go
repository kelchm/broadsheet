// Package reconcile is broadsheet's eager background loop: it keeps the local
// archive current by polling each source's provider, storing new editions, and
// pruning old ones — independent of any incoming HTTP request.
//
// This is the only part of broadsheet that touches the network. Everything the
// HTTP layer does is a pure read over the archive the reconciler maintains.
package reconcile

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kelchm/broadsheet/internal/archive"
	"github.com/kelchm/broadsheet/internal/source"
)

// StateStore is what the reconciler needs from persistent state: provider
// version tokens and per-source health records. Satisfied by internal/store
// (SQLite) and the legacy internal/cache JSON store alike.
type StateStore interface {
	Versions(sourceID string) map[string]string
	SetVersions(sourceID string, versions map[string]string) error
	RecordSuccess(sourceID string, when time.Time) error
	RecordFailure(sourceID, msg string, when time.Time) error
}

// fetchEventPruner is optionally implemented by stores that keep health
// history (the SQLite store does; the legacy JSON store doesn't).
type fetchEventPruner interface {
	PruneFetchEvents(retention time.Duration, now time.Time) (int, error)
}

// pollRecorder is optionally implemented by stores that track clean polls
// separately from stored editions (the two-timestamp health model).
type pollRecorder interface {
	RecordPoll(sourceID string, when time.Time) error
}

// Reconciler keeps the archive up to date for a set of sources.
type Reconciler struct {
	// Sources is a static set; SourcesFn, when set, is consulted each cycle
	// instead (a live view, so runtime enable/disable applies next cycle).
	Sources   []source.Source
	SourcesFn func() []source.Source
	Archive   *archive.Store
	Store     StateStore
	Deps      source.Deps
	Retention time.Duration
	Interval  time.Duration
	Logger    *slog.Logger

	// CacheDir is the disposable render cache; if set, cached PNGs are pruned
	// on the same retention as the archive (they are worthless once their
	// artifact is gone).
	CacheDir string

	// Now is the clock; nil means time.Now. Injected for tests.
	Now func() time.Time
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reconciler) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

// Run performs an immediate reconcile, then reconciles every Interval until the
// context is canceled. Intended to be launched in its own goroutine.
func (r *Reconciler) Run(ctx context.Context) {
	r.ReconcileOnce(ctx)

	if r.Interval <= 0 {
		return
	}
	t := time.NewTicker(r.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.ReconcileOnce(ctx)
		}
	}
}

// ReconcileOnce polls every source once and prunes the archive. Sources are
// polled sequentially, which naturally staggers upstream requests.
func (r *Reconciler) ReconcileOnce(ctx context.Context) {
	now := r.now()
	srcs := r.Sources
	if r.SourcesFn != nil {
		srcs = r.SourcesFn()
	}
	for _, src := range srcs {
		select {
		case <-ctx.Done():
			return
		default:
		}
		r.ReconcileSource(ctx, src, now)
	}

	if removed, err := r.Archive.Prune(r.Retention, now); err != nil {
		r.logger().Warn("archive prune failed", "err", err)
	} else if removed > 0 {
		r.logger().Info("pruned old editions", "count", removed)
	}

	if r.CacheDir != "" {
		if removed, err := pruneCache(r.CacheDir, r.Retention, now); err != nil {
			r.logger().Warn("render cache prune failed", "err", err)
		} else if removed > 0 {
			r.logger().Info("pruned cached renders", "count", removed)
		}
	}

	// Health history follows the same retention as everything else, when the
	// store keeps history at all.
	if p, ok := r.Store.(fetchEventPruner); ok {
		if _, err := p.PruneFetchEvents(r.Retention, now); err != nil {
			r.logger().Warn("fetch event prune failed", "err", err)
		}
	}
}

// pruneCache removes cached render PNGs older than retention, plus atomic-write
// temp litter (.…tmp) older than a day (a crash between CreateTemp and Rename
// orphans them). Cache filenames start with the edition date (YYYYMMDD…);
// anything else that doesn't parse is left alone. Errors on individual files
// are skipped — the cache is disposable.
func pruneCache(root string, retention time.Duration, now time.Time) (int, error) {
	c := now.Add(-retention).UTC()
	cutoff := time.Date(c.Year(), c.Month(), c.Day(), 0, 0, 0, 0, time.UTC)
	tmpCutoff := now.Add(-24 * time.Hour)

	dirs, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		dir := filepath.Join(root, d.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			name := f.Name()
			if f.IsDir() {
				continue
			}
			if strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".tmp") {
				if fi, err := f.Info(); err == nil && fi.ModTime().Before(tmpCutoff) {
					if os.Remove(filepath.Join(dir, name)) == nil {
						removed++
					}
				}
				continue
			}
			if len(name) < 8 {
				continue
			}
			day, err := time.Parse("20060102", name[:8])
			if err != nil {
				continue
			}
			if day.Before(cutoff) {
				if os.Remove(filepath.Join(dir, name)) == nil {
					removed++
				}
			}
		}
	}
	return removed, nil
}

// ReconcileSource polls one source and archives any new editions, recording
// health. Exported so a single source can be refreshed on demand (e.g. the CLI).
func (r *Reconciler) ReconcileSource(ctx context.Context, src source.Source, now time.Time) {
	log := r.logger()

	seen := r.Store.Versions(src.ID)
	editions, versions, err := src.Provider.Poll(ctx, r.Deps, seen, now)
	if err != nil {
		_ = r.Store.RecordFailure(src.ID, err.Error(), now)
		log.Warn("reconcile poll failed", "source", src.ID, "err", err)
		return
	}
	// The poll itself succeeded (even if it was all 304s, or a Put fails
	// below): record reachability so a weekend of no-op polls doesn't look
	// like a dead source.
	if pr, ok := r.Store.(pollRecorder); ok {
		_ = pr.RecordPoll(src.ID, now)
	}

	stored := 0
	var putErr error
	failed := map[string]bool{} // Version tokens of editions that failed to store
	for _, ed := range editions {
		if _, err := r.Archive.Put(src.ID, ed); err != nil {
			log.Warn("archive put failed", "source", src.ID, "err", err)
			putErr = err
			if ed.Version != "" {
				failed[ed.Version] = true
			}
			continue
		}
		stored++
	}

	// A token for an edition we failed to store must not be carried forward:
	// the next conditional fetch would 304 on it and the edition would never be
	// re-fetched (upstream only keeps a short live window). Reverting to the
	// previously-seen token (or dropping the key) makes the next poll retry the
	// download instead. Editions with an empty Version can't be matched back to
	// a token; providers are expected to set Version whenever they set one in
	// the versions map.
	//
	// Matching is by token VALUE (Edition carries no versions-map key), so if
	// two keys ever share a token and only one edition fails, the other's token
	// is reverted too — costing one redundant refetch, never a lost edition.
	// Accepted tradeoff until Edition grows a provider key.
	if len(failed) > 0 {
		for k, v := range versions {
			if !failed[v] {
				continue
			}
			if old, ok := seen[k]; ok {
				versions[k] = old
			} else {
				delete(versions, k)
			}
		}
	}

	// Persist version tokens even when nothing stored — 304s update nothing but
	// we still want to carry tokens forward.
	if err := r.Store.SetVersions(src.ID, versions); err != nil {
		log.Warn("persist versions failed", "source", src.ID, "err", err)
	}

	if stored > 0 {
		_ = r.Store.RecordSuccess(src.ID, now)
		log.Info("archived editions", "source", src.ID, "count", stored)
	}
	// A failed Put is a real failure even if other editions stored fine —
	// otherwise the archive silently stops updating behind a healthy-looking
	// record.
	if putErr != nil {
		_ = r.Store.RecordFailure(src.ID, "archive put: "+putErr.Error(), now)
	}
}
