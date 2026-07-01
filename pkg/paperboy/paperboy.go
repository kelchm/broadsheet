// Package paperboy is the public Go API for the paperboy newspaper renderer.
//
// Most users will run the paperboy-server binary or hit its HTTP endpoints
// directly. This package is for embedding the engine into another Go program —
// for example, a custom TRMNL plugin or a Home Assistant integration.
//
// Basic usage:
//
//	p, err := paperboy.New(paperboy.Config{DataDir: "./data"})
//	if err != nil { ... }
//	res, err := p.RenderNext(ctx)
//	// res.Image is PNG bytes
package paperboy

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/png" // register PNG decoder for image.Decode
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/disintegration/imaging"

	"github.com/kelchm/paperboy/internal/buildinfo"
	"github.com/kelchm/paperboy/internal/cache"
	"github.com/kelchm/paperboy/internal/crop"
	"github.com/kelchm/paperboy/internal/fetch"
	"github.com/kelchm/paperboy/internal/rasterize"
	"github.com/kelchm/paperboy/internal/rotation"
	"github.com/kelchm/paperboy/internal/source"
)

// Version reports the paperboy release version.
func Version() string { return buildinfo.Version }

// Source describes a newspaper feed. Alias of the internal canonical type.
type Source = source.Source

// CropHints carries per-source hints for the crop detector. Alias of the
// internal canonical type.
type CropHints = source.CropHints

// Config holds runtime configuration for a Paperboy instance.
type Config struct {
	// DataDir is where cached images and state.json live. Required.
	DataDir string

	// Width is the target image width in pixels. Default 1600.
	Width int

	// Sources optionally overrides the default source registry.
	// If nil, the built-in registry is used.
	Sources []Source

	// Logger; if nil, slog.Default() is used.
	Logger *slog.Logger
}

// RenderOptions are per-call overrides. The zero value means "use defaults":
// return the master-width cached PNG untouched.
//
// The server-side PAPERBOY_WIDTH (Config.Width) is the *master* width — the
// resolution we rasterize and cache at, which acts as a quality ceiling.
// Per-call OutputWidth resizes down from that master before returning.
// Requests for OutputWidth larger than the master are capped at the master
// to avoid upscaling artifacts (text softening).
type RenderOptions struct {
	// OutputWidth is the desired width in pixels. 0 means "no resize"
	// (return the master). Values larger than the master are capped.
	OutputWidth int
}

// Result is what a render call returns.
type Result struct {
	Image     []byte    // rendered PNG bytes
	SourceID  string    // which source produced the image
	FetchedAt time.Time // when the underlying PDF was acquired
	Stale     bool      // true if served from cache because no live fetch succeeded
	DaysOld   int       // 0 for today, 1 for yesterday, etc.
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

// Paperboy is the engine. Construct one with New, then call RenderNext or
// RenderFor as needed. Safe for concurrent use.
type Paperboy struct {
	cfg      Config
	sources  []source.Source
	store    *cache.Store
	images   *cache.Images
	picker   *rotation.Picker
	pipeline *pipelineFetcher
}

// New constructs a Paperboy with the given config.
func New(cfg Config) (*Paperboy, error) {
	if cfg.DataDir == "" {
		return nil, fmt.Errorf("paperboy: DataDir is required")
	}
	if cfg.Width == 0 {
		cfg.Width = 1600
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	srcs := cfg.Sources
	if srcs == nil {
		srcs = source.Default()
	}

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("paperboy: create data dir: %w", err)
	}

	images := &cache.Images{Root: cfg.DataDir}
	store, err := cache.Open(filepath.Join(cfg.DataDir, "state.json"))
	if err != nil {
		return nil, fmt.Errorf("paperboy: open cache state: %w", err)
	}

	pipeline := &pipelineFetcher{
		images:     images,
		fetcher:    fetch.New(),
		rasterizer: rasterize.NewFitz(),
		cropper:    crop.Noop{},
		width:      cfg.Width,
	}

	picker := &rotation.Picker{
		Sources: srcs,
		Store:   store,
		Fetcher: pipeline,
		Lookup:  &cache.FilesystemLookup{Images: images},
		Logger:  cfg.Logger,
	}

	return &Paperboy{
		cfg:      cfg,
		sources:  srcs,
		store:    store,
		images:   images,
		picker:   picker,
		pipeline: pipeline,
	}, nil
}

// RenderNext returns the next paper in the rotation, advancing the rotation
// index. Falls back across sources and dates on failure.
//
// Pass a RenderOptions value to control output dimensions per-call:
//
//	res, _ := p.RenderNext(ctx)                                    // master width
//	res, _ := p.RenderNext(ctx, paperboy.RenderOptions{OutputWidth: 800})
func (p *Paperboy) RenderNext(ctx context.Context, opts ...RenderOptions) (*Result, error) {
	r, err := p.picker.PickNext(ctx)
	if err != nil {
		return nil, err
	}
	return p.readResult(r, optsOrDefault(opts))
}

// RenderFor returns a render for a specific source. Does not advance the
// rotation index, and does not fall back to other sources, but it does record
// per-source health.
func (p *Paperboy) RenderFor(ctx context.Context, sourceID string, opts ...RenderOptions) (*Result, error) {
	r, err := p.picker.PickFor(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	return p.readResult(r, optsOrDefault(opts))
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

func (p *Paperboy) readResult(r *rotation.Result, opts RenderOptions) (*Result, error) {
	data, err := os.ReadFile(r.PNGPath)
	if err != nil {
		return nil, fmt.Errorf("paperboy: read rendered image: %w", err)
	}

	masterCfg := WidthMaster(p.cfg)
	out := opts.OutputWidth
	if out > masterCfg {
		out = masterCfg
	}

	// Decode once so we always know exact dimensions, even for the
	// no-resize path.
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("paperboy: decode cached png: %w", err)
	}

	if out > 0 && out != img.Bounds().Dx() {
		img = imaging.Resize(img, out, 0, imaging.Lanczos)
		var buf bytes.Buffer
		if err := imaging.Encode(&buf, img, imaging.PNG); err != nil {
			return nil, fmt.Errorf("paperboy: encode resized png: %w", err)
		}
		data = buf.Bytes()
	}

	return &Result{
		Image:     data,
		SourceID:  r.SourceID,
		FetchedAt: r.FetchedAt,
		Stale:     r.Stale,
		DaysOld:   r.DaysOld,
		Width:     img.Bounds().Dx(),
		Height:    img.Bounds().Dy(),
	}, nil
}

// WidthMaster returns the master (cache) width for a Config, applying the
// default if unset.
func WidthMaster(c Config) int {
	if c.Width <= 0 {
		return 1600
	}
	return c.Width
}
