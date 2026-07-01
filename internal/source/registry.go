package source

// Default returns the built-in newspaper source registry.
//
// Prefixes come from https://www.freedomforum.org/todaysfrontpages/ — the
// per-paper code in the PDF URL, e.g. NY_NYT for The New York Times.
//
// Adding a new paper: drop in a new Source{} entry. The fetcher and renderer
// need no other changes. CropHints is carried through to the crop seam for a
// future masthead detector; the current passthrough crop ignores it.
func Default() []Source {
	return []Source{
		{
			ID:          "dc-wp",
			DisplayName: "The Washington Post",
			Prefix:      "DC_WP",
			CropHints:   CropHints{MastheadText: "The Washington Post"},
		},
		{
			ID:          "ma-bg",
			DisplayName: "The Boston Globe",
			Prefix:      "MA_BG",
			CropHints:   CropHints{MastheadText: "The Boston Globe"},
		},
		{
			ID:          "ny-nyt",
			DisplayName: "The New York Times",
			Prefix:      "NY_NYT",
			CropHints:   CropHints{MastheadText: "The New York Times"},
		},
		{
			ID:          "ca-lat",
			DisplayName: "Los Angeles Times",
			Prefix:      "CA_LAT",
			CropHints:   CropHints{MastheadText: "Los Angeles Times"},
		},
		{
			ID:          "can-ts",
			DisplayName: "Toronto Star",
			Prefix:      "CAN_TS",
			CropHints:   CropHints{MastheadText: "Toronto Star"},
		},
		{
			ID:          "ca-sfc",
			DisplayName: "San Francisco Chronicle",
			Prefix:      "CA_SFC",
			CropHints:   CropHints{MastheadText: "San Francisco Chronicle"},
		},
	}
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
