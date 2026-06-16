package rasterize

import (
	"context"
	"fmt"
	"os"

	"github.com/disintegration/imaging"
	"github.com/gen2brain/go-fitz"
)

// FitzRasterizer renders PDFs using go-fitz, which bundles MuPDF via FFI
// (no CGo, no system libs required on supported platforms).
//
// This is the default rasterizer — fast, native, and requires zero setup
// beyond `go get`.
type FitzRasterizer struct {
	// DPI is the render DPI. Default 300.
	DPI float64
}

// NewFitz returns a FitzRasterizer with defaults.
func NewFitz() *FitzRasterizer {
	return &FitzRasterizer{DPI: 300}
}

// Rasterize implements Rasterizer.
func (f *FitzRasterizer) Rasterize(_ context.Context, pdfPath, pngPath string, width int) error {
	dpi := f.DPI
	if dpi == 0 {
		dpi = 300
	}

	doc, err := fitz.New(pdfPath)
	if err != nil {
		return fmt.Errorf("rasterize: open pdf %s: %w", pdfPath, err)
	}
	defer doc.Close()

	if doc.NumPage() == 0 {
		return fmt.Errorf("rasterize: pdf %s has no pages", pdfPath)
	}

	img, err := doc.ImageDPI(0, dpi)
	if err != nil {
		return fmt.Errorf("rasterize: render page 0: %w", err)
	}

	// Grayscale + resize to target width, preserving aspect ratio.
	gray := imaging.Grayscale(img)
	resized := imaging.Resize(gray, width, 0, imaging.Lanczos)

	out, err := os.Create(pngPath)
	if err != nil {
		return fmt.Errorf("rasterize: create png %s: %w", pngPath, err)
	}
	defer out.Close()

	if err := imaging.Encode(out, resized, imaging.PNG); err != nil {
		return fmt.Errorf("rasterize: encode png: %w", err)
	}
	return nil
}
