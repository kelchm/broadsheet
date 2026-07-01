// Package registry assembles the built-in source list, binding each paper to a
// concrete provider. It lives outside the source package so that source can
// stay a provider-free leaf (registry imports both source and the concrete
// providers; nothing imports registry except the engine).
package registry

import (
	"github.com/kelchm/paperboy/internal/provider/freedomforum"
	"github.com/kelchm/paperboy/internal/source"
)

// Default returns the built-in newspaper source registry.
//
// Prefixes come from https://www.freedomforum.org/todaysfrontpages/ — the
// per-paper code in the PDF URL, e.g. NY_NYT for The New York Times.
//
// Adding a paper is a single entry: give it an ID, a display name, and a
// provider. The reconciler, archive, renderer, and handlers need no other
// changes.
func Default() []source.Source {
	ff := func(id, name, prefix string) source.Source {
		return source.Source{
			ID:          id,
			DisplayName: name,
			Provider:    freedomforum.FreedomForum{Prefix: prefix},
			CropHints:   source.CropHints{MastheadText: name},
		}
	}
	return []source.Source{
		ff("dc-wp", "The Washington Post", "DC_WP"),
		ff("ma-bg", "The Boston Globe", "MA_BG"),
		ff("ny-nyt", "The New York Times", "NY_NYT"),
		ff("ca-lat", "Los Angeles Times", "CA_LAT"),
		ff("can-ts", "Toronto Star", "CAN_TS"),
		ff("ca-sfc", "San Francisco Chronicle", "CA_SFC"),
	}
}
