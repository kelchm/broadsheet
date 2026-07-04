package rasterize

import (
	"bytes"
	"context"
	"image"
	_ "image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/gen2brain/go-fitz"
)

const fixture = "testdata/one-page.pdf"

// TestRasterize_FixturePDF exercises the real MuPDF path end to end: open,
// bounds-derived DPI, render, grayscale, resize, atomic write. This is the one
// test that executes the natively-linked library in CI (it has broken before —
// see the glibc>=2.38 fix in git history), so keep it hermetic and fast.
func TestRasterize_FixturePDF(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out.png")
	const width = 400

	if err := NewFitz().Rasterize(context.Background(), fixture, out, width); err != nil {
		t.Fatalf("Rasterize: %v", err)
	}

	data, err := os.ReadFile(out) //nolint:gosec // G304: t.TempDir()-derived path
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if format != "png" {
		t.Errorf("format = %q, want png", format)
	}
	b := img.Bounds()
	if b.Dx() != width {
		t.Errorf("width = %d, want %d", b.Dx(), width)
	}
	// US Letter (612x792pt) at width 400 -> height ~518.
	if b.Dy() < 500 || b.Dy() > 540 {
		t.Errorf("height = %d, want ~518 (aspect preserved)", b.Dy())
	}

	// Grayscale output with real ink: the fixture draws a black band across the
	// top, so the top-center pixel must be dark and gray (R==G==B), and the
	// page background near the middle-left must be light.
	darkR, darkG, darkB, _ := img.At(width/2, b.Dy()/20).RGBA()
	if darkR != darkG || darkG != darkB {
		t.Errorf("output is not grayscale: got RGB %d,%d,%d", darkR>>8, darkG>>8, darkB>>8)
	}
	if darkR>>8 > 64 {
		t.Errorf("top band luminance = %d, want dark (the fixture's black band)", darkR>>8)
	}
	lightR, _, _, _ := img.At(width/20, b.Dy()/2).RGBA()
	if lightR>>8 < 192 {
		t.Errorf("background luminance = %d, want light", lightR>>8)
	}
}

func TestRasterize_CanceledContextStopsEarly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := filepath.Join(t.TempDir(), "out.png")
	if err := NewFitz().Rasterize(ctx, fixture, out, 400); err == nil {
		t.Fatal("Rasterize with canceled context should error")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Error("no output should be written on cancellation")
	}
}

func TestDeriveDPI(t *testing.T) {
	doc, err := openFixture()
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer func() { _ = doc.Close() }()

	// Letter is 612pt wide: dpi = 72 * 2*width / 612.
	if got := deriveDPI(doc, 1600); got != 300 {
		// 72*3200/612 = 376.5 -> clamped to 300
		t.Errorf("deriveDPI(1600) = %v, want clamped 300", got)
	}
	if got := deriveDPI(doc, 400); got < 90 || got > 100 {
		// 72*800/612 = 94.1
		t.Errorf("deriveDPI(400) = %v, want ~94", got)
	}
	if got := deriveDPI(doc, 0); got != 300 {
		t.Errorf("deriveDPI(0) = %v, want fallback 300", got)
	}
}

func openFixture() (*fitz.Document, error) { return fitz.New(fixture) }
