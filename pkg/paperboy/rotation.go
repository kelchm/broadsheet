package paperboy

// Stateless, time-driven rotation: which paper a rotation shows is a pure
// function of the wall clock and the spec — no cursor, no per-device state,
// no mutation on read. Every client asking at the same moment gets the same
// answer, so previews, prefetchers, proxies, and monitors are all harmless,
// and a restart changes nothing.
//
//	slot  = floor(now / interval) + phase        (or an explicit Slot)
//	index = slot mod len(sources)
//
// The slot boundary doubles as the refresh hint: the HTTP layer advertises
// seconds-to-next-change so battery clients can sleep precisely until the
// content actually changes.

import (
	"context"
	"errors"
	"time"
)

// ErrNoSourcesMatch is returned when a rotation's Sources filter matches
// nothing — a caller error, distinct from an empty archive.
var ErrNoSourcesMatch = errors.New("paperboy: no sources match the request")

// DefaultRotationInterval is the dwell per paper when a RotationSpec doesn't
// set one.
const DefaultRotationInterval = 30 * time.Minute

// RotationSpec describes a rotation. The zero value rotates all configured
// sources on the default interval.
type RotationSpec struct {
	// Sources restricts and orders the rotation (empty = all configured
	// sources in registry order). Unknown IDs are skipped.
	Sources []string

	// Interval is the dwell per paper. Default 30m.
	Interval time.Duration

	// Phase offsets the slot index, letting displays on the same playlist
	// deliberately show different papers (phase=1 is "one paper ahead").
	Phase int64

	// Slot, if non-nil, pins the absolute slot index instead of deriving it
	// from the clock. Used by display pages that address slots explicitly so
	// every fetch of a given slot URL yields the same paper regardless of
	// caches or clock skew.
	Slot *int64
}

// Rotation is a resolved rotation: which source the current (or given) slot
// selects, and when the selection next changes.
type Rotation struct {
	SourceID    string    // the selected source
	Slot        int64     // the absolute slot index that selected it
	NextChange  time.Time // the next slot boundary (clock-derived)
	Substituted bool      // the slot's source had nothing archived; the next source with content was served
}

func (s RotationSpec) interval() time.Duration {
	// Sub-second intervals would truncate to zero whole seconds (and divide by
	// zero); anything below 1s gets the default, same as unset. Sub-second
	// *components* truncate (90.5s behaves as 90s).
	if s.Interval < time.Second {
		return DefaultRotationInterval
	}
	return s.Interval
}

// ResolveRotation resolves a spec against the clock and the archive. If the
// selected source has nothing archived yet (cold-start fill-in), the rotation
// deterministically advances to the next source that does — every client
// makes the same substitution — and marks the result Substituted. With an
// entirely empty archive it returns ErrNoneAvailable.
func (p *Paperboy) ResolveRotation(spec RotationSpec) (Rotation, error) {
	srcs := p.getSources()
	if len(spec.Sources) > 0 {
		srcs = filterSources(srcs, spec.Sources)
	}
	if len(srcs) == 0 {
		return Rotation{}, ErrNoSourcesMatch
	}

	interval := spec.interval()
	secs := int64(interval / time.Second)
	now := p.now().UTC()
	base := now.Unix() / secs

	slot := base + spec.Phase
	if spec.Slot != nil {
		slot = *spec.Slot
	}
	// The boundary is clock-derived even for an explicit slot: it answers
	// "when does the rotation advance", not "when does this URL change".
	next := time.Unix((base+1)*secs, 0).UTC()

	n := int64(len(srcs))
	idx := int(((slot % n) + n) % n) // true modulo for negative slots/phases

	for i := range srcs {
		cand := srcs[(idx+i)%len(srcs)]
		if _, ok := p.archive.Newest(cand.ID); ok {
			return Rotation{
				SourceID:    cand.ID,
				Slot:        slot,
				NextChange:  next,
				Substituted: i > 0,
			}, nil
		}
	}
	return Rotation{}, ErrNoneAvailable
}

// NewestEdition reports the edition date of the newest archived edition for a
// source, without rendering anything.
func (p *Paperboy) NewestEdition(sourceID string) (time.Time, bool) {
	entry, ok := p.archive.Newest(sourceID)
	if !ok {
		return time.Time{}, false
	}
	return entry.Date, true
}

// RenderRotation resolves the rotation and renders the selected source's
// newest edition. It is a pure read: calling it any number of times changes
// nothing.
func (p *Paperboy) RenderRotation(ctx context.Context, spec RotationSpec, opts RenderOptions) (*Result, Rotation, error) {
	rot, err := p.ResolveRotation(spec)
	if err != nil {
		return nil, Rotation{}, err
	}
	entry, ok := p.archive.Newest(rot.SourceID)
	if !ok {
		// The source lost its content between resolve and render (prune race);
		// vanishingly rare and self-healing on retry.
		return nil, Rotation{}, ErrNoneAvailable
	}
	res, err := p.serve(ctx, entry, rot.Substituted, opts)
	if err != nil {
		return nil, Rotation{}, err
	}
	return res, rot, nil
}
