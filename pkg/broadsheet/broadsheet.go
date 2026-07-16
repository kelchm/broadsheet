// Package broadsheet is the public Go API for the broadsheet newspaper renderer.
//
// Most people will just run broadsheet-server or hit its HTTP endpoints. This
// package is for embedding the engine in another Go program — say, a custom
// TRMNL plugin or a Home Assistant integration.
//
// The engine is passive: it serves rendered front pages from a local archive.
// To keep that archive current, start the background reconciler explicitly with
// StartReconciler; embedders that only want to render existing editions can skip
// it.
//
// Basic usage:
//
//	p, err := broadsheet.New(broadsheet.Config{DataDir: "./data"})
//	if err != nil { ... }
//	p.StartReconciler(ctx)         // begin mirroring upstream in the background
//	res, err := p.RenderCurrent(ctx)
//	// res.Image is PNG bytes
package broadsheet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"image"
	"image/color"
	_ "image/png" // register PNG decoder for image.Decode
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/disintegration/imaging"
	"golang.org/x/sync/singleflight"

	"github.com/kelchm/broadsheet/internal/archive"
	"github.com/kelchm/broadsheet/internal/buildinfo"
	"github.com/kelchm/broadsheet/internal/cache"
	"github.com/kelchm/broadsheet/internal/catalog"
	"github.com/kelchm/broadsheet/internal/reconcile"
	"github.com/kelchm/broadsheet/internal/registry"
	"github.com/kelchm/broadsheet/internal/render"
	"github.com/kelchm/broadsheet/internal/source"
	"github.com/kelchm/broadsheet/internal/store"
)

// Source describes a newspaper feed. Alias of the internal canonical type.
type Source = source.Source

// CropHints carries per-source hints for the crop detector. Alias of the
// internal canonical type.
type CropHints = source.CropHints

// Version reports the broadsheet release version.
func Version() string { return buildinfo.Version }

// ErrNoneAvailable is returned when nothing has been archived yet (cold start).
var ErrNoneAvailable = errors.New("broadsheet: no editions available yet")

// ErrUnknownSource is returned when a source ID isn't configured — a caller
// error (HTTP 404-shaped), distinct from server-side render failures.
var ErrUnknownSource = errors.New("broadsheet: unknown source")

// ErrEditionNotFound is returned for a dated edition that is permanently
// absent (pruned, or never published) — 404-shaped, never retryable.
var ErrEditionNotFound = errors.New("broadsheet: edition not found")

const (
	defaultWidth       = 1600
	defaultPoll        = 30 * time.Minute
	defaultArchiveDays = 14
	defaultMarginPct   = 3.0

	// renderTimeout bounds a cache-fill render. Renders run detached from the
	// requesting context (see serve), so this is their only deadline.
	renderTimeout = 2 * time.Minute
)

// Cursors tracks each device's position in the rotation. RenderCurrent advances
// a device's cursor on every call — that's "rotate on each load."
//
// The default implementation is in-memory (see Config.Cursors). If devices
// become first-class, configurable entities, provide a persistent implementation
// (e.g. SQLite-backed) and nothing else in the engine changes.
type Cursors interface {
	// Next returns the source index to serve for device now (in [0,n)) and
	// advances the device's position by one. n is the number of sources in the
	// rotation for this call.
	Next(device string, n int) int
}

type memCursors struct {
	mu sync.Mutex
	m  map[string]int
}

func newMemCursors() *memCursors { return &memCursors{m: make(map[string]int)} }

func (c *memCursors) Next(device string, n int) int {
	if n <= 0 {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	i := c.m[device]
	c.m[device] = i + 1
	return i % n
}

// FitMode controls how the page is placed on a target canvas when both a width
// and height are given. Used by the PNG path for devices that consume a raw,
// exactly-sized image (no browser to do CSS); the HTML display page frames with
// CSS instead.
type FitMode string

const (
	// FitContain scales the page to fit inside the canvas, letterboxed with the
	// background color (a matted look — good for portrait panels).
	FitContain FitMode = "contain"
	// FitCover scales the page to fill the canvas, cropping the overflow.
	FitCover FitMode = "cover"
)

// Config holds runtime configuration for a Engine instance.
type Config struct {
	// DataDir is where the archive, render cache, and broadsheet.db live. Required.
	DataDir string

	// Width is the master render width in pixels (quality ceiling). Default 1600.
	Width int

	// PollInterval is the reconciler cadence. Default 30m.
	PollInterval time.Duration

	// ArchiveDays is how many days of editions to retain. Default 14.
	ArchiveDays int

	// Sources optionally bypasses the store entirely. When nil (the default),
	// the store is seeded from the embedded catalog and the user's enabled set
	// is loaded from it.
	Sources []Source

	// Cursors optionally overrides the per-device rotation store. Default is
	// in-memory (resets on restart); provide a persistent one to survive it.
	Cursors Cursors

	// Logger; if nil, slog.Default() is used.
	Logger *slog.Logger
}

// RenderOptions are per-call overrides.
//
// Framing (OutputHeight/Fit/MarginPct) is for the PNG path — devices that pull a
// raw, exactly-sized image and can't frame it themselves. The HTML display page
// ignores these and frames with CSS instead.
type RenderOptions struct {
	// OutputWidth / OutputHeight are the target canvas in pixels. If both are
	// set, the page is fit onto that exact canvas (see Fit). If only one is set,
	// the other follows the page's aspect ratio. The page is never upscaled past
	// its master resolution; a larger canvas just gets more background.
	OutputWidth  int
	OutputHeight int

	// Fit applies only when both dimensions are set. Default FitContain.
	Fit FitMode

	// MarginPct is the background border as a percent of the canvas's shorter
	// side. 0 means the default (defaultMarginPct); a negative value means no
	// margin.
	MarginPct float64

	// Sources and Device are the rotation policy for RenderCurrent (ignored by
	// RenderFor). Sources restricts and orders the rotation to a subset of source
	// IDs (empty = all). Device identifies the requester: each device advances
	// its own cursor on every call ("rotate on each load"). Empty is a valid
	// key (the default device); the server fills it from ?device= or client IP.
	Sources []string
	Device  string
}

// Result is what a render call returns.
type Result struct {
	Image     []byte    // rendered PNG bytes
	SourceID  string    // which source produced the image
	FetchedAt time.Time // edition date
	Stale     bool      // true if served as a cross-source fallback
	DaysOld   int       // 0 for today's edition, 1 for yesterday's, etc.
	Width     int       // actual pixel width of Image
	Height    int       // actual pixel height of Image
	ETag      string    // strong validator over (source, edition, artifact mtime, render params), pre-quoted for HTTP
}

// Health describes the per-source health of the engine.
type Health struct {
	Sources map[string]SourceHealth
}

// SourceHealth is the per-source health record. LastPollOK proves upstream
// reachability (clean poll, even all-304); LastFetchOK is when an edition
// last stored.
type SourceHealth struct {
	LastPollOK     *time.Time
	LastFetchOK    *time.Time
	LastFetchError *time.Time
	LastError      string
}

// Engine is the engine. Construct one with New. Safe for concurrent use.
type Engine struct {
	cfg        Config
	archive    *archive.Store
	renderer   *render.Renderer
	store      *store.Store
	reconciler *reconcile.Reconciler
	cacheDir   string
	cursors    Cursors
	now        func() time.Time

	// The active source set. Mutable at runtime (enable/disable via the API
	// reloads it from the store), so all readers take the lock and treat the
	// returned slice as an immutable snapshot. reloadMu serializes the whole
	// store-mutate-then-reload sequence so concurrent PATCHes can't leave the
	// live set missing a committed change.
	srcMu    sync.RWMutex
	reloadMu sync.Mutex
	sources  []source.Source

	// renderSF collapses concurrent cold-cache renders of the same PNG into a
	// single rasterization — each in-flight render can hold hundreds of MB.
	renderSF singleflight.Group
	// renderSem bounds rasterizations GLOBALLY: singleflight only collapses
	// same-edition requests, and a page fanning out many different cold
	// editions (the archive grid) would otherwise run them all at once and
	// OOM small hosts. Renders queue instead.
	renderSem chan struct{}
	// composeSem bounds concurrent master decodes (~20MB of pixels each) —
	// kept tight so composes stacked on a running rasterization fit small hosts.
	composeSem chan struct{}
	// variants memoizes small rendered outputs by content identity (the ETag),
	// so thumbnail-heavy pages stop re-decoding masters on every image.
	variants *variantCache
}

// New constructs a Engine with the given config.
func New(cfg Config) (*Engine, error) {
	if cfg.DataDir == "" {
		return nil, fmt.Errorf("broadsheet: DataDir is required")
	}
	if cfg.Width == 0 {
		cfg.Width = defaultWidth
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPoll
	}
	if cfg.ArchiveDays <= 0 {
		cfg.ArchiveDays = defaultArchiveDays
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	cursors := cfg.Cursors
	if cursors == nil {
		cursors = newMemCursors()
	}

	archiveDir := filepath.Join(cfg.DataDir, "archive")
	cacheDir := filepath.Join(cfg.DataDir, "cache")
	for _, d := range []string{archiveDir, cacheDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, fmt.Errorf("broadsheet: create %s: %w", d, err)
		}
	}

	// A pre-rename deployment's database moves over automatically — once,
	// before the store opens it (WAL sidecars included).
	dbPath := filepath.Join(cfg.DataDir, "broadsheet.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		if _, err := os.Stat(filepath.Join(cfg.DataDir, "paperboy.db")); err == nil {
			for _, ext := range []string{"", "-wal", "-shm"} {
				old := filepath.Join(cfg.DataDir, "paperboy.db"+ext)
				if _, err := os.Stat(old); err == nil {
					_ = os.Rename(old, dbPath+ext)
				}
			}
			cfg.Logger.Info("renamed legacy paperboy.db to broadsheet.db")
		}
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("broadsheet: open store: %w", err)
	}
	importLegacyState(st, filepath.Join(cfg.DataDir, "state.json"), cfg.Logger)

	// Build the archive before reconciling sources: loadSources stamps archive
	// labels before it prunes, so a paper dropped in this release keeps its
	// history labeled (and the archive stays self-describing).
	arch := &archive.Store{Root: archiveDir}

	srcs := cfg.Sources
	if srcs == nil {
		srcs, err = loadSources(st, arch, cfg.Logger)
		if err != nil {
			_ = st.Close()
			return nil, err
		}
	} else {
		// Embedder mode has no store to reconcile, but its archives should still
		// be self-describing — label them from the configured sources. There's no
		// prune here, so a label write failure is non-fatal; just note it.
		names := make(map[string]string, len(srcs))
		for _, s := range srcs {
			names[s.ID] = s.DisplayName
		}
		if err := stampArchiveLabels(arch, names); err != nil && cfg.Logger != nil {
			cfg.Logger.Warn("archive label stamping failed", "err", err)
		}
	}

	p := &Engine{
		cfg:        cfg,
		sources:    srcs,
		archive:    arch,
		renderer:   render.New(),
		store:      st,
		cacheDir:   cacheDir,
		cursors:    cursors,
		now:        time.Now,
		renderSem:  make(chan struct{}, 1),
		composeSem: make(chan struct{}, 2),
		variants:   newVariantCache(128, 256<<10),
	}
	p.reconciler = &reconcile.Reconciler{
		SourcesFn: p.getSources, // live view: enable/disable applies next cycle
		Archive:   arch,
		Store:     st,
		Deps:      source.Deps{HTTP: &http.Client{Timeout: 30 * time.Second}, Logger: cfg.Logger},
		Retention: time.Duration(cfg.ArchiveDays) * 24 * time.Hour,
		Interval:  cfg.PollInterval,
		Logger:    cfg.Logger,
		CacheDir:  cacheDir,
	}
	return p, nil
}

// stampArchiveLabels writes each source's display name into its archive metadata
// so history stays labeled — and the archive stays self-describing and portable —
// after a paper leaves the catalog and its store row is pruned. It only touches
// ids that already have an archive directory (SetName is a no-op for a blank name
// and rewrites only when the name changed), so it never creates directories and
// never clobbers a transplanted archive that already carries its own label.
// stampArchiveLabels writes each source's display name into its archive metadata
// so history stays labeled — and the archive stays self-describing and portable —
// after a paper leaves the catalog and its store row is pruned. It only touches
// ids that already have an archive directory (SetName never creates one), and
// SetName rewrites only when the name changed. An id absent from names (a foreign
// paper transplanted into the archive, not in the catalog or store) is skipped,
// so its own label is left untouched; a known id is (re)labeled from names, which
// is the intended refresh. It returns the first write error so the caller can
// avoid a destructive prune when a soon-to-be-dropped paper's name wasn't saved.
func stampArchiveLabels(arch *archive.Store, names map[string]string) error {
	for _, id := range arch.SourceIDs() {
		if name := names[id]; name != "" {
			if err := arch.SetName(id, name); err != nil {
				return fmt.Errorf("broadsheet: stamp archive label %q: %w", id, err)
			}
		}
	}
	return nil
}

// getSources returns the active source set (an immutable snapshot).
func (p *Engine) getSources() []source.Source {
	p.srcMu.RLock()
	defer p.srcMu.RUnlock()
	return p.sources
}

// ReloadSources re-reads the enabled set from the store and swaps it in. A
// no-op for engines constructed with explicit Config.Sources. An empty enabled
// set is legal (the user can disable everything; devices get 4xx/503 until
// something is enabled again).
func (p *Engine) ReloadSources() error {
	if p.cfg.Sources != nil {
		return nil
	}
	p.reloadMu.Lock()
	defer p.reloadMu.Unlock()
	return p.reloadLocked()
}

func (p *Engine) reloadLocked() error {
	srcs, err := loadEnabled(p.store, p.cfg.Logger)
	if err != nil {
		return err
	}
	p.srcMu.Lock()
	p.sources = srcs
	p.srcMu.Unlock()
	return nil
}

// SetSourceEnabled flips a catalog source in or out of the enabled set and
// applies it immediately (rotation next request, polling next cycle).
func (p *Engine) SetSourceEnabled(sourceID string, enabled bool) error {
	p.reloadMu.Lock()
	defer p.reloadMu.Unlock()
	if err := p.store.SetSourceEnabled(sourceID, enabled); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: %q", ErrUnknownSource, sourceID)
		}
		return fmt.Errorf("broadsheet: set enabled: %w", err)
	}
	if p.cfg.Sources != nil {
		return nil
	}
	return p.reloadLocked()
}

// CatalogEntry is one catalog paper with its enabled state, for browsing.
type CatalogEntry struct {
	ID       string
	Name     string
	Location string
	Enabled  bool
}

// Catalog returns every known paper (the full store catalog) with enabled
// flags, in position order.
func (p *Engine) Catalog() ([]CatalogEntry, error) {
	rows, err := p.store.ListSources(false)
	if err != nil {
		return nil, fmt.Errorf("broadsheet: catalog: %w", err)
	}
	loc := map[string]string{}
	if entries, err := catalog.All(); err == nil {
		for _, e := range entries {
			loc[e.ID] = e.Location
		}
	}
	out := make([]CatalogEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, CatalogEntry{
			ID: r.ID, Name: r.DisplayName, Location: loc[r.ID], Enabled: r.Enabled,
		})
	}
	return out, nil
}

// ArchiveName returns the display name captured alongside a source's archive
// while it was archiving, or "" if none. It lets the archive browser label a
// paper that has left the catalog — whose live catalog name is gone — with the
// name its history was collected under, instead of a bare id. The archive is
// self-describing: identity travels with the data, not the catalog row.
func (p *Engine) ArchiveName(id string) string {
	return p.archive.Name(id)
}

// ArchiveIndex returns edition dates (oldest first, same-day duplicates
// collapsed) for every source holding at least one archived edition — including
// disabled papers and papers dropped from the catalog entirely, whose collected
// history stays browsable until it ages out on the normal retention. Catalog
// membership governs polling and the catalog UI; the archive is independent data
// with its own age-based lifetime, so a dropped paper's front pages remain here
// (and renderable, via knownSource) rather than vanishing the moment it's removed.
func (p *Engine) ArchiveIndex() map[string][]time.Time {
	out := map[string][]time.Time{}
	for _, id := range p.archive.SourceIDs() {
		var dates []time.Time
		var last time.Time
		for _, entry := range p.archive.List(id) {
			if !entry.Date.Equal(last) {
				dates = append(dates, entry.Date)
				last = entry.Date
			}
		}
		if len(dates) > 0 {
			out[id] = dates
		}
	}
	return out
}

// ListEditions returns the archived edition dates for a source, oldest first.
// Disabled catalog papers remain addressable — their history outlives the
// toggle. Same-day duplicates (media-type changes on re-post) are collapsed.
func (p *Engine) ListEditions(sourceID string) ([]time.Time, error) {
	if !p.knownSource(sourceID) {
		return nil, fmt.Errorf("%w: %q", ErrUnknownSource, sourceID)
	}
	entries := p.archive.List(sourceID)
	out := make([]time.Time, 0, len(entries))
	var last time.Time
	for _, e := range entries {
		if !e.Date.Equal(last) {
			out = append(out, e.Date)
			last = e.Date
		}
	}
	return out, nil
}

// RenderEdition renders a specific archived edition of a source.
func (p *Engine) RenderEdition(ctx context.Context, sourceID string, date time.Time, opts ...RenderOptions) (*Result, error) {
	if !p.knownSource(sourceID) {
		return nil, fmt.Errorf("%w: %q", ErrUnknownSource, sourceID)
	}
	entry, ok := p.archive.Get(sourceID, date)
	if !ok {
		// Permanent, not retryable: the date was pruned or never published.
		return nil, fmt.Errorf("%w: %s has no edition for %s", ErrEditionNotFound, sourceID, date.UTC().Format("20060102"))
	}
	return p.serve(ctx, entry, false, optsOrDefault(opts))
}

// loadSources seeds the store from the embedded catalog, then loads the
// enabled set. Seeding runs every boot and fully reconciles the store to the
// catalog (see store.SeedSources): papers added to the catalog appear, dropped
// papers are pruned, and existing papers' wiring is refreshed — all while the
// user's enabled toggles are preserved. Sources live as rows — provider type +
// JSON config — decoded into typed providers here; a row that fails to decode is
// skipped with a warning rather than taking the whole engine down.
func loadSources(st *store.Store, arch *archive.Store, logger *slog.Logger) ([]source.Source, error) {
	entries, err := catalog.All()
	if err != nil {
		return nil, fmt.Errorf("broadsheet: load catalog: %w", err)
	}

	// Stamp archive labels BEFORE reconciling. Names come from the current store
	// rows (which still hold the name of a paper about to be pruned) overlaid with
	// the catalog (fresh names win for papers that survive). Doing this ahead of
	// SeedSources' prune is what lets a paper dropped in *this* release keep its
	// history labeled; disabled papers, which never re-archive, are covered the
	// same way. The reconciler keeps active papers' labels fresh on each Put.
	names := map[string]string{}
	stored, err := st.ListSources(false)
	if err != nil {
		return nil, fmt.Errorf("broadsheet: list sources for archive labeling: %w", err)
	}
	for _, r := range stored {
		names[r.ID] = r.DisplayName
	}
	for _, e := range entries {
		names[e.ID] = e.Name
	}
	// If a label write fails, a paper about to be dropped this boot might lose the
	// name it needs to keep its archived history readable — so skip the prune this
	// boot rather than delete a row whose name we couldn't preserve. The upsert
	// still runs; the prune retries on the next boot once the write succeeds.
	prune := true
	if err := stampArchiveLabels(arch, names); err != nil {
		if logger != nil {
			logger.Warn("archive label stamping failed; skipping catalog prune this boot", "err", err)
		}
		prune = false
	}

	rows := make([]store.SourceRow, 0, len(entries))
	for i, e := range entries {
		rows = append(rows, store.SourceRow{
			ID: e.ID, DisplayName: e.Name,
			ProviderType: e.Provider, ProviderConfig: e.Config,
			Enabled: e.Default, Position: i,
		})
	}
	if err := st.SeedSources(rows, prune); err != nil {
		return nil, fmt.Errorf("broadsheet: seed sources: %w", err)
	}
	return loadEnabled(st, logger)
}

// loadEnabled reads and decodes the enabled set. Zero enabled rows is a legal,
// user-reachable state (everything toggled off); the error case is enabled
// rows existing but NONE decoding — that's data this build can't understand.
func loadEnabled(st *store.Store, logger *slog.Logger) ([]source.Source, error) {
	stored, err := st.ListSources(true)
	if err != nil {
		return nil, fmt.Errorf("broadsheet: list sources: %w", err)
	}
	out := make([]source.Source, 0, len(stored))
	for _, r := range stored {
		s, err := registry.Build(r.ID, r.DisplayName, r.ProviderType, r.ProviderConfig)
		if err != nil {
			logger.Warn("skipping undecodable source", "id", r.ID, "err", err)
			continue
		}
		out = append(out, s)
	}
	if len(out) == 0 && len(stored) > 0 {
		return nil, fmt.Errorf("broadsheet: none of the %d enabled sources decoded (version skew? check the warnings above)", len(stored))
	}
	return out, nil
}

// knownSource reports whether an ID is addressable for reads — in the live set,
// anywhere in the catalog (for store-backed engines), or holding archived
// editions on disk. Editions/refresh endpoints must address disabled papers too
// (the API advertises the full catalog), and archived history outlives catalog
// membership: a paper toggled off — or dropped from the catalog entirely — stays
// readable and renderable until its editions age out on the normal retention.
// Catalog membership and the archive are independent lifetimes.
func (p *Engine) knownSource(id string) bool {
	if source.ByID(p.getSources(), id) != nil {
		return true
	}
	// Archived history keeps a paper addressable even with no catalog row left.
	if _, ok := p.archive.Newest(id); ok {
		return true
	}
	if p.cfg.Sources != nil {
		return false
	}
	rows, err := p.store.ListSources(false)
	if err != nil {
		return false
	}
	for _, r := range rows {
		if r.ID == id {
			return true
		}
	}
	return false
}

// importLegacyState migrates a pre-SQLite state.json (provider ETags +
// per-source health) into the store, then sets the file aside so the import
// runs exactly once. Best-effort: its contents are re-derivable, so failures
// warn and move on.
func importLegacyState(st *store.Store, path string, logger *slog.Logger) {
	if _, err := os.Stat(path); err != nil {
		return
	}
	legacy, err := cache.Open(path)
	if err != nil {
		logger.Warn("legacy state.json unreadable; skipping import", "err", err)
		return
	}
	snap := legacy.Snapshot()
	for id, vers := range snap.Versions {
		// Never overwrite tokens the store already has: if a previous import's
		// rename failed, the reconciler may have persisted newer ETags since.
		if len(st.Versions(id)) > 0 {
			continue
		}
		if err := st.SetVersions(id, vers); err != nil {
			logger.Warn("import legacy versions failed", "source", id, "err", err)
		}
	}
	for id, rec := range snap.Sources {
		if rec.LastFetchOK != nil {
			_ = st.RecordSuccess(id, *rec.LastFetchOK)
		}
		if rec.LastFetchError != nil {
			_ = st.RecordFailure(id, rec.LastErrorMsg, *rec.LastFetchError)
		}
	}
	// A corrupt legacy file was already set aside as .corrupt by cache.Open, so
	// the source file may be gone — that's fine, not a failed rename.
	if err := os.Rename(path, path+".imported"); err != nil && !os.IsNotExist(err) {
		logger.Warn("could not set aside legacy state.json; it will re-import next boot", "err", err)
		return
	}
	logger.Info("imported legacy state.json into the store",
		"sources", len(snap.Sources), "versions", len(snap.Versions))
}

// Close releases the engine's persistent resources (the store's database
// handle). Call it when done with an engine constructed by New; the server
// does so on shutdown, and embedders constructing engines dynamically must
// too. Render/read calls must not race Close.
func (p *Engine) Close() error {
	return p.store.Close()
}

// StartReconciler launches the background mirror loop in its own goroutine. It
// reconciles immediately, then every PollInterval, until ctx is canceled.
func (p *Engine) StartReconciler(ctx context.Context) {
	go p.reconciler.Run(ctx)
}

// Poll runs a single reconcile pass across all sources synchronously.
func (p *Engine) Poll(ctx context.Context) {
	p.reconciler.ReconcileOnce(ctx)
}

// Refresh polls a single source synchronously and archives any new edition.
func (p *Engine) Refresh(ctx context.Context, sourceID string) error {
	src := source.ByID(p.getSources(), sourceID)
	if src == nil && p.cfg.Sources == nil {
		// Disabled catalog papers are refreshable too (fetch-then-preview
		// without enabling into every display's rotation).
		if rows, err := p.store.ListSources(false); err == nil {
			for _, r := range rows {
				if r.ID != sourceID {
					continue
				}
				if s, err := registry.Build(r.ID, r.DisplayName, r.ProviderType, r.ProviderConfig); err == nil {
					src = &s
				}
				break
			}
		}
	}
	if src == nil {
		return fmt.Errorf("%w: %q", ErrUnknownSource, sourceID)
	}
	p.reconciler.ReconcileSource(ctx, *src, p.now().UTC())
	return nil
}

// RenderCurrent returns the current paper for the requesting device and advances
// that device's rotation cursor by one — so each load moves to the next paper.
// The device's own refresh cadence sets the pace; there is no server-side rotate
// clock. Falls back to the newest archived edition of any source if the selected
// source has none yet.
func (p *Engine) RenderCurrent(ctx context.Context, opts ...RenderOptions) (*Result, error) {
	o := optsOrDefault(opts)
	srcs := p.getSources()
	if len(o.Sources) > 0 {
		srcs = filterSources(srcs, o.Sources)
	}
	if len(srcs) == 0 {
		return nil, ErrNoSourcesMatch
	}

	src := srcs[p.cursors.Next(o.Device, len(srcs))]
	if entry, ok := p.archive.Newest(src.ID); ok {
		return p.serve(ctx, entry, false, o)
	}
	if entry, ok := p.archive.NewestAny(); ok {
		return p.serve(ctx, entry, true, o)
	}
	return nil, ErrNoneAvailable
}

// RenderFor returns the newest archived edition for a specific source.
func (p *Engine) RenderFor(ctx context.Context, sourceID string, opts ...RenderOptions) (*Result, error) {
	if source.ByID(p.getSources(), sourceID) == nil {
		return nil, fmt.Errorf("%w: %q", ErrUnknownSource, sourceID)
	}
	entry, ok := p.archive.Newest(sourceID)
	if !ok {
		// Retryable, not a caller error: the reconciler simply hasn't filled
		// this source yet.
		return nil, fmt.Errorf("%w: no archived edition for %s", ErrNoneAvailable, sourceID)
	}
	return p.serve(ctx, entry, false, optsOrDefault(opts))
}

func (p *Engine) serve(ctx context.Context, entry archive.Entry, stale bool, opts RenderOptions) (*Result, error) {
	master := WidthMaster(p.cfg)
	// The cache key carries the master width so a BROADSHEET_WIDTH change never
	// serves old-width masters; freshness against the archived artifact handles
	// re-posted (corrected) editions.
	pngPath := filepath.Join(p.cacheDir, entry.SourceID,
		fmt.Sprintf("%s.w%d.png", entry.Date.UTC().Format("20060102"), master))

	// Render lazily into the disposable cache on first view; re-render when the
	// archive has a newer artifact. singleflight collapses concurrent cold-cache
	// requests for the same PNG into one rasterization.
	if !renderCacheFresh(pngPath, entry.Path) {
		if _, err, _ := p.renderSF.Do(pngPath, func() (any, error) {
			if renderCacheFresh(pngPath, entry.Path) {
				return nil, nil // a concurrent caller already rendered it
			}
			// Render detached from the requesting context: the flight's result
			// is shared by every concurrent waiter (and by the cache), so the
			// winner disconnecting must not cancel the render out from under
			// the rest. renderTimeout bounds both the queue wait and the render.
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), renderTimeout)
			defer cancel()
			// Queue for the global render slot — see renderSem.
			select {
			case p.renderSem <- struct{}{}:
				defer func() { <-p.renderSem }()
			case <-rctx.Done():
				return nil, fmt.Errorf("render queue: %w", rctx.Err())
			}
			// Stat the artifact BEFORE rendering and stamp its mtime onto the
			// finished PNG: if the reconciler overwrites the artifact while
			// this render is in flight, the PNG (stamped with the old mtime)
			// stays older than the new artifact and re-renders next request —
			// otherwise the finished PNG's own mtime would mask the update.
			srcInfo, statErr := os.Stat(entry.Path)
			if err := p.renderer.Render(rctx, entry.Path, entry.Media, pngPath, master); err != nil {
				return nil, err
			}
			if statErr == nil {
				_ = os.Chtimes(pngPath, srcInfo.ModTime(), srcInfo.ModTime())
			}
			return nil, nil
		}); err != nil {
			return nil, fmt.Errorf("broadsheet: render: %w", err)
		}
	}

	// Stat before read: the ETag hashes the mtime of the exact bytes served.
	// The cached PNG is mtime-stamped to the artifact it was rendered from, so
	// this stays coherent even if the reconciler overwrites the artifact while
	// this request is in flight (stat-ing the artifact here instead could pin
	// an old body under a new ETag behind 304s).
	pngInfo, err := os.Stat(pngPath)
	if err != nil {
		return nil, fmt.Errorf("broadsheet: stat render: %w", err)
	}

	daysOld := int(p.now().UTC().Sub(entry.Date).Hours()) / 24
	if daysOld < 0 {
		daysOld = 0
	}

	// The ETag is the full content identity (edition + render mtime + build +
	// params), so it doubles as the variant-cache key: a thumbnail-heavy page
	// hits here instead of re-decoding the master per image.
	etag := contentETag(entry, pngInfo.ModTime(), master, opts)
	if cached, ok := p.variants.get(etag); ok {
		out := *cached
		out.Stale = stale
		out.DaysOld = daysOld
		return &out, nil
	}

	// Bound concurrent master decodes: each holds ~20MB of pixels, and a grid
	// page fans out many at once.
	select {
	case p.composeSem <- struct{}{}:
		defer func() { <-p.composeSem }()
	case <-ctx.Done():
		return nil, fmt.Errorf("broadsheet: %w", ctx.Err())
	}

	data, err := os.ReadFile(pngPath) //nolint:gosec // G304: internal cache path from a validated source entry, not user input
	if err != nil {
		return nil, fmt.Errorf("broadsheet: read render: %w", err)
	}
	page, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("broadsheet: decode render: %w", err)
	}

	out := compose(page, opts, master)
	var buf bytes.Buffer
	if err := imaging.Encode(&buf, out, imaging.PNG); err != nil {
		return nil, fmt.Errorf("broadsheet: encode: %w", err)
	}

	res := &Result{
		Image:     buf.Bytes(),
		SourceID:  entry.SourceID,
		FetchedAt: entry.Date,
		Stale:     stale,
		DaysOld:   daysOld,
		Width:     out.Bounds().Dx(),
		Height:    out.Bounds().Dy(),
		ETag:      etag,
	}
	p.variants.put(etag, res)
	return res, nil
}

// variantCache is a small bounded FIFO memo of rendered outputs, keyed by
// ETag (which already encodes edition, render mtime, build, and params, so
// entries can never serve stale content — a change mints a new key). Only
// small outputs are kept: it exists for thumbnails, not full pages.
type variantCache struct {
	mu       sync.Mutex
	entries  map[string]*Result
	order    []string
	capacity int
	maxBytes int
}

func newVariantCache(capacity, maxBytes int) *variantCache {
	return &variantCache{
		entries: make(map[string]*Result, capacity), capacity: capacity, maxBytes: maxBytes,
	}
}

func (c *variantCache) get(key string) (*Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.entries[key]
	return r, ok
}

func (c *variantCache) put(key string, r *Result) {
	if len(r.Image) > c.maxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[key]; exists {
		return
	}
	for len(c.entries) >= c.capacity {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.entries, oldest)
	}
	stored := *r // callers get copies with Stale/DaysOld patched; keep a canonical value
	c.entries[key] = &stored
	c.order = append(c.order, key)
}

// contentETag builds a strong validator over everything that determines the
// response bytes: the edition identity (source + date + the served render's
// mtime, which is stamped from the artifact it was rendered from — a corrected
// edition changes it), the build version (render-code changes must invalidate
// client caches), the master width, and the framing parameters. Pre-quoted for
// direct use as an HTTP ETag.
func contentETag(entry archive.Entry, renderMtime time.Time, master int, opts RenderOptions) string {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%s|%s|%d|%s|%d|%d|%d|%s|%g",
		entry.SourceID, entry.Date.UTC().Format("20060102"), renderMtime.UnixNano(),
		buildinfo.Version,
		master, opts.OutputWidth, opts.OutputHeight, opts.Fit, opts.MarginPct)
	return fmt.Sprintf("%q", strconv.FormatUint(h.Sum64(), 16))
}

// filterSources returns the sources whose IDs appear in ids, in the order ids
// lists them (so the caller controls the rotation sequence). Unknown IDs are
// skipped.
func filterSources(all []source.Source, ids []string) []source.Source {
	out := make([]source.Source, 0, len(ids))
	for _, id := range ids {
		if s := source.ByID(all, id); s != nil {
			out = append(out, *s)
		}
	}
	return out
}

// compose frames the page for a device that consumes a raw PNG: it fits the page
// onto the requested canvas (or the page's own aspect if only one dimension is
// given), on a white background with a margin so content never touches the edge.
// The page is never upscaled past its master resolution. The HTML display page
// does this in CSS instead and leaves OutputHeight/Fit/MarginPct unset.
func compose(page image.Image, o RenderOptions, master int) image.Image {
	white := color.NRGBA{R: 255, G: 255, B: 255, A: 255}
	pageH := page.Bounds().Dy()

	mp := o.MarginPct
	switch {
	case mp == 0:
		mp = defaultMarginPct
	case mp < 0:
		mp = 0
	}
	marginPx := func(basis int) int { return int(math.Round(mp / 100 * float64(basis))) }

	w, h := o.OutputWidth, o.OutputHeight
	var result image.Image
	switch {
	case w > 0 && h > 0:
		m := marginPx(min(w, h))
		boxW, boxH := max(1, w-2*m), max(1, h-2*m)
		if o.Fit == FitCover {
			pw, ph := page.Bounds().Dx(), page.Bounds().Dy()
			if boxW > pw || boxH > ph {
				// Filling would upscale past the master ("never upscaled" is a
				// documented contract); crop the native-resolution center
				// region instead, leaving background where the page runs out.
				result = imaging.PasteCenter(imaging.New(w, h, white),
					imaging.CropCenter(page, min(boxW, pw), min(boxH, ph)))
			} else {
				result = imaging.PasteCenter(imaging.New(w, h, white),
					imaging.Fill(page, boxW, boxH, imaging.Center, imaging.Lanczos))
			}
		} else {
			result = imaging.PasteCenter(imaging.New(w, h, white),
				imaging.Fit(page, boxW, boxH, imaging.Lanczos)) // contain, never upscales
		}
	case w > 0:
		m := marginPx(w)
		content := imaging.Resize(page, min(max(1, w-2*m), master), 0, imaging.Lanczos)
		result = imaging.PasteCenter(imaging.New(w, content.Bounds().Dy()+2*m, white), content)
	case h > 0:
		m := marginPx(h)
		content := imaging.Resize(page, 0, min(max(1, h-2*m), pageH), imaging.Lanczos)
		result = imaging.PasteCenter(imaging.New(content.Bounds().Dx()+2*m, h, white), content)
	default:
		m := marginPx(master)
		if m == 0 {
			return page
		}
		result = imaging.PasteCenter(imaging.New(page.Bounds().Dx()+2*m, pageH+2*m, white), page)
	}
	return imaging.Grayscale(result)
}

func optsOrDefault(opts []RenderOptions) RenderOptions {
	if len(opts) > 0 {
		return opts[0]
	}
	return RenderOptions{}
}

// ListSources returns the configured sources.
func (p *Engine) ListSources() []Source {
	srcs := p.getSources()
	out := make([]Source, len(srcs))
	copy(out, srcs)
	return out
}

// Ready reports whether at least one usable edition has been archived.
func (p *Engine) Ready() bool {
	_, ok := p.archive.NewestAny()
	return ok
}

// HealthSnapshot returns the current per-source health.
func (p *Engine) HealthSnapshot() Health {
	snap, err := p.store.HealthSnapshot()
	if err != nil {
		p.cfg.Logger.Warn("health snapshot failed", "err", err)
		return Health{Sources: map[string]SourceHealth{}}
	}
	out := Health{Sources: make(map[string]SourceHealth, len(snap))}
	for id, rec := range snap {
		out.Sources[id] = SourceHealth{
			LastPollOK:     rec.LastPollOK,
			LastFetchOK:    rec.LastFetchOK,
			LastFetchError: rec.LastFetchError,
			LastError:      rec.LastErrorMsg,
		}
	}
	return out
}

// WidthMaster returns the master (cache) width for a Config, applying the
// default if unset.
func WidthMaster(c Config) int {
	if c.Width <= 0 {
		return defaultWidth
	}
	return c.Width
}

// renderCacheFresh reports whether the cached PNG exists, is non-empty, and is
// at least as new as the archived artifact it was rendered from. An archive
// overwrite (a re-posted, corrected edition) makes the artifact newer than the
// PNG and so invalidates it.
func renderCacheFresh(pngPath, srcPath string) bool {
	png, err := os.Stat(pngPath)
	if err != nil || png.Size() == 0 {
		return false
	}
	src, err := os.Stat(srcPath)
	if err != nil {
		// Artifact unreadable (e.g. pruned between listing and here): a
		// re-render would fail anyway, so keep serving the cached PNG.
		return true
	}
	return !png.ModTime().Before(src.ModTime())
}
