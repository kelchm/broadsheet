package render

import (
	"context"
	"image/color"
	"path/filepath"
	"testing"

	"github.com/disintegration/imaging"

	"github.com/kelchm/paperboy/internal/source"
)

func TestRenderImageResizesToWidth(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "in.png")
	if err := imaging.Save(imaging.New(200, 100, color.NRGBA{200, 200, 200, 255}), src); err != nil {
		t.Fatalf("save fixture: %v", err)
	}

	dst := filepath.Join(dir, "out.png")
	if err := New().Render(context.Background(), src, source.MediaImage, dst, 80); err != nil {
		t.Fatalf("Render: %v", err)
	}

	out, err := imaging.Open(dst)
	if err != nil {
		t.Fatalf("open output: %v", err)
	}
	if got := out.Bounds().Dx(); got != 80 {
		t.Errorf("output width = %d, want 80 (aspect preserved)", got)
	}
	if got := out.Bounds().Dy(); got != 40 {
		t.Errorf("output height = %d, want 40", got)
	}
}

func TestRenderUnsupportedMedia(t *testing.T) {
	if err := New().Render(context.Background(), "x", source.MediaType("weird"), "y", 10); err == nil {
		t.Error("expected error for unsupported media")
	}
}

func TestRenderPDFDispatchesToRasterizer(t *testing.T) {
	// End-to-end through the real fitz rasterizer against the checked-in
	// fixture, so the MediaPDF branch is exercised, not just MediaImage.
	dst := filepath.Join(t.TempDir(), "out.png")
	err := New().Render(context.Background(),
		"../rasterize/testdata/one-page.pdf", source.MediaPDF, dst, 120)
	if err != nil {
		t.Fatalf("Render(MediaPDF): %v", err)
	}
	out, err := imaging.Open(dst)
	if err != nil {
		t.Fatalf("open output: %v", err)
	}
	if got := out.Bounds().Dx(); got != 120 {
		t.Errorf("output width = %d, want 120", got)
	}
}
