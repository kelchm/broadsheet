package crop

import (
	"context"
	"image"
	"image/color"
	"testing"
)

// span is an ink rectangle [y0,y1)×[x0,x1) painted onto a white page.
type span struct{ y0, y1, x0, x1 int }

const testW = 1000

func page(h int, spans ...span) *image.Gray {
	img := image.NewGray(image.Rect(0, 0, testW, h))
	for i := range img.Pix {
		img.Pix[i] = 255 // white ground
	}
	for _, s := range spans {
		for y := s.y0; y < s.y1; y++ {
			for x := s.x0; x < s.x1; x++ {
				img.SetGray(x, y, color.Gray{Y: 0})
			}
		}
	}
	return img
}

// topPx runs the default ContentTrim and returns the detected top edge in
// pixels (box.Y × height) plus found.
func topPx(t *testing.T, img image.Image) (int, bool) {
	t.Helper()
	box, found, err := NewContentTrim().Detect(context.Background(), img, Hints{})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	return int(box.Y*float64(img.Bounds().Dy()) + 0.5), found
}

func near(got, want, tol int) bool { return got >= want-tol && got <= want+tol }

func TestContentTrim_TrimsMargins(t *testing.T) {
	// Content block at rows 300..1000 on a tall white page; top should land just
	// above it (minus the ~0.5% pad).
	img := page(2000, span{300, 1000, 100, 900})
	top, found := topPx(t, img)
	if !found {
		t.Fatal("expected found=true")
	}
	if !near(top, 290, 4) { // 300 - padY(10)
		t.Fatalf("top = %d, want ~290", top)
	}
}

func TestContentTrim_SkipsBleedStrip(t *testing.T) {
	// A thin, sparse strip high in the bleed (rows 30..50), a wide gap, then the
	// masthead at row 130 — mimics the NYT registration-mark case. The strip
	// must be skipped so the top lands on the content.
	img := page(2000,
		span{30, 50, 200, 300},    // bleed strip: thin, sparse, top ~1.5%
		span{130, 1000, 100, 900}, // real content, wide
	)
	top, found := topPx(t, img)
	if !found {
		t.Fatal("expected found=true")
	}
	if !near(top, 120, 5) { // 130 - padY(10); NOT ~20
		t.Fatalf("top = %d, want ~120 (bleed strip skipped)", top)
	}
}

// Three of the four bleed guards (the fourth, bleed-zone position, is
// TestContentTrim_KeepsStripBelowBleedZone): each case violates one condition so
// the leading strip is treated as content and NOT skipped (top stays on the
// strip, ~row 30 - pad ~= 20).
func TestContentTrim_KeepsStrip(t *testing.T) {
	cases := []struct {
		name  string
		spans []span
	}{
		{"gap too small", []span{{30, 50, 200, 300}, {70, 1000, 100, 900}}},    // gap 20 < 30
		{"strip too dense", []span{{30, 50, 100, 900}, {130, 1000, 100, 900}}}, // peak 800 > 180
		{"strip too tall", []span{{30, 90, 200, 300}, {170, 1000, 100, 900}}},  // 60px > 24
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			top, found := topPx(t, page(2000, c.spans...))
			if !found {
				t.Fatal("expected found=true")
			}
			if !near(top, 20, 6) { // strip kept: 30 - padY(10)
				t.Fatalf("top = %d, want ~20 (strip NOT skipped)", top)
			}
		})
	}
}

func TestContentTrim_KeepsStripBelowBleedZone(t *testing.T) {
	// Strip starts at row 60 (3% of height) — below the 2.5% bleed zone — so it
	// is treated as content, not a printer's mark.
	img := page(2000, span{60, 80, 200, 300}, span{160, 1000, 100, 900})
	top, _ := topPx(t, img)
	if !near(top, 50, 6) { // 60 - padY(10)
		t.Fatalf("top = %d, want ~50 (strip below bleed zone kept)", top)
	}
}

func TestContentTrim_BlankPageIsNoOp(t *testing.T) {
	_, found := topPx(t, page(2000)) // all white
	if found {
		t.Fatal("blank page should return found=false")
	}
}

func TestContentTrim_BleedZoneScalesWithHeight(t *testing.T) {
	// The same absolute strip (rows 55..70) is 2.75% down a 2000px page — outside
	// the 2.5% bleed zone, so kept — but only 1.8% down a 3000px page — inside it,
	// so skipped. Confirms the guards are height-relative, not pixel-absolute.
	strip := span{55, 70, 200, 300}
	shortTop, _ := topPx(t, page(2000, strip, span{200, 1900, 100, 900}))
	if !near(shortTop, 45, 6) { // strip kept: 55 - padY(10)
		t.Fatalf("short-page top = %d, want ~45 (strip kept, below bleed zone)", shortTop)
	}
	tallTop, _ := topPx(t, page(3000, strip, span{200, 2900, 100, 900}))
	if !near(tallTop, 185, 6) { // strip skipped -> lands on content: 200 - padY(15)
		t.Fatalf("tall-page top = %d, want ~185 (strip skipped, inside bleed zone)", tallTop)
	}
}
