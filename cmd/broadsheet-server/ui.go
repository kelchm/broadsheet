package main

// The admin UI: server-rendered pages and htmx fragments over the same engine
// the JSON API uses. Hypermedia-first — server state is the only state; the
// few interactive behaviors (row toggle/refresh, health auto-refresh) are
// declarative htmx swaps of server-rendered fragments. htmx 2.0.9 is vendored
// via go:embed; there is no build step.

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/kelchm/broadsheet/internal/buildinfo"
	"github.com/kelchm/broadsheet/pkg/broadsheet"
)

//go:embed web/templates/*.html web/static/*
var webFS embed.FS

var uiTmpl = template.Must(template.ParseFS(webFS, "web/templates/*.html"))

// staticHandler serves the embedded assets with a day of caching — they only
// change with a new binary, and the binary restart busts nothing that matters.
func staticHandler() http.Handler {
	sub, err := fs.Sub(webFS, "web/static")
	if err != nil {
		panic(err) // embedded path; a failure is a build defect
	}
	files := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Assets only — no directory listings.
		if r.URL.Path == "" || strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		files.ServeHTTP(w, r)
	})
}

const adminCookie = "broadsheet_admin"

// cookieValue derives what the admin cookie stores: a digest, so the cookie
// jar never holds the reusable master secret itself.
func cookieValue(token string) string {
	sum := sha256.Sum256([]byte("broadsheet-admin:" + token))
	return hex.EncodeToString(sum[:])
}

// sameOriginOK rejects cross-site-shaped mutations (plain-form CSRF): browsers
// send Sec-Fetch-Site and/or Origin on cross-origin POSTs; non-browser clients
// send neither and pass. Defense-in-depth that also covers the no-token
// default, where there is no credential to forget to send.
func sameOriginOK(r *http.Request) bool {
	if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" && sfs != "same-origin" && sfs != "none" {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		if !strings.HasSuffix(origin, "://"+r.Host) {
			return false
		}
	}
	return true
}

// uiAuth gates mutating UI routes the same way the JSON API is gated, but
// browser-shaped: a bearer header still works, and so does a cookie planted by
// visiting any /admin page with ?token=<token> once. SameSite=Strict keeps
// cross-origin pages from riding the cookie (CSRF). No token configured = open
// (trusted-network default).
func uiAuth(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !sameOriginOK(r) {
				http.Error(w, "cross-origin mutation rejected", http.StatusForbidden)
				return
			}
			if token == "" {
				next.ServeHTTP(w, r)
				return
			}
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) == 1 {
				next.ServeHTTP(w, r)
				return
			}
			if c, err := r.Cookie(adminCookie); err == nil &&
				subtle.ConstantTimeCompare([]byte(c.Value), []byte(cookieValue(token))) == 1 {
				next.ServeHTTP(w, r)
				return
			}
			http.Error(w, "unauthorized: pass ?token= once to set the admin cookie, or send the bearer token", http.StatusUnauthorized)
		})
	}
}

// plantAdminCookie turns a valid ?token= query into the SameSite=Strict admin
// cookie. Applied to the (open) page routes so a bookmarked /admin?token=…
// enables the mutation buttons.
func plantAdminCookie(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if token != "" {
				if q := r.URL.Query().Get("token"); q != "" &&
					subtle.ConstantTimeCompare([]byte(q), []byte(token)) == 1 {
					// Secure only when the request arrived over TLS: forcing it
					// would silently break plain-HTTP homelab deployments. The
					// value is a digest, never the raw secret.
					http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is set when TLS is present; HttpOnly+SameSite always
						Name: adminCookie, Value: cookieValue(token), Path: "/admin",
						HttpOnly: true, SameSite: http.SameSiteStrictMode,
						Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
					})
					// Strip the secret out of the address bar, history, and any
					// fronting proxy's access-log request line.
					if r.Method == http.MethodGet {
						q := r.URL.Query()
						q.Del("token")
						target := r.URL.Path // relative: our own /admin route path, never a foreign host
						if enc := q.Encode(); enc != "" {
							target += "?" + enc
						}
						http.Redirect(w, r, target, http.StatusSeeOther) //nolint:gosec // G710: same-page relative redirect (path from our own matched route, no scheme/host)
						return
					}
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// paperRow is the view model for one catalog row (papers page + fragments).
type paperRow struct {
	ID, Name, Location string
	Search             string
	Enabled            bool
	LastPoll           string
	LastEdition        string
	Err                string
	ErrDetail          string
}

// ago humanizes a timestamp for the health tables.
func ago(t *time.Time) string {
	if t == nil {
		return ""
	}
	d := time.Since(*t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// paperRows assembles view models for the full catalog (or a single ID).
func paperRows(p *broadsheet.Engine, onlyID string) ([]paperRow, error) {
	cat, err := p.Catalog()
	if err != nil {
		return nil, err
	}
	health := p.HealthSnapshot().Sources
	rows := make([]paperRow, 0, len(cat))
	for _, c := range cat {
		if onlyID != "" && c.ID != onlyID {
			continue
		}
		h := health[c.ID]
		row := paperRow{
			ID: c.ID, Name: c.Name, Location: c.Location,
			Search:   strings.ToLower(c.Name + " " + c.Location + " " + c.ID),
			Enabled:  c.Enabled,
			LastPoll: ago(h.LastPollOK),
		}
		if h.LastFetchOK != nil {
			row.LastEdition = ago(h.LastFetchOK)
		}
		if h.LastError != "" {
			row.Err = "error"
			row.ErrDetail = h.LastError
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func renderUI(w http.ResponseWriter, name string, data any) {
	// Buffer first: streaming straight to the ResponseWriter commits a 200
	// before a template error can surface as a 500.
	var buf bytes.Buffer
	if err := uiTmpl.ExecuteTemplate(&buf, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Inline page scripts need 'unsafe-inline'; the win is blocking anything remote.
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self'; img-src 'self'")
	_, _ = w.Write(buf.Bytes())
}

func handleUIStatus(p *broadsheet.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		rows, err := enabledRows(p)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		cat, err := p.Catalog()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		renderUI(w, "page_status", map[string]any{
			"Version": buildinfo.Version, "Commit": buildinfo.Commit,
			"Ready": p.Ready(), "Enabled": len(p.ListSources()), "CatalogSize": len(cat),
			"Rows": rows,
		})
	}
}

func enabledRows(p *broadsheet.Engine) ([]paperRow, error) {
	all, err := paperRows(p, "")
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, r := range all {
		if r.Enabled {
			out = append(out, r)
		}
	}
	return out, nil
}

func handleUIHealthFragment(p *broadsheet.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		rows, err := enabledRows(p)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		renderUI(w, "health_table", rows)
	}
}

func handleUIPapers(p *broadsheet.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		rows, err := paperRows(p, "")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		enabled := 0
		for _, r := range rows {
			if r.Enabled {
				enabled++
			}
		}
		renderUI(w, "page_papers", map[string]any{
			"Rows": rows, "Enabled": enabled, "Total": len(rows),
		})
	}
}

func handleUIBuilder(p *broadsheet.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		rows, err := enabledRows(p)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		renderUI(w, "page_builder", map[string]any{"Enabled": rows})
	}
}

// renderRow re-renders one paper row after a mutation (the htmx swap target).
func renderRow(w http.ResponseWriter, p *broadsheet.Engine, id string) {
	rows, err := paperRows(p, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(rows) == 0 {
		http.Error(w, "unknown source", http.StatusNotFound)
		return
	}
	renderUI(w, "paper_row", rows[0])
}

func handleUIToggle(p *broadsheet.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// The rendered button carries the explicit target state (?to=), so a
		// double-click or a concurrent click is idempotent rather than a
		// read-modify-write race that flips twice.
		to := r.URL.Query().Get("to") == "true"
		id := chi.URLParam(r, "id")
		if err := p.SetSourceEnabled(id, to); err != nil {
			writeEngineError(w, err)
			return
		}
		renderRow(w, p, id)
	}
}

func handleUIRefresh(p *broadsheet.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if err := p.Refresh(r.Context(), id); err != nil {
			writeEngineError(w, err)
			return
		}
		renderRow(w, p, id)
	}
}

// archiveCell is one (paper, day) cell in the archive grid.
type archiveCell struct {
	ID   string
	Date string
	Has  bool
}

type archiveRow struct {
	ID, Name string
	Cells    []archiveCell
}

type archiveColumn struct {
	Label string
}

// handleUIArchive renders the sources x days grid over everything archived.
// Gaps are visible on purpose — "no edition that day" is information (holiday
// skips, fetch failures) the health page can't show historically.
func handleUIArchive(p *broadsheet.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		idx := p.ArchiveIndex()

		// Columns: the union of archived dates, newest first, capped to keep
		// the grid readable (retention bounds it anyway).
		const maxColumns = 21
		seen := map[string]bool{}
		var dates []string
		for _, ds := range idx {
			for _, d := range ds {
				k := d.UTC().Format("20060102")
				if !seen[k] {
					seen[k] = true
					dates = append(dates, k)
				}
			}
		}
		sort.Sort(sort.Reverse(sort.StringSlice(dates)))
		if len(dates) > maxColumns {
			dates = dates[:maxColumns]
		}

		names := map[string]string{}
		if cat, err := p.Catalog(); err == nil {
			for _, c := range cat {
				names[c.ID] = c.Name
			}
		}

		rows := make([]archiveRow, 0, len(idx))
		for id, ds := range idx {
			have := map[string]bool{}
			for _, d := range ds {
				have[d.UTC().Format("20060102")] = true
			}
			// Prefer the live catalog name; for a paper dropped from the catalog,
			// fall back to the name its archive was collected under; only then the
			// bare id.
			name := names[id]
			if name == "" {
				name = p.ArchiveName(id)
			}
			if name == "" {
				name = id
			}
			row := archiveRow{ID: id, Name: name}
			for _, col := range dates {
				row.Cells = append(row.Cells, archiveCell{ID: id, Date: col, Has: have[col]})
			}
			rows = append(rows, row)
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })

		cols := make([]archiveColumn, 0, len(dates))
		for _, d := range dates {
			if t, err := time.Parse("20060102", d); err == nil {
				cols = append(cols, archiveColumn{Label: t.Format("Jan 2")})
			} else {
				cols = append(cols, archiveColumn{Label: d})
			}
		}
		renderUI(w, "page_archive", map[string]any{"Rows": rows, "Columns": cols})
	}
}
