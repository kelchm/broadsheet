// Package source defines the core source/provider contracts.
//
// It's a leaf: just the shared types (Source, Provider, Edition, ...), with no
// concrete providers and no registry, so providers and the engine can import it
// without a cycle. Concrete providers live in internal/provider/*; the default
// registry that wires sources to providers lives in internal/registry.
package source

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// Source is a paper paperboy serves.
type Source struct {
	ID          string
	DisplayName string
	CropHints   CropHints

	// Provider knows how to acquire this source's editions. It is a typed value
	// that is both the provider's per-source config and its behavior.
	Provider Provider
}

// CropHints carries per-source hints for the crop detector. Carried through to
// the crop seam for a future masthead detector; the current passthrough crop
// ignores it.
type CropHints struct {
	// MastheadText is the visible masthead string.
	MastheadText string
}

// MediaType is the kind of artifact a provider returns. It tells the engine
// whether an edition must be rasterized (PDF) or can be treated as an already
// rendered image.
type MediaType string

// The media types a provider can return.
const (
	MediaPDF   MediaType = "application/pdf"
	MediaImage MediaType = "image/png"
)

// Edition is one fetched newspaper edition.
type Edition struct {
	// Date is the edition date. FreedomForum derives it from HTTP Last-Modified.
	Date time.Time
	// Version is an opaque change token (FreedomForum: the ETag). The engine
	// persists it and hands it back to the provider on the next poll; it never
	// interprets it.
	Version string
	// Media drives whether the engine rasterizes the bytes or uses them directly.
	Media MediaType
	// Data is the raw artifact (a PDF for FreedomForum).
	Data []byte
}

// Deps are shared runtime dependencies injected into a poll, so Provider values
// can stay pure configuration (many sources, one shared HTTP connection).
type Deps struct {
	HTTP   *http.Client
	Logger *slog.Logger
}

// Provider acquires editions for the sources it backs.
type Provider interface {
	// Poll returns editions that are new or changed since seen (the version
	// tokens persisted from the previous poll), plus the version tokens to
	// persist for next time. It may return no editions (nothing changed). The
	// returned versions map is authoritative: the engine stores exactly what is
	// returned.
	Poll(ctx context.Context, deps Deps, seen map[string]string, now time.Time) (
		editions []Edition, versions map[string]string, err error)
}

// ByID looks up a source by its ID. Returns nil if not found.
func ByID(sources []Source, id string) *Source {
	for i := range sources {
		if sources[i].ID == id {
			return &sources[i]
		}
	}
	return nil
}
