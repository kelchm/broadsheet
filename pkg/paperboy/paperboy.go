// Package paperboy is the public Go API for the paperboy newspaper renderer.
//
// Most people will just run paperboy-server or hit its HTTP endpoints. This
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
//	p, err := paperboy.New(paperboy.Config{DataDir: "./data"})
//	if err != nil { ... }
//	p.StartReconciler(ctx)         // begin mirroring upstream in the background
//	res, err := p.RenderCurrent(ctx)
//	// res.Image is PNG bytes
package paperboy

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

	"github.com/kelchm/paperboy/internal/archive"
	"github.com/kelchm/paperboy/internal/buildinfo"
	"github.com/kelchm/paperboy/internal/cache"
	"github.com/kelchm/paperboy/internal/catalog"
	"github.com/kelchm/paperboy/internal/reconcile"
	"github.com/kelchm/paperboy/internal/registry"
	"github.com/kelchm/paperboy/internal/render"
	"github.com/kelchm/paperboy/internal/source"
	"github.com/kelchm/paperboy/internal/store"
)

// Source describes a newspaper feed. Alias of the internal canonical type.
type Source = source.Source

// CropHints carries per-source hints for the crop detector. Alias of the
// internal canonical type.
type CropHints = source.CropHints

// Version reports the paperboy release version.
func Version() string { return buildinfo.Version }

// ErrNoneAvailable is returned when nothing has been archived yet (cold start).
var ErrNoneAvailable = errors.New("paperboy: no editions available yet")

// ErrUnknownSource is returned when a source ID isn't configured — a caller
// error (HTTP 404-shaped), distinct from server-side render failures.
var ErrUnknownSource = errors.New("paperboy: unknown source")

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

// Config holds runtime configuration for a Paperboy instance.
type Config struct {
	// DataDir is where the archive, render cache, and paperboy.db live. Required.
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

// SourceHealth is the per-source health record.
type SourceHealth struct {
	LastFetchOK    *time.Time
	LastFetchError *time.Time
	LastError      string
}

// Paperboy is the engine. Construct one with New. Safe for concurrent use.
type Paperboy struct {
	cfg        Config
	sources    []source.Source
	archive    *archive.Store
	renderer   *render.Renderer
	store      *store.Store
	reconciler *reconcile.Reconciler
	cacheDir   string
	cursors    Cursors
	now        func() time.Time

	// renderSF collapses concurrent cold-cache renders of the same PNG into a
	// single rasterization — each in-flight render can hold hundreds of MB.
	renderSF singleflight.Group
}

// New constructs a Paperboy with the given config.
func New(cfg Config) (*Paperboy, error) {
	if cfg.DataDir == "" {
		return nil, fmt.Errorf("paperboy: DataDir is required")
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
			return nil, fmt.Errorf("paperboy: create %s: %w", d, err)
		}
	}

	st, err := store.Open(filepath.Join(cfg.DataDir, "paperboy.db"))
	if err != nil {
		return nil, fmt.Errorf("paperboy: open store: %w", err)
	}
	importLegacyState(st, filepath.Join(cfg.DataDir, "state.json"), cfg.Logger)

	srcs := cfg.Sources
	if srcs == nil {
		srcs, err = loadSources(st, cfg.Logger)
		if err != nil {
			_ = st.Close()
			return nil, err
		}
	}

	arch := &archive.Store{Root: archiveDir}
	rec := &reconcile.Reconciler{
		Sources:   srcs,
		Archive:   arch,
		Store:     st,
		Deps:      source.Deps{HTTP: &http.Client{Timeout: 30 * time.Second}, Logger: cfg.Logger},
		Retention: time.Duration(cfg.ArchiveDays) * 24 * time.Hour,
		Interval:  cfg.PollInterval,
		Logger:    cfg.Logger,
		CacheDir:  cacheDir,
	}

	return &Paperboy{
		cfg:        cfg,
		sources:    srcs,
		archive:    arch,
		renderer:   render.New(),
		store:      st,
		reconciler: rec,
		cacheDir:   cacheDir,
		cursors:    cursors,
		now:        time.Now,
	}, nil
}

// loadSources seeds the store from the embedded catalog, then loads the
// enabled set. Seeding runs every boot: INSERT OR IGNORE means papers added to
// the catalog in a new release appear in the store, while rows the user has
// touched are never clobbered. Sources live as rows — provider type + JSON
// config — decoded into typed providers here; a row that fails to decode is
// skipped with a warning rather than taking the whole engine down.
func loadSources(st *store.Store, logger *slog.Logger) ([]source.Source, error) {
	entries, err := catalog.All()
	if err != nil {
		return nil, fmt.Errorf("paperboy: load catalog: %w", err)
	}
	rows := make([]store.SourceRow, 0, len(entries))
	for i, e := range entries {
		rows = append(rows, store.SourceRow{
			ID: e.ID, DisplayName: e.Name,
			ProviderType: e.Provider, ProviderConfig: e.Config,
			Enabled: e.Default, Position: i,
		})
	}
	if err := st.SeedSources(rows); err != nil {
		return nil, fmt.Errorf("paperboy: seed sources: %w", err)
	}

	stored, err := st.ListSources(true)
	if err != nil {
		return nil, fmt.Errorf("paperboy: list sources: %w", err)
	}
	var out []source.Source
	for _, r := range stored {
		s, err := registry.Build(r.ID, r.DisplayName, r.ProviderType, r.ProviderConfig)
		if err != nil {
			logger.Warn("skipping undecodable source", "id", r.ID, "err", err)
			continue
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("paperboy: no enabled sources decoded (%d enabled rows; check the warnings above, or set Config.Sources)", len(stored))
	}
	return out, nil
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
func (p *Paperboy) Close() error {
	return p.store.Close()
}

// StartReconciler launches the background mirror loop in its own goroutine. It
// reconciles immediately, then every PollInterval, until ctx is canceled.
func (p *Paperboy) StartReconciler(ctx context.Context) {
	go p.reconciler.Run(ctx)
}

// Poll runs a single reconcile pass across all sources synchronously.
func (p *Paperboy) Poll(ctx context.Context) {
	p.reconciler.ReconcileOnce(ctx)
}

// Refresh polls a single source synchronously and archives any new edition.
func (p *Paperboy) Refresh(ctx context.Context, sourceID string) error {
	src := source.ByID(p.sources, sourceID)
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
func (p *Paperboy) RenderCurrent(ctx context.Context, opts ...RenderOptions) (*Result, error) {
	o := optsOrDefault(opts)
	srcs := p.sources
	if len(o.Sources) > 0 {
		srcs = filterSources(p.sources, o.Sources)
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
func (p *Paperboy) RenderFor(ctx context.Context, sourceID string, opts ...RenderOptions) (*Result, error) {
	if source.ByID(p.sources, sourceID) == nil {
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

func (p *Paperboy) serve(ctx context.Context, entry archive.Entry, stale bool, opts RenderOptions) (*Result, error) {
	master := WidthMaster(p.cfg)
	// The cache key carries the master width so a PAPERBOY_WIDTH change never
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
			// Stat the artifact BEFORE rendering and stamp its mtime onto the
			// finished PNG: if the reconciler overwrites the artifact while
			// this render is in flight, the PNG (stamped with the old mtime)
			// stays older than the new artifact and re-renders next request —
			// otherwise the finished PNG's own mtime would mask the update.
			srcInfo, statErr := os.Stat(entry.Path)
			// Render detached from the requesting context: the flight's result
			// is shared by every concurrent waiter (and by the cache), so the
			// winner disconnecting must not cancel the render out from under
			// the rest. renderTimeout is the render's own bound.
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), renderTimeout)
			defer cancel()
			if err := p.renderer.Render(rctx, entry.Path, entry.Media, pngPath, master); err != nil {
				return nil, err
			}
			if statErr == nil {
				_ = os.Chtimes(pngPath, srcInfo.ModTime(), srcInfo.ModTime())
			}
			return nil, nil
		}); err != nil {
			return nil, fmt.Errorf("paperboy: render: %w", err)
		}
	}

	// Stat before read: the ETag hashes the mtime of the exact bytes served.
	// The cached PNG is mtime-stamped to the artifact it was rendered from, so
	// this stays coherent even if the reconciler overwrites the artifact while
	// this request is in flight (stat-ing the artifact here instead could pin
	// an old body under a new ETag behind 304s).
	pngInfo, err := os.Stat(pngPath)
	if err != nil {
		return nil, fmt.Errorf("paperboy: stat render: %w", err)
	}
	data, err := os.ReadFile(pngPath) //nolint:gosec // G304: internal cache path from a validated source entry, not user input
	if err != nil {
		return nil, fmt.Errorf("paperboy: read render: %w", err)
	}
	page, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("paperboy: decode render: %w", err)
	}

	out := compose(page, opts, master)
	var buf bytes.Buffer
	if err := imaging.Encode(&buf, out, imaging.PNG); err != nil {
		return nil, fmt.Errorf("paperboy: encode: %w", err)
	}

	daysOld := int(p.now().UTC().Sub(entry.Date).Hours()) / 24
	if daysOld < 0 {
		daysOld = 0
	}
	return &Result{
		Image:     buf.Bytes(),
		SourceID:  entry.SourceID,
		FetchedAt: entry.Date,
		Stale:     stale,
		DaysOld:   daysOld,
		Width:     out.Bounds().Dx(),
		Height:    out.Bounds().Dy(),
		ETag:      contentETag(entry, pngInfo.ModTime(), master, opts),
	}, nil
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
func (p *Paperboy) ListSources() []Source {
	out := make([]Source, len(p.sources))
	copy(out, p.sources)
	return out
}

// Ready reports whether at least one usable edition has been archived.
func (p *Paperboy) Ready() bool {
	_, ok := p.archive.NewestAny()
	return ok
}

// HealthSnapshot returns the current per-source health.
func (p *Paperboy) HealthSnapshot() Health {
	snap, err := p.store.HealthSnapshot()
	if err != nil {
		p.cfg.Logger.Warn("health snapshot failed", "err", err)
		return Health{Sources: map[string]SourceHealth{}}
	}
	out := Health{Sources: make(map[string]SourceHealth, len(snap))}
	for id, rec := range snap {
		out.Sources[id] = SourceHealth{
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
