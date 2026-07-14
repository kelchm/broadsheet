package crop

import (
	"context"
	"image"
)

// ContentTrim removes uniform whitespace margins around a page's content.
//
// It's the Phase 1 detector: deterministic, dependency-free, and provably
// safe. It finds the bounding box of "ink" (pixels darker than a near-white
// threshold) and trims the blank border around it, then re-inflates by a
// small pad so content never sits flush against the edge. Because it only ever
// removes rows/columns that contain no ink, it can never cut into content —
// the worst case is that it trims nothing (returns found=false).
//
// One wrinkle it does handle: printer's marks. Many press PDFs carry a strip of
// registration marks, CMYK color bars, or a plate/fold ident code in the very
// top bleed (e.g. the NYT's "C M Y K … Nxxx,…,Bs-4C,E1"). That is ink but not
// content, and a naive "first ink row" would anchor the top on it. So the top
// edge skips a *leading bleed strip* — but only a band that is provably junk:
// thin, faint, in the extreme-top margin, and separated from the body by a
// clear whitespace gap (see topEdge). The conditions are strict enough that a
// real element (a dateline, a rule under the masthead) is never skipped;
// validated by the eval-corpus A/B (12 NYT fixes, 0 regressions).
//
// This is still *not* masthead or skybox removal: a promo strip above the
// nameplate is thick content-grade ink, so ContentTrim keeps it. Those
// detectors come in a later phase, behind the same Detector interface. What
// ContentTrim buys now is a tighter, margin-normalized page — with zero risk.
type ContentTrim struct {
	// DarkThreshold is the luma (0-255) at or below which a pixel counts as
	// ink. Renders are grayscale on a ~255 white ground; the default leaves
	// headroom for antialiasing and faint paper tint.
	DarkThreshold uint8
	// MinInk is the number of ink pixels a row/column must contain to count as
	// "content" rather than noise. Small, so a single stray speck can't defeat
	// the trim, but a real line of type clears it easily.
	MinInk int
	// PadFraction re-inflates the detected box on every side by this fraction
	// of the corresponding dimension, so the crop keeps a hair of breathing
	// room around content. 0.005 = 0.5%.
	PadFraction float64

	// Bleed-strip skip (see topEdge). A leading ink band is discarded as a
	// printer's-mark strip only when it satisfies ALL four, as fractions of the
	// page: it starts within BleedFraction of the top, is at most
	// MaxStripFraction tall, its densest row is below SparseFraction of the
	// width, and the whitespace after it is at least MinGapFraction (and at
	// least as tall as the strip itself).
	BleedFraction    float64 // top margin the strip must start within. Default 0.025.
	MaxStripFraction float64 // max strip height (of page height). Default 0.012.
	SparseFraction   float64 // max strip peak ink (of width). Default 0.18.
	MinGapFraction   float64 // min trailing whitespace (of height). Default 0.015.
}

// NewContentTrim returns a ContentTrim with defaults tuned for MuPDF-rasterized
// front pages on a white ground.
func NewContentTrim() *ContentTrim {
	return &ContentTrim{
		DarkThreshold:    245,
		MinInk:           3,
		PadFraction:      0.005,
		BleedFraction:    0.025,
		MaxStripFraction: 0.012,
		SparseFraction:   0.18,
		MinGapFraction:   0.015,
	}
}

// Detect implements Detector. hints are unused. It never returns an error.
func (t *ContentTrim) Detect(ctx context.Context, img image.Image, _ Hints) (Box, bool, error) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return Full(), false, nil
	}

	// Per-row and per-column ink counts in one pass over the pixels.
	rowInk := make([]int, h)
	colInk := make([]int, w)
	for y := 0; y < h; y++ {
		if ctx.Err() != nil {
			return Full(), false, ctx.Err()
		}
		for x := 0; x < w; x++ {
			if luma(img.At(b.Min.X+x, b.Min.Y+y)) <= t.DarkThreshold {
				rowInk[y]++
				colInk[x]++
			}
		}
	}

	top := t.topEdge(rowInk, w, h)
	if top < 0 {
		// The page is effectively blank — trimming would erase it. Leave it.
		return Full(), false, nil
	}
	bottom := lastAtLeast(rowInk, t.MinInk)
	left := firstAtLeast(colInk, t.MinInk)
	right := lastAtLeast(colInk, t.MinInk)

	// Re-inflate by the pad, in pixels, clamped to the image.
	padX := int(t.PadFraction*float64(w) + 0.5)
	padY := int(t.PadFraction*float64(h) + 0.5)
	x0 := max(0, left-padX)
	y0 := max(0, top-padY)
	x1 := min(w-1, right+padX)
	y1 := min(h-1, bottom+padY)

	box := Box{
		X: float64(x0) / float64(w),
		Y: float64(y0) / float64(h),
		W: float64(x1-x0+1) / float64(w),
		H: float64(y1-y0+1) / float64(h),
	}.Clamp()

	if box.IsEffectivelyFull() {
		return Full(), false, nil
	}
	return box, true, nil
}

// luma returns the Rec. 601 luma of c as an 8-bit value. Renders are already
// grayscale, but computing luma keeps the detector correct on color input too.
func luma(c interface{ RGBA() (r, g, b, a uint32) }) uint8 {
	r, g, b, _ := c.RGBA() // 16-bit premultiplied, 0-0xffff
	// 0.299R + 0.587G + 0.114B, then down to 8 bits.
	y := (299*r + 587*g + 114*b) / 1000
	return uint8(y >> 8) //nolint:gosec // y is bounded to 16-bit by construction; >>8 fits 8 bits
}

// firstAtLeast returns the index of the first element >= n, or -1 if none.
func firstAtLeast(counts []int, n int) int {
	for i, c := range counts {
		if c >= n {
			return i
		}
	}
	return -1
}

// lastAtLeast returns the index of the last element >= n, or -1 if none.
func lastAtLeast(counts []int, n int) int {
	for i := len(counts) - 1; i >= 0; i-- {
		if counts[i] >= n {
			return i
		}
	}
	return -1
}

// inkRun is a contiguous run of ink rows [lo, hi], inclusive.
type inkRun struct{ lo, hi int }

// inkBands segments rowInk into the contiguous runs whose count >= minInk.
func inkBands(rowInk []int, minInk int) []inkRun {
	var out []inkRun
	for y := 0; y < len(rowInk); {
		if rowInk[y] >= minInk {
			lo := y
			for y < len(rowInk) && rowInk[y] >= minInk {
				y++
			}
			out = append(out, inkRun{lo: lo, hi: y - 1})
		} else {
			y++
		}
	}
	return out
}

// topEdge returns the first content row, discarding any leading "bleed strip"
// (registration marks / CMYK bars / plate-ident code) that sits in the extreme
// top margin. A strip is skipped only when it is thin, faint, high on the page,
// and followed by a clear whitespace gap — see the ContentTrim doc. Returns -1
// when the page holds no ink at all.
func (t *ContentTrim) topEdge(rowInk []int, w, h int) int {
	bands := inkBands(rowInk, t.MinInk)
	if len(bands) == 0 {
		return -1
	}
	i := 0
	for i < len(bands)-1 {
		strip := bands[i]
		stripH := strip.hi - strip.lo + 1
		gap := bands[i+1].lo - strip.hi - 1
		peak := 0
		for y := strip.lo; y <= strip.hi; y++ {
			if rowInk[y] > peak {
				peak = rowInk[y]
			}
		}
		isBleed := float64(strip.lo) <= t.BleedFraction*float64(h) &&
			float64(stripH) <= t.MaxStripFraction*float64(h) &&
			float64(peak) < t.SparseFraction*float64(w) &&
			float64(gap) >= max(float64(stripH), t.MinGapFraction*float64(h))
		if !isBleed {
			break // the topmost band is real content; stop here
		}
		i++
	}
	return bands[i].lo
}
