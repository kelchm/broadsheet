// Package reconcile is paperboy's eager background loop: it keeps the local
// archive current by polling each source's provider, storing new editions, and
// pruning old ones — independent of any incoming HTTP request.
//
// This is the only part of paperboy that touches the network. Everything the
// HTTP layer does is a pure read over the archive the reconciler maintains.
package reconcile

import (
	"context"
	"log/slog"
	"time"

	"github.com/kelchm/paperboy/internal/archive"
	"github.com/kelchm/paperboy/internal/cache"
	"github.com/kelchm/paperboy/internal/source"
)

// Reconciler keeps the archive up to date for a set of sources.
type Reconciler struct {
	Sources   []source.Source
	Archive   *archive.Store
	Store     *cache.Store
	Deps      source.Deps
	Retention time.Duration
	Interval  time.Duration
	Logger    *slog.Logger

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
	for _, src := range r.Sources {
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

	stored := 0
	for _, ed := range editions {
		if _, err := r.Archive.Put(src.ID, ed); err != nil {
			log.Warn("archive put failed", "source", src.ID, "err", err)
			continue
		}
		stored++
	}

	// Persist version tokens regardless — 304s update nothing but we still want
	// to carry tokens forward.
	if err := r.Store.SetVersions(src.ID, versions); err != nil {
		log.Warn("persist versions failed", "source", src.ID, "err", err)
	}

	if stored > 0 {
		_ = r.Store.RecordSuccess(src.ID, now)
		log.Info("archived editions", "source", src.ID, "count", stored)
	}
}
