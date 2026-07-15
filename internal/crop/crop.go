// Package crop trims a rendered newspaper front page to a tighter box.
//
// The engine renders every edition to a master-width PNG and then, at serve
// time, applies a crop before framing (see pkg/broadsheet). Crop is metadata,
// not a new artifact: a normalized [Box] is resolved per source/edition and
// applied to the decoded master. The master render stays crop-agnostic, so
// changing a crop never forces a re-render — only a new ETag.
//
// Two layers, mirroring the render/detect split:
//
//   - [Detector] inspects a page image and proposes a [Box]. Phase 1 ships one
//     detector, [ContentTrim], which is deterministic and provably safe: it
//     only ever trims uniform whitespace margins, so it can never cut into
//     real content. Smarter detectors (masthead / skybox removal) plug in
//     behind the same interface once there's a trustworthy eval to judge them.
//
//   - A [Box] is the resolved crop, in normalized coordinates so it's
//     independent of the master width. [Box.Apply] realizes it against a
//     concrete image.
//
// The package is deliberately independent of internal/source: the engine
// translates source.CropHints into [Hints] at the seam.
package crop

import (
	"context"
	"image"
	"math"
)

// AlgoVersion identifies the auto-detection behavior. It is folded into the
// render ETag so a change to any detector's output invalidates cached crops
// derived from the old behavior. Bump it whenever a detector's geometry
// changes in a way that should re-crop already-cached editions.
const AlgoVersion = "1"

// minSpan is the smallest crop dimension (as a fraction of the image) treated as
// legitimate. Anything smaller is a corrupt/degenerate box, so Clamp collapses
// it to Full() rather than letting Apply emit a sliver crop.
const minSpan = 0.02

// Box is a crop rectangle in normalized coordinates: X, Y, W, H each in [0,1],
// as fractions of the source image's width/height. Normalizing keeps a box
// meaningful across master-width changes and across the master/downscaled
// variants of the same page.
//
// The zero Box has W==0/H==0 and is treated as "no crop" (see IsEffectivelyFull
// / Apply), so a Box that was never set is safe to apply.
type Box struct {
	X, Y, W, H float64
}

// Full is the identity box covering the whole image.
func Full() Box { return Box{X: 0, Y: 0, W: 1, H: 1} }

// Clamp returns b with its coordinates constrained to a valid sub-rectangle of
// the unit square. An out-of-range or degenerate box collapses toward Full so
// a bad detector or a malformed override can never produce an empty or
// out-of-bounds crop.
func (b Box) Clamp() Box {
	// A non-finite coordinate (a corrupted override, a NaN from bad math) would
	// slip past every comparison below, so reject it up front.
	for _, v := range [4]float64{b.X, b.Y, b.W, b.H} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return Full()
		}
	}
	if b.W < minSpan || b.H < minSpan {
		return Full()
	}
	x, y, w, h := b.X, b.Y, b.W, b.H
	if x < 0 {
		w += x // pulling the left edge back in shrinks width
		x = 0
	}
	if y < 0 {
		h += y
		y = 0
	}
	if x > 1 {
		x = 1
	}
	if y > 1 {
		y = 1
	}
	if x+w > 1 {
		w = 1 - x
	}
	if y+h > 1 {
		h = 1 - y
	}
	if w < minSpan || h < minSpan {
		return Full()
	}
	return Box{X: x, Y: y, W: w, H: h}
}

// IsEffectivelyFull reports whether b, once clamped, covers essentially the
// whole image — within eps on every edge. Such a box isn't worth applying
// (the crop would be a no-op or shave a sub-pixel sliver), so callers skip it.
func (b Box) IsEffectivelyFull() bool {
	const eps = 0.002 // ~3px on a 1600px master
	c := b.Clamp()
	return c.X <= eps && c.Y <= eps && c.X+c.W >= 1-eps && c.Y+c.H >= 1-eps
}

// Apply crops img to b (clamped). A box that's effectively full returns img
// unchanged. The returned image shares img's pixel storage where possible
// (image.Image sub-imaging), so callers must not mutate it.
func (b Box) Apply(img image.Image) image.Image {
	if b.IsEffectivelyFull() {
		return img
	}
	c := b.Clamp()
	bnds := img.Bounds()
	iw, ih := bnds.Dx(), bnds.Dy()
	x0 := bnds.Min.X + int(c.X*float64(iw)+0.5)
	y0 := bnds.Min.Y + int(c.Y*float64(ih)+0.5)
	x1 := bnds.Min.X + int((c.X+c.W)*float64(iw)+0.5)
	y1 := bnds.Min.Y + int((c.Y+c.H)*float64(ih)+0.5)
	// Guard against rounding that collapses the rect.
	if x1 <= x0 {
		x1 = x0 + 1
	}
	if y1 <= y0 {
		y1 = y0 + 1
	}
	rect := image.Rect(x0, y0, x1, y1).Intersect(bnds)
	if sub, ok := img.(interface {
		SubImage(image.Rectangle) image.Image
	}); ok {
		return sub.SubImage(rect)
	}
	// Fallback for images without SubImage: copy the region.
	dst := image.NewNRGBA(image.Rect(0, 0, rect.Dx(), rect.Dy()))
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			dst.Set(x-rect.Min.X, y-rect.Min.Y, img.At(x, y))
		}
	}
	return dst
}

// Hints carry per-source detection inputs, translated from source.CropHints at
// the engine seam so this package stays independent of internal/source. Phase
// 1's ContentTrim ignores them; they exist for the masthead/skybox detectors
// that land in later phases.
type Hints struct {
	// MastheadText is the visible nameplate string, an OCR/text-layer target.
	MastheadText string
	// PDFPath is the archived source PDF, for text-layer detectors. May be empty.
	PDFPath string
}

// Detector inspects a page image and proposes a crop.
//
// found is false when the detector has no opinion (leave the page uncropped);
// a returned Box is only meaningful when found is true. err is reserved for
// real failures (a detector that shells out, etc.) — "nothing detected" is
// (Full, false, nil), not an error.
type Detector interface {
	Detect(ctx context.Context, img image.Image, hints Hints) (box Box, found bool, err error)
}
