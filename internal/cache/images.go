package cache

import (
	"fmt"
	"os"
	"path/filepath"
)

// Images is the on-disk image cache rooted at a directory.
// Layout: <root>/<sourceID>/<YYYYMMDD>.png (and .pdf for the source).
type Images struct {
	Root string
}

// PNGPath returns the canonical path for a cached PNG.
func (i *Images) PNGPath(sourceID, yyyymmdd string) string {
	return filepath.Join(i.Root, sourceID, yyyymmdd+".png")
}

// PDFPath returns the canonical path for the source PDF.
func (i *Images) PDFPath(sourceID, yyyymmdd string) string {
	return filepath.Join(i.Root, sourceID, yyyymmdd+".pdf")
}

// EnsureDir creates the per-source directory if needed.
func (i *Images) EnsureDir(sourceID string) error {
	dir := filepath.Join(i.Root, sourceID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("cache: mkdir %s: %w", dir, err)
	}
	return nil
}

// Exists reports whether a cached file exists and is non-empty.
func Exists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Size() > 0
}
