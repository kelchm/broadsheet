// Package rasterize turns a PDF into a raster image.
//
// We define a small interface so callers don't depend on a specific backend.
// Two implementations are planned:
//
//   - magickRasterizer: shells out to ImageMagick. Works without CGo. Default.
//   - fitzRasterizer:   uses go-fitz (MuPDF). Faster, no external process.
//     Selected when CGo is enabled and the build tag is set.
package rasterize

import "context"

// Rasterizer converts a PDF on disk to a PNG on disk.
type Rasterizer interface {
	// Rasterize reads the PDF at pdfPath and writes a PNG to pngPath.
	// The output is grayscale, width pixels wide, and aspect-ratio preserved.
	Rasterize(ctx context.Context, pdfPath, pngPath string, width int) error
}
