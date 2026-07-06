package rasterize

import (
	"context"
	"fmt"
	"image"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"

	"github.com/disintegration/imaging"
	"github.com/gen2brain/go-fitz"
)

// FitzRasterizer renders PDFs using go-fitz, which links the bundled MuPDF via
// cgo on default builds (an FFI/purego variant exists behind go-fitz's nocgo
// build tag). No external process, no system PDF libraries.
type FitzRasterizer struct {
	// DPI forces a fixed render DPI. 0 (the default) derives the DPI from the
	// page bounds so the raster comes out ~2x the target width — enough
	// supersampling for a sharp Lanczos downscale without the fixed-300-DPI
	// memory spike a large broadsheet incurs (a full page at 300 DPI is a
	// ~100MB+ RGBA allocation before grayscale/resize copies).
	DPI float64
}

// NewFitz returns a FitzRasterizer with defaults (bounds-derived DPI).
func NewFitz() *FitzRasterizer {
	return &FitzRasterizer{}
}

// Rasterize implements Rasterizer.
func (f *FitzRasterizer) Rasterize(ctx context.Context, pdfPath, pngPath string, width int) error {
	// Each rasterization transiently allocates ~150-200MB (full-page RGBA +
	// grayscale clone). Go returns freed heap to the OS lazily, so a sequence
	// of renders (the archive grid warming up) ratchets RSS toward an OOM on
	// small hosts even though renders are serialized. Force the release before
	// the next queued render starts; at our cadence the full GC is free.
	defer debug.FreeOSMemory()
	doc, err := fitz.New(pdfPath)
	if err != nil {
		return fmt.Errorf("rasterize: open pdf %s: %w", pdfPath, err)
	}
	defer func() { _ = doc.Close() }()

	if doc.NumPage() == 0 {
		return fmt.Errorf("rasterize: pdf %s has no pages", pdfPath)
	}

	dpi := f.DPI
	if dpi <= 0 {
		dpi = deriveDPI(doc, width)
	}

	// The stages below each allocate a full-page image; give a canceled caller
	// its answer between them instead of burning CPU on a render nobody wants.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("rasterize: %w", err)
	}
	img, err := doc.ImageDPI(0, dpi)
	if err != nil {
		return fmt.Errorf("rasterize: render page 0: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("rasterize: %w", err)
	}

	// Grayscale into an 8-bit buffer (a quarter of imaging.Grayscale's NRGBA
	// clone — the raster is the largest allocation in the process), then
	// resize; imaging's scanner reads the source incrementally, so the small
	// Gray image is the only extra copy.
	gray := toGray(img)
	img = nil //nolint:ineffassign,wastedassign // release the raster before the resize allocates
	resized := imaging.Resize(gray, width, 0, imaging.Lanczos)

	// Write atomically: encode to a temp file in the destination directory,
	// then rename into place. This guarantees a concurrent reader (or a second
	// render of the same source) never observes a half-written PNG.
	tmp, err := os.CreateTemp(filepath.Dir(pngPath), ".png-*.tmp")
	if err != nil {
		return fmt.Errorf("rasterize: tmp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once renamed

	if err := imaging.Encode(tmp, resized, imaging.PNG); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("rasterize: encode png: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("rasterize: sync png: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("rasterize: close png: %w", err)
	}
	if err := os.Rename(tmpName, pngPath); err != nil {
		return fmt.Errorf("rasterize: rename png %s: %w", pngPath, err)
	}
	return nil
}

// deriveDPI picks the render DPI that makes page 0 come out about 1.5x the
// target pixel width, clamped to [72, 300]. Page bounds are in points
// (1/72 inch), so dpi = 72 * desiredPx / boundsWidthPts. 1.5x supersampling
// keeps Lanczos output crisp while the full-page raster — the single largest
// allocation in the process, and the one that OOMs small hosts — stays ~half
// the size 2x would need. Falls back to 300 (the old fixed value) when bounds
// are unavailable.
func deriveDPI(doc *fitz.Document, width int) float64 {
	const minDPI, maxDPI = 72.0, 300.0
	bounds, err := doc.Bound(0)
	if err != nil || bounds.Dx() <= 0 || width <= 0 {
		return maxDPI
	}
	dpi := 72 * 1.5 * float64(width) / float64(bounds.Dx())
	return math.Min(maxDPI, math.Max(minDPI, dpi))
}

// toGray converts the raster to 8-bit grayscale (Rec. 601 luma).
func toGray(src *image.RGBA) *image.Gray {
	b := src.Bounds()
	dst := image.NewGray(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		si := src.PixOffset(b.Min.X, y)
		di := dst.PixOffset(b.Min.X, y)
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl := src.Pix[si], src.Pix[si+1], src.Pix[si+2]
			dst.Pix[di] = uint8((299*int(r) + 587*int(g) + 114*int(bl)) / 1000)
			si += 4
			di++
		}
	}
	return dst
}
