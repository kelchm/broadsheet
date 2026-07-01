// Package render normalizes an archived edition into a master-width grayscale
// PNG in the disposable cache. PDFs are rasterized (go-fitz); already-rendered
// image artifacts are decoded and normalized. The per-request downscale from
// the master lives in the engine, not here.
package render

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/disintegration/imaging"

	"github.com/kelchm/paperboy/internal/rasterize"
	"github.com/kelchm/paperboy/internal/source"
)

// Renderer produces master-width PNGs from archived artifacts.
type Renderer struct {
	Rasterizer rasterize.Rasterizer
}

// New returns a Renderer backed by the default (fitz/MuPDF) rasterizer.
func New() *Renderer {
	return &Renderer{Rasterizer: rasterize.NewFitz()}
}

// Render writes a master-width PNG for the archived artifact at srcPath to
// dstPath, dispatching on media type.
func (r *Renderer) Render(ctx context.Context, srcPath string, media source.MediaType, dstPath string, width int) error {
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o750); err != nil {
		return fmt.Errorf("render: mkdir: %w", err)
	}
	switch media {
	case source.MediaPDF:
		return r.Rasterizer.Rasterize(ctx, srcPath, dstPath, width)
	case source.MediaImage:
		return renderImage(srcPath, dstPath, width)
	default:
		return fmt.Errorf("render: unsupported media %q", media)
	}
}

// renderImage decodes an image artifact, grayscales and resizes it to width,
// and writes the PNG atomically (temp + rename).
func renderImage(srcPath, dstPath string, width int) error {
	img, err := imaging.Open(srcPath)
	if err != nil {
		return fmt.Errorf("render: open image %s: %w", srcPath, err)
	}
	out := imaging.Resize(imaging.Grayscale(img), width, 0, imaging.Lanczos)

	tmp, err := os.CreateTemp(filepath.Dir(dstPath), ".png-*.tmp")
	if err != nil {
		return fmt.Errorf("render: tmp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if err := imaging.Encode(tmp, out, imaging.PNG); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("render: encode: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("render: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("render: close: %w", err)
	}
	if err := os.Rename(tmpName, dstPath); err != nil {
		return fmt.Errorf("render: rename %s: %w", dstPath, err)
	}
	return nil
}
