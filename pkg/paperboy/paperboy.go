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
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/kelchm/paperboy/internal/cache"
	"github.com/kelchm/paperboy/internal/crop"
	"github.com/kelchm/paperboy/internal/fetch"
	"github.com/kelchm/paperboy/internal/rasterize"
	"github.com/kelchm/paperboy/internal/rotation"
	"github.com/kelchm/paperboy/internal/source"
)

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

	// CropOCR enables the optional OCR-confirmed masthead detection pass.
	// Default false (heuristic-only crop, or Noop if OpenCV isn't built in).
	CropOCR bool

	// Logger; if nil, slog.Default() is used.
	Logger *slog.Logger
}

// Result is what a render call returns.
type Result struct {
	Image     []byte    // rendered PNG bytes
	SourceID  string    // which source produced the image
	FetchedAt time.Time // when the underlying PDF was acquired
	Stale     bool      // true if served from cache because no live fetch succeeded
	DaysOld   int       // 0 for today, 1 for yesterday, etc.
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
func (p *Paperboy) RenderNext(ctx context.Context) (*Result, error) {
	r, err := p.picker.PickNext(ctx)
	if err != nil {
		return nil, err
	}
	return p.readResult(r)
}

// RenderFor returns a render for a specific source. Does not advance the
// rotation index.
func (p *Paperboy) RenderFor(ctx context.Context, sourceID string) (*Result, error) {
	src := source.ByID(p.sources, sourceID)
	if src == nil {
		return nil, fmt.Errorf("paperboy: unknown source %q", sourceID)
	}
	for d := 0; d <= 2; d++ {
		path, ts, err := p.pipeline.FetchAndRender(ctx, *src, d)
		if err == nil {
			return p.readResult(&rotation.Result{
				SourceID: sourceID, PNGPath: path, FetchedAt: ts, DaysOld: d,
			})
		}
	}
	return nil, fmt.Errorf("paperboy: no usable paper for %s in last 3 days", sourceID)
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

func (p *Paperboy) readResult(r *rotation.Result) (*Result, error) {
	data, err := os.ReadFile(r.PNGPath)
	if err != nil {
		return nil, fmt.Errorf("paperboy: read rendered image: %w", err)
	}
	return &Result{
		Image:     data,
		SourceID:  r.SourceID,
		FetchedAt: r.FetchedAt,
		Stale:     r.Stale,
		DaysOld:   r.DaysOld,
	}, nil
}
