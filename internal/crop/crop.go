// Package crop is the seam for smart alignment of newspaper front pages.
//
// The Cropper interface is the boundary between the orchestrator and a
// detection algorithm. The only implementation today is Noop, which returns
// the input unchanged; it is what the engine wires up by default. A real
// masthead/content-boundary detector can be added behind this interface later
// without changing any callers.
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

// Noop is the passthrough Cropper. It copies (or no-ops, if paths match) and
// returns Applied=false. It is the default until a real detector is wired up
// behind the Cropper interface.
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
