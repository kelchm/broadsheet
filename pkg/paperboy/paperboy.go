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
	"image"
	_ "image/png" // register PNG decoder for image.Decode
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/disintegration/imaging"

	"github.com/kelchm/paperboy/internal/archive"
	"github.com/kelchm/paperboy/internal/buildinfo"
	"github.com/kelchm/paperboy/internal/cache"
	"github.com/kelchm/paperboy/internal/reconcile"
	"github.com/kelchm/paperboy/internal/registry"
	"github.com/kelchm/paperboy/internal/render"
	"github.com/kelchm/paperboy/internal/source"
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

const (
	defaultWidth       = 1600
	defaultRotate      = time.Hour
	defaultPoll        = 30 * time.Minute
	defaultArchiveDays = 14
)

// Config holds runtime configuration for a Paperboy instance.
type Config struct {
	// DataDir is where the archive, render cache, and state.json live. Required.
	DataDir string

	// Width is the master render width in pixels (quality ceiling). Default 1600.
	Width int

	// RotateInterval is how long each source stays the /current slot. Default 1h.
	RotateInterval time.Duration

	// PollInterval is the reconciler cadence. Default 30m.
	PollInterval time.Duration

	// ArchiveDays is how many days of editions to retain. Default 14.
	ArchiveDays int

	// Sources optionally overrides the default source registry.
	Sources []Source

	// Logger; if nil, slog.Default() is used.
	Logger *slog.Logger
}

// RenderOptions are per-call overrides. The zero value returns the master-width
// PNG untouched; OutputWidth resizes down from the master (values above the
// master are capped — no upscaling).
type RenderOptions struct {
	OutputWidth int
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
	store      *cache.Store
	reconciler *reconcile.Reconciler
	cacheDir   string
	rotate     time.Duration
	now        func() time.Time
}

// New constructs a Paperboy with the given config.
func New(cfg Config) (*Paperboy, error) {
	if cfg.DataDir == "" {
		return nil, fmt.Errorf("paperboy: DataDir is required")
	}
	if cfg.Width == 0 {
		cfg.Width = defaultWidth
	}
	if cfg.RotateInterval <= 0 {
		cfg.RotateInterval = defaultRotate
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

	srcs := cfg.Sources
	if srcs == nil {
		srcs = registry.Default()
	}

	archiveDir := filepath.Join(cfg.DataDir, "archive")
	cacheDir := filepath.Join(cfg.DataDir, "cache")
	for _, d := range []string{archiveDir, cacheDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, fmt.Errorf("paperboy: create %s: %w", d, err)
		}
	}

	store, err := cache.Open(filepath.Join(cfg.DataDir, "state.json"))
	if err != nil {
		return nil, fmt.Errorf("paperboy: open state: %w", err)
	}

	arch := &archive.Store{Root: archiveDir}
	rec := &reconcile.Reconciler{
		Sources:   srcs,
		Archive:   arch,
		Store:     store,
		Deps:      source.Deps{HTTP: &http.Client{Timeout: 30 * time.Second}, Logger: cfg.Logger},
		Retention: time.Duration(cfg.ArchiveDays) * 24 * time.Hour,
		Interval:  cfg.PollInterval,
		Logger:    cfg.Logger,
	}

	return &Paperboy{
		cfg:        cfg,
		sources:    srcs,
		archive:    arch,
		renderer:   render.New(),
		store:      store,
		reconciler: rec,
		cacheDir:   cacheDir,
		rotate:     cfg.RotateInterval,
		now:        time.Now,
	}, nil
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
		return fmt.Errorf("paperboy: unknown source %q", sourceID)
	}
	p.reconciler.ReconcileSource(ctx, *src, p.now().UTC())
	return nil
}

// RenderCurrent returns the current rotation slot — a deterministic function of
// the clock, so it is a safe read that does not mutate anything. Falls back to
// the newest archived edition of any source if the slot's source has none yet.
func (p *Paperboy) RenderCurrent(ctx context.Context, opts ...RenderOptions) (*Result, error) {
	n := len(p.sources)
	if n == 0 {
		return nil, fmt.Errorf("paperboy: no sources configured")
	}
	src := p.sources[rotationSlot(p.now(), p.rotate, n)]
	if entry, ok := p.archive.Newest(src.ID); ok {
		return p.serve(ctx, entry, false, optsOrDefault(opts))
	}
	if entry, ok := p.archive.NewestAny(); ok {
		return p.serve(ctx, entry, true, optsOrDefault(opts))
	}
	return nil, ErrNoneAvailable
}

// RenderFor returns the newest archived edition for a specific source.
func (p *Paperboy) RenderFor(ctx context.Context, sourceID string, opts ...RenderOptions) (*Result, error) {
	if source.ByID(p.sources, sourceID) == nil {
		return nil, fmt.Errorf("paperboy: unknown source %q", sourceID)
	}
	entry, ok := p.archive.Newest(sourceID)
	if !ok {
		return nil, fmt.Errorf("paperboy: no archived edition for %s yet", sourceID)
	}
	return p.serve(ctx, entry, false, optsOrDefault(opts))
}

// rotationSlot picks the current source index as a pure function of the clock.
func rotationSlot(now time.Time, interval time.Duration, n int) int {
	if n <= 0 {
		return 0
	}
	secs := int64(interval / time.Second)
	if secs <= 0 {
		secs = 1
	}
	return int((now.Unix() / secs) % int64(n))
}

func (p *Paperboy) serve(ctx context.Context, entry archive.Entry, stale bool, opts RenderOptions) (*Result, error) {
	master := WidthMaster(p.cfg)
	pngPath := filepath.Join(p.cacheDir, entry.SourceID, entry.Date.UTC().Format("20060102")+".png")

	// Render lazily into the disposable cache on first view.
	if !fileNonEmpty(pngPath) {
		if err := p.renderer.Render(ctx, entry.Path, entry.Media, pngPath, master); err != nil {
			return nil, fmt.Errorf("paperboy: render: %w", err)
		}
	}

	data, err := os.ReadFile(pngPath) //nolint:gosec // G304: internal cache path from a validated source entry, not user input
	if err != nil {
		return nil, fmt.Errorf("paperboy: read render: %w", err)
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("paperboy: decode render: %w", err)
	}

	out := opts.OutputWidth
	if out > master {
		out = master
	}
	if out > 0 && out != img.Bounds().Dx() {
		img = imaging.Resize(img, out, 0, imaging.Lanczos)
		var buf bytes.Buffer
		if err := imaging.Encode(&buf, img, imaging.PNG); err != nil {
			return nil, fmt.Errorf("paperboy: encode resized: %w", err)
		}
		data = buf.Bytes()
	}

	daysOld := int(p.now().UTC().Sub(entry.Date).Hours()) / 24
	if daysOld < 0 {
		daysOld = 0
	}
	return &Result{
		Image:     data,
		SourceID:  entry.SourceID,
		FetchedAt: entry.Date,
		Stale:     stale,
		DaysOld:   daysOld,
		Width:     img.Bounds().Dx(),
		Height:    img.Bounds().Dy(),
	}, nil
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
	snap := p.store.Snapshot()
	out := Health{Sources: make(map[string]SourceHealth, len(snap.Sources))}
	for id, rec := range snap.Sources {
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

func fileNonEmpty(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Size() > 0
}
