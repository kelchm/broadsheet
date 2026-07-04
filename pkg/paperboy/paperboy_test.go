package paperboy

import (
	"image/color"
	"testing"

	"github.com/disintegration/imaging"
)

func TestMemCursors_AdvancePerDevice(t *testing.T) {
	c := newMemCursors()

	// One device cycles through the sources and wraps.
	got := []int{
		c.Next("a", 3), c.Next("a", 3), c.Next("a", 3), c.Next("a", 3),
	}
	want := []int{0, 1, 2, 0}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("device a sequence = %v, want %v", got, want)
		}
	}

	// A different device has an independent cursor.
	if i := c.Next("b", 3); i != 0 {
		t.Errorf("device b first index = %d, want 0 (independent)", i)
	}

	// n == 0 is guarded.
	if i := c.Next("a", 0); i != 0 {
		t.Errorf("Next with n=0 = %d, want 0", i)
	}
}

func TestCompose_Dimensions(t *testing.T) {
	page := imaging.New(400, 600, color.Gray{Y: 128}) // a portrait "page"
	const master = 400

	// Both w and h: output is exactly that canvas, for contain and cover.
	contain := compose(page, RenderOptions{OutputWidth: 300, OutputHeight: 500, Fit: FitContain}, master)
	if contain.Bounds().Dx() != 300 || contain.Bounds().Dy() != 500 {
		t.Errorf("contain w&h = %dx%d, want 300x500", contain.Bounds().Dx(), contain.Bounds().Dy())
	}
	if out := compose(page, RenderOptions{OutputWidth: 500, OutputHeight: 300, Fit: FitCover}, master); out.Bounds().Dx() != 500 || out.Bounds().Dy() != 300 {
		t.Errorf("cover w&h = %dx%d, want 500x300", out.Bounds().Dx(), out.Bounds().Dy())
	}

	// Width only: width matches; height follows the page aspect (no margin here).
	if out := compose(page, RenderOptions{OutputWidth: 200, MarginPct: -1}, master); out.Bounds().Dx() != 200 || out.Bounds().Dy() != 300 {
		t.Errorf("width-only = %dx%d, want 200x300", out.Bounds().Dx(), out.Bounds().Dy())
	}

	// contain (portrait page into a taller canvas) leaves a white margin — the
	// corner is background, not page ink.
	if r, g, b, _ := contain.At(2, 2).RGBA(); r>>8 < 250 || g>>8 < 250 || b>>8 < 250 {
		t.Errorf("expected white margin at corner, got r=%d g=%d b=%d", r>>8, g>>8, b>>8)
	}
}
