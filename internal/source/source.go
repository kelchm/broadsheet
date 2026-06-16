// Package source defines newspaper sources and the default registry.
package source

// Source describes a newspaper feed.
//
// This mirrors paperboy.Source in the public API; we keep an internal copy
// so internal packages don't import the public package (the dependency arrow
// always points from public → internal, never back).
type Source struct {
	ID          string
	DisplayName string
	Prefix      string
	CropHints   CropHints
}

// CropHints carries per-source hints for the crop detector.
type CropHints struct {
	// MastheadText is the visible masthead string for OCR confirmation.
	MastheadText string
}
