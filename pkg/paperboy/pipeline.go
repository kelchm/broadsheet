package paperboy

import (
	"context"
	"time"

	"github.com/kelchm/paperboy/internal/cache"
	"github.com/kelchm/paperboy/internal/crop"
	"github.com/kelchm/paperboy/internal/fetch"
	"github.com/kelchm/paperboy/internal/rasterize"
	"github.com/kelchm/paperboy/internal/source"
)

// pipelineFetcher implements rotation.Fetcher by walking the
// fetch → rasterize → crop → cache pipeline for a single (source, daysAgo).
//
// Each step short-circuits if the artifact is already on disk: we never
// re-download a PDF we already have, and we never re-rasterize a PNG that
// already exists. This makes repeated calls cheap and the on-disk cache the
// source of truth.
type pipelineFetcher struct {
	images     *cache.Images
	fetcher    *fetch.Fetcher
	rasterizer rasterize.Rasterizer
	cropper    crop.Cropper
	width      int
}

// FetchAndRender implements rotation.Fetcher.
func (pf *pipelineFetcher) FetchAndRender(ctx context.Context, src source.Source, daysAgo int) (string, time.Time, error) {
	day := time.Now().UTC().AddDate(0, 0, -daysAgo)
	yyyymmdd := day.Format("20060102")

	pngPath := pf.images.PNGPath(src.ID, yyyymmdd)
	if cache.Exists(pngPath) {
		return pngPath, day, nil
	}

	if err := pf.images.EnsureDir(src.ID); err != nil {
		return "", time.Time{}, err
	}

	pdfPath := pf.images.PDFPath(src.ID, yyyymmdd)
	if !cache.Exists(pdfPath) {
		if err := pf.fetcher.Download(ctx, src.Prefix, day, pdfPath); err != nil {
			return "", time.Time{}, err
		}
	}

	if err := pf.rasterizer.Rasterize(ctx, pdfPath, pngPath, pf.width); err != nil {
		return "", time.Time{}, err
	}

	hints := crop.Hints{MastheadText: src.CropHints.MastheadText}
	if _, err := pf.cropper.Crop(ctx, pngPath, pngPath, hints); err != nil {
		return "", time.Time{}, err
	}

	return pngPath, day, nil
}
