// Package crop implements smart alignment of newspaper front pages.
//
// The interface here is the boundary between the orchestrator and the actual
// detection algorithm. Two implementations are planned:
//
//   - Noop:     returns the input unchanged. Used as a baseline + when CV deps
//     are not available.
//   - OpenCV:   uses gocv for whitespace trim, masthead band detection, and
//     optional OCR-confirmed crop. The real "smart" crop.
package crop

import (
	"context"
	"os"
)

// Hints lets callers pass per-source detection hints.
type Hints struct {
	// MastheadText is the visible paper name for OCR confirmation.
	MastheadText string
}

// Result describes the crop applied (or not).
type Result struct {
	// Applied is true if the cropper modified the image.
	Applied bool
	// Confidence is in [0,1]; meaningful only when Applied is true.
	Confidence float64
}

// Cropper transforms an input PNG into an aligned/cropped output PNG.
type Cropper interface {
	// Crop reads inPath and writes the (possibly cropped) PNG to outPath.
	// inPath and outPath MAY be the same; implementations must handle that.
	Crop(ctx context.Context, inPath, outPath string, hints Hints) (Result, error)
}

// Noop is the placeholder Cropper. It copies (or no-ops, if paths match) and
// returns Applied=false. Useful when the OpenCV-based cropper is unavailable
// or disabled.
type Noop struct{}

// Crop implements Cropper by copying inPath to outPath (or doing nothing if
// they are the same file).
func (Noop) Crop(_ context.Context, inPath, outPath string, _ Hints) (Result, error) {
	if inPath == outPath {
		return Result{Applied: false}, nil
	}
	data, err := os.ReadFile(inPath)
	if err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(outPath, data, 0o644); err != nil {
		return Result{}, err
	}
	return Result{Applied: false}, nil
}
