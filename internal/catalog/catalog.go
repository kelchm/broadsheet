// Package catalog ships the built-in list of known papers as embedded data.
// The catalog is what a user browses and toggles; their enabled set lives in
// the store (seeded from the catalog's defaults on first boot). Entries are
// pure data — the registry decodes provider type + config into behavior.
package catalog

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
)

//go:embed catalog.json
var raw []byte

// Entry is one catalog paper.
type Entry struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Provider string          `json:"provider"`
	Config   json.RawMessage `json:"config"`
	// Location is a human-readable place ("Seattle, WA"; "Canada") for the
	// catalog browser.
	Location string `json:"location,omitempty"`
	// Default marks the papers enabled on a fresh install.
	Default bool `json:"default,omitempty"`
}

var (
	once    sync.Once
	entries []Entry
	parseEr error
)

// All returns the embedded catalog. The data is compile-time constant; a parse
// failure is a build defect surfaced on first use.
func All() ([]Entry, error) {
	once.Do(func() {
		parseEr = json.Unmarshal(raw, &entries)
		if parseEr == nil && len(entries) == 0 {
			parseEr = fmt.Errorf("catalog: embedded catalog is empty")
		}
	})
	return entries, parseEr
}
