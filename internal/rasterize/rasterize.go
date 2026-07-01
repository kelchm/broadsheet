// Package rasterize turns a PDF into a raster image.
//
// We define a small interface so callers don't depend on a specific backend.
// The only implementation today is FitzRasterizer, which uses go-fitz (MuPDF
// statically linked from the bundled archive — no external process and no
// system PDF libraries required). The interface leaves room for alternative
// backends without touching callers.
package rasterize

import "context"

// Rasterizer converts a PDF on disk to a PNG on disk.
type Rasterizer interface {
	// Rasterize reads the PDF at pdfPath and writes a PNG to pngPath.
	// The output is grayscale, width pixels wide, and aspect-ratio preserved.
	Rasterize(ctx context.Context, pdfPath, pngPath string, width int) error
}
