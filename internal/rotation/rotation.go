// Package rotation orchestrates source selection + the cross-source graceful
// fallback that fixes the original "Newspaper File Not Found" bug.
//
// The algorithm:
//
//  1. Pick the next source in round-robin order (rotation.NextIndex).
//  2. For that source, try to fetch today → yesterday → 2 days ago.
//  3. If any succeeds, render it and return.
//  4. If all three dates fail, mark the source unhealthy, advance the rotation
//     index, and repeat from step 1 with the next source.
//  5. If every source fails, return the most recently cached image from ANY
//     source with Stale=true.
//  6. Only if no cache exists anywhere do we return an error to the caller.
//
// In short: the caller should virtually never see a hard error.
package rotation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/kelchm/paperboy/internal/cache"
	"github.com/kelchm/paperboy/internal/source"
)

// Result describes the chosen render.
type Result struct {
	SourceID  string
	PNGPath   string
	FetchedAt time.Time
	DaysOld   int
	Stale     bool // true if served from cache because all live fetches failed
}

// ErrNoneAvailable is returned only if no source could be fetched AND no cache
// exists. In a healthy installation this should never be returned after the
// first successful fetch.
var ErrNoneAvailable = errors.New("rotation: no sources available and no cache to serve")

// Fetcher renders a single (source, daysAgo) attempt. Implementations live in
// the higher-level orchestrator that wires fetch → rasterize → crop → cache.
type Fetcher interface {
	// FetchAndRender attempts to acquire and render the paper for src at the
	// given daysAgo offset. Returns the on-disk PNG path on success.
	// Returns a non-nil error (specifically wrapping fetch.ErrNotFound) when
	// upstream has no paper at that date.
	FetchAndRender(ctx context.Context, src source.Source, daysAgo int) (pngPath string, fetchedAt time.Time, err error)
}

// CacheLookup finds the most recent cached image across all sources.
type CacheLookup interface {
	// MostRecent returns the most recently cached image across any source.
	// Returns ok=false if no cache exists yet.
	MostRecent() (sourceID, pngPath string, fetchedAt time.Time, ok bool)
}

// Picker drives rotation + fallback.
type Picker struct {
	Sources []source.Source
	Store   *cache.Store
	Fetcher Fetcher
	Lookup  CacheLookup
	Logger  *slog.Logger
}

// PickNext returns the next paper, advancing rotation. It will retry across
// sources and dates and ultimately fall back to the on-disk cache.
func (p *Picker) PickNext(ctx context.Context) (*Result, error) {
	if len(p.Sources) == 0 {
		return nil, fmt.Errorf("rotation: no sources configured")
	}
	if p.Logger == nil {
		p.Logger = slog.Default()
	}

	// Try every source once, starting from the current rotation index.
	startIdx := p.currentIndex()
	for attempt := 0; attempt < len(p.Sources); attempt++ {
		idx := (startIdx + attempt) % len(p.Sources)
		src := p.Sources[idx]

		pngPath, fetchedAt, daysOld, ok := p.tryDates(ctx, src)
		if ok {
			// Success — record health, advance rotation past this source.
			p.recordSuccess(src.ID, fetchedAt)
			p.advanceTo(idx + 1)
			return &Result{
				SourceID:  src.ID,
				PNGPath:   pngPath,
				FetchedAt: fetchedAt,
				DaysOld:   daysOld,
				Stale:     false,
			}, nil
		}

		// All dates failed for this source. Record health and continue.
		p.recordFailure(src.ID, "no paper at any of today/yesterday/2 days ago")
		p.Logger.Warn("source had no usable paper", "source", src.ID)
	}

	// Every source failed live. Fall back to the most recent cached image.
	if p.Lookup != nil {
		if sourceID, path, fetchedAt, ok := p.Lookup.MostRecent(); ok {
			p.Logger.Warn("serving stale cached image because no source had today's paper",
				"source", sourceID, "fetched_at", fetchedAt)
			return &Result{
				SourceID:  sourceID,
				PNGPath:   path,
				FetchedAt: fetchedAt,
				Stale:     true,
			}, nil
		}
	}

	return nil, ErrNoneAvailable
}

func (p *Picker) tryDates(ctx context.Context, src source.Source) (pngPath string, fetchedAt time.Time, daysOld int, ok bool) {
	for d := 0; d <= 2; d++ {
		path, ts, err := p.Fetcher.FetchAndRender(ctx, src, d)
		if err == nil {
			return path, ts, d, true
		}
		p.Logger.Debug("fetch attempt failed",
			"source", src.ID, "days_ago", d, "err", err)
	}
	return "", time.Time{}, 0, false
}

func (p *Picker) currentIndex() int {
	if p.Store == nil {
		return 0
	}
	return p.Store.Snapshot().Rotation.NextIndex % max(1, len(p.Sources))
}

func (p *Picker) advanceTo(idx int) {
	if p.Store == nil {
		return
	}
	_ = p.Store.Update(func(st *cache.State) {
		st.Rotation.NextIndex = idx % max(1, len(p.Sources))
	})
}

func (p *Picker) recordSuccess(sourceID string, when time.Time) {
	if p.Store == nil {
		return
	}
	_ = p.Store.Update(func(st *cache.State) {
		rec := st.Sources[sourceID]
		t := when
		rec.LastFetchOK = &t
		rec.LastErrorMsg = ""
		st.Sources[sourceID] = rec
	})
}

func (p *Picker) recordFailure(sourceID, msg string) {
	if p.Store == nil {
		return
	}
	now := time.Now().UTC()
	_ = p.Store.Update(func(st *cache.State) {
		rec := st.Sources[sourceID]
		rec.LastFetchError = &now
		rec.LastErrorMsg = msg
		st.Sources[sourceID] = rec
	})
}
