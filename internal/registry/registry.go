// Package registry turns source *data* into source *behavior*: it decodes a
// provider type + JSON config (from the embedded catalog or the store) into a
// typed provider value. It lives outside the source package so that source can
// stay a provider-free leaf (registry imports both source and the concrete
// providers; nothing imports registry except the engine).
package registry

import (
	"encoding/json"
	"fmt"

	"github.com/kelchm/broadsheet/internal/catalog"
	"github.com/kelchm/broadsheet/internal/provider/freedomforum"
	"github.com/kelchm/broadsheet/internal/source"
)

// Decode instantiates a typed provider from its stored representation. This is
// the params-decoding seam that lets sources live as rows instead of Go code:
// adding a provider means one more case here plus its package.
func Decode(providerType string, config json.RawMessage) (source.Provider, error) {
	switch providerType {
	case "freedomforum":
		var c struct {
			Prefix string `json:"prefix"`
		}
		if len(config) > 0 {
			if err := json.Unmarshal(config, &c); err != nil {
				return nil, fmt.Errorf("registry: freedomforum config: %w", err)
			}
		}
		if c.Prefix == "" {
			return nil, fmt.Errorf("registry: freedomforum config needs a prefix")
		}
		return freedomforum.FreedomForum{Prefix: c.Prefix}, nil
	default:
		return nil, fmt.Errorf("registry: unknown provider type %q", providerType)
	}
}

// Build assembles a Source from its data form.
func Build(id, displayName, providerType string, config json.RawMessage) (source.Source, error) {
	p, err := Decode(providerType, config)
	if err != nil {
		return source.Source{}, fmt.Errorf("%w (source %s)", err, id)
	}
	return source.Source{
		ID:          id,
		DisplayName: displayName,
		Provider:    p,
		CropHints:   source.CropHints{MastheadText: displayName},
	}, nil
}

// Default returns the catalog's default-enabled papers as ready sources — a
// zero-config convenience for embedders. (The engine seeds its store from the
// catalog directly; this helper isn't on that path.)
func Default() []source.Source {
	entries, err := catalog.All()
	if err != nil {
		// The catalog is embedded, compile-time data; failing to parse it is a
		// build defect, not a runtime condition.
		panic(err)
	}
	var out []source.Source
	for _, e := range entries {
		if !e.Default {
			continue
		}
		s, err := Build(e.ID, e.Name, e.Provider, e.Config)
		if err != nil {
			panic(err)
		}
		out = append(out, s)
	}
	return out
}
