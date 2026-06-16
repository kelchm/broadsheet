package rasterize

import (
	"context"
	"fmt"
	"os/exec"
)

// MagickRasterizer renders PDFs by shelling out to ImageMagick's `magick`
// binary. Works without any CGo dependency; requires `magick` on PATH.
type MagickRasterizer struct {
	// Binary is the path to the ImageMagick CLI. Default "magick".
	Binary string
	// Density is the DPI to render at. Default 300.
	Density int
}

// NewMagick returns a MagickRasterizer with defaults.
func NewMagick() *MagickRasterizer {
	return &MagickRasterizer{Binary: "magick", Density: 300}
}

// Rasterize implements Rasterizer.
func (m *MagickRasterizer) Rasterize(ctx context.Context, pdfPath, pngPath string, width int) error {
	bin := m.Binary
	if bin == "" {
		bin = "magick"
	}
	density := m.Density
	if density == 0 {
		density = 300
	}

	// ImageMagick 7 requires input file to appear before image operators
	// (unlike IM6's `convert` which was looser about ordering). Settings
	// that affect reading (-density) come before the input filename.
	args := []string{
		"-density", fmt.Sprintf("%d", density),
		pdfPath + "[0]", // first page only
		"-background", "white",
		"-alpha", "remove",
		"-colorspace", "Gray",
		"-resize", fmt.Sprintf("%d", width),
		pngPath,
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("rasterize: magick failed: %w (output: %s)", err, string(out))
	}
	return nil
}
