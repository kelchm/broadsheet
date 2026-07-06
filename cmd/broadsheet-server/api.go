package main

// /api/v1 is the management plane: what the admin UI (and automation) talks
// to. JSON in and out, mutations optionally gated by a bearer token. Distinct
// from the device plane (rotation endpoints), which stays unauthenticated —
// panels can't do auth.

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/kelchm/broadsheet/internal/buildinfo"
	"github.com/kelchm/broadsheet/pkg/broadsheet"
)

// requireToken gates mutating management calls. With no token configured the
// gate is open (homelab default — the README says to set one before exposing
// the server beyond a trusted network).
func requireToken(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if token != "" {
				want, got := []byte("Bearer "+token), []byte(r.Header.Get("Authorization"))
				if subtle.ConstantTimeCompare(want, got) != 1 {
					http.Error(w, "missing or invalid bearer token", http.StatusUnauthorized)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

type apiHealth struct {
	LastPollOK    *time.Time `json:"last_poll_ok,omitempty"`
	LastEditionOK *time.Time `json:"last_edition_ok,omitempty"`
	LastErrorAt   *time.Time `json:"last_error_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
}

type apiSource struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Location string    `json:"location,omitempty"`
	Enabled  bool      `json:"enabled"`
	Health   apiHealth `json:"health"`
}

func toAPIHealth(h broadsheet.SourceHealth) apiHealth {
	return apiHealth{
		LastPollOK:    h.LastPollOK,
		LastEditionOK: h.LastFetchOK,
		LastErrorAt:   h.LastFetchError,
		LastError:     h.LastError,
	}
}

// GET /api/v1/sources — the full catalog with enabled flags and health.
func handleAPISources(p *broadsheet.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		cat, err := p.Catalog()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		health := p.HealthSnapshot().Sources
		out := make([]apiSource, 0, len(cat))
		for _, c := range cat {
			out = append(out, apiSource{
				ID: c.ID, Name: c.Name, Location: c.Location, Enabled: c.Enabled,
				Health: toAPIHealth(health[c.ID]),
			})
		}
		writeJSON(w, map[string]any{"sources": out})
	}
}

// PATCH /api/v1/sources/{id} — body {"enabled": bool}.
func handleAPIPatchSource(p *broadsheet.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Enabled *bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Enabled == nil {
			http.Error(w, `body must be {"enabled": true|false}`, http.StatusBadRequest)
			return
		}
		id := chi.URLParam(r, "id")
		if err := p.SetSourceEnabled(id, *body.Enabled); err != nil {
			writeEngineError(w, err)
			return
		}
		writeJSON(w, map[string]any{"id": id, "enabled": *body.Enabled})
	}
}

// POST /api/v1/sources/{id}/refresh — poll one source now. Returns the
// source's health afterwards; failures show up there (the poll itself is
// fire-and-observe, mirroring the reconciler's semantics).
func handleAPIRefresh(p *broadsheet.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if err := p.Refresh(r.Context(), id); err != nil {
			writeEngineError(w, err)
			return
		}
		writeJSON(w, map[string]any{
			"id":     id,
			"health": toAPIHealth(p.HealthSnapshot().Sources[id]),
		})
	}
}

// GET /api/v1/sources/{id}/editions — archived edition dates, oldest first.
func handleAPIEditions(p *broadsheet.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		dates, err := p.ListEditions(id)
		if err != nil {
			writeEngineError(w, err)
			return
		}
		out := make([]string, 0, len(dates))
		for _, d := range dates {
			out = append(out, d.UTC().Format("20060102"))
		}
		writeJSON(w, map[string]any{"id": id, "editions": out})
	}
}

// GET /api/v1/status — liveness plus a small operational summary.
func handleAPIStatus(p *broadsheet.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		enabled := len(p.ListSources())
		cat, err := p.Catalog()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		total := len(cat)
		writeJSON(w, map[string]any{
			"version":         buildinfo.Version,
			"commit":          buildinfo.Commit,
			"ready":           p.Ready(),
			"sources_enabled": enabled,
			"catalog_size":    total,
		})
	}
}

// GET /paper/{id}/{date}.png — a specific archived edition (device plane;
// pure read, ETag'd, long-lived cache since a dated edition rarely changes).
func handleEdition(p *broadsheet.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		opts, err := parseRenderOpts(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		date, err := time.Parse("20060102", chi.URLParam(r, "date"))
		if err != nil {
			http.Error(w, "invalid date (want YYYYMMDD)", http.StatusBadRequest)
			return
		}
		res, err := p.RenderEdition(r.Context(), chi.URLParam(r, "id"), date, opts)
		if err != nil {
			writeEngineError(w, err)
			return
		}
		w.Header().Set("ETag", res.ETag)
		// Short max-age + ETag revalidation: even a dated edition can change
		// when upstream re-posts a correction.
		w.Header().Set("Cache-Control", "public, max-age=300")
		if etagMatches(r.Header.Get("If-None-Match"), res.ETag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		writeImageBody(w, res)
	}
}
