// Command broadsheet-server runs the HTTP API for broadsheet.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/kelchm/broadsheet/internal/buildinfo"
	"github.com/kelchm/broadsheet/pkg/broadsheet"
)

type envConfig struct {
	Port         int           `env:"BROADSHEET_PORT" envDefault:"8080"`
	DataDir      string        `env:"BROADSHEET_DATA_DIR" envDefault:"./data"`
	Width        int           `env:"BROADSHEET_WIDTH" envDefault:"1600"`
	LogLevel     string        `env:"BROADSHEET_LOG_LEVEL" envDefault:"info"`
	PollInterval time.Duration `env:"BROADSHEET_POLL_INTERVAL" envDefault:"30m"`
	ArchiveDays  int           `env:"BROADSHEET_ARCHIVE_DAYS" envDefault:"14"`
	// Crop trims each served page to its content bounds (safe: whitespace and
	// printer's marks only). "auto" (default) is on; "off" serves full pages.
	Crop string `env:"BROADSHEET_CROP" envDefault:"auto"`
	// AdminToken, when set, gates mutating /api/v1 calls behind
	// "Authorization: Bearer <token>". Empty = open (trusted-network default).
	AdminToken string `env:"BROADSHEET_ADMIN_TOKEN"`
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version", "-v", "version":
			fmt.Println("broadsheet-server", buildinfo.String())
			return
		}
	}

	// Pre-rename deployments set PAPERBOY_* — map any without a BROADSHEET_*
	// counterpart so an upgrade doesn't reset config, and warn below so the
	// operator migrates. The fallback goes away at 1.0.
	var legacyEnv []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PAPERBOY_") {
			continue
		}
		k, v, _ := strings.Cut(kv, "=")
		nk := "BROADSHEET_" + strings.TrimPrefix(k, "PAPERBOY_")
		if _, set := os.LookupEnv(nk); !set {
			_ = os.Setenv(nk, v)
			legacyEnv = append(legacyEnv, k)
		}
	}

	var ec envConfig
	if err := env.Parse(&ec); err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(2)
	}

	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(ec.LogLevel)); err != nil {
		lvl = slog.LevelInfo
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
	slog.SetDefault(logger)
	if len(legacyEnv) > 0 {
		logger.Warn("PAPERBOY_* environment variables are deprecated; rename them to BROADSHEET_*", "mapped", legacyEnv)
	}

	p, err := broadsheet.New(broadsheet.Config{
		DataDir:      ec.DataDir,
		Width:        ec.Width,
		PollInterval: ec.PollInterval,
		ArchiveDays:  ec.ArchiveDays,
		DisableCrop:  strings.EqualFold(ec.Crop, "off"),
		Logger:       logger,
	})
	if err != nil {
		logger.Error("init broadsheet", "err", err)
		os.Exit(1)
	}

	// Start the background mirror loop, tied to the process lifecycle.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	p.StartReconciler(ctx)

	addr := fmt.Sprintf(":%d", ec.Port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           newRouter(p, logger, ec.AdminToken),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		logger.Info("broadsheet-server listening", "addr", addr, "version", buildinfo.Version, "commit", buildinfo.Commit, "data_dir", ec.DataDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server stopped", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown error", "err", err)
	}
	if err := p.Close(); err != nil {
		logger.Error("close engine", "err", err)
	}
}

// newRouter assembles the real route table. Kept separate from main() so
// httptest can exercise exactly what production serves.
func newRouter(p *broadsheet.Engine, logger *slog.Logger, adminToken string) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(loggingMW(logger))

	// The time-driven device plane (see rotation.go): pure reads, ETag'd,
	// carrying forward-looking refresh hints.
	r.Get("/rotation", handleRotationPage(p))
	r.Get("/rotation.png", handleRotationPNG(p))
	r.Get("/api/display", handleTRMNLDisplay(p))

	// The admin UI (see ui.go): server-rendered pages + htmx fragments. Pages
	// are open reads; mutations accept the bearer token or the cookie planted
	// by visiting any page with ?token=.
	r.Route("/admin", func(ui chi.Router) {
		ui.Use(plantAdminCookie(adminToken))
		ui.Get("/", handleUIStatus(p))
		ui.Get("/papers", handleUIPapers(p))
		ui.Get("/builder", handleUIBuilder(p))
		ui.Get("/archive", handleUIArchive(p))
		ui.Get("/fragments/health", handleUIHealthFragment(p))
		ui.Group(func(mut chi.Router) {
			mut.Use(uiAuth(adminToken))
			mut.Post("/papers/{id}/toggle", handleUIToggle(p))
			mut.Post("/papers/{id}/refresh", handleUIRefresh(p))
		})
	})
	r.Handle("/static/*", http.StripPrefix("/static/", staticHandler()))

	// The management plane (see api.go): what the admin UI talks to. Reads are
	// open; mutations honor BROADSHEET_ADMIN_TOKEN when configured.
	r.Route("/api/v1", func(api chi.Router) {
		api.Get("/status", handleAPIStatus(p))
		api.Get("/sources", handleAPISources(p))
		api.Get("/sources/{id}/editions", handleAPIEditions(p))
		api.Group(func(mut chi.Router) {
			mut.Use(requireToken(adminToken))
			mut.Patch("/sources/{id}", handleAPIPatchSource(p))
			mut.Post("/sources/{id}/refresh", handleAPIRefresh(p))
		})
	})

	r.Get("/health", handleHealth)
	r.Get("/healthz", handleReadiness(p))
	r.Get("/sources", handleSources(p))
	r.Get("/paper/{id}.png", handlePaper(p))
	r.Get("/paper/{id}/{date}.png", handleEdition(p))

	// Deprecated: the advance-on-GET rotation. Each fetch mutates a per-device
	// cursor, which breaks HTTP caching and makes previews perturb displays.
	// Point displays at /rotation (HTML) or /rotation.png (raw) instead; these
	// remain for existing deployments and will go away before 1.0.
	r.Get("/", handleDisplay)
	r.Get("/current.png", handleCurrent(p))
	return r
}

// displayHTML is the zero-config "point your display here" page. It fills the
// viewport, detects its own size, and requests an exactly-fitted image — so a
// device never has to be told its dimensions. Any rotation params on the page
// URL (sources/interval/phase) pass through to the image request. It's static:
// all the per-device logic runs client-side from window.location.
const displayHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>broadsheet</title>
<style>
  :root { --pad: 3vmin; }
  html, body { margin: 0; height: 100%; background: #fff; }
  body { box-sizing: border-box; padding: var(--pad); overflow: hidden; }
  /* Default (no fit=): fill the viewport width, keep the aspect ratio, pin to
     the top, and clip whatever runs past the bottom edge. */
  .sheet { display: block; width: 100%; height: auto; background: #fff; }
  /* fit=contain: scale the whole page to fit inside the viewport, letterboxed. */
  body.contain .sheet { height: 100%; object-fit: contain; }
  /* fit=cover: fill the viewport, center-cropping the overflow. */
  body.cover .sheet { height: 100%; object-fit: cover; }
</style>
</head>
<body>
<img id="p" class="sheet" alt="">
<noscript><img class="sheet" src="/current.png" alt=""></noscript>
<script>
(function () {
  var q = new URLSearchParams(window.location.search);

  // Framing is done here in CSS, driven by the page URL:
  //   ?margin=<n>    -> padding, in vmin (default 3)
  //   default        -> fit to width, top-aligned, clipping the bottom overflow
  //   ?fit=contain   -> scale the whole page to fit inside, letterboxed
  //   ?fit=cover     -> fill the viewport, center-cropping the overflow
  if (q.has('margin')) {
    document.documentElement.style.setProperty('--pad', (parseFloat(q.get('margin')) || 0) + 'vmin');
  }
  var fit = q.get('fit');
  if (fit === 'contain' || fit === 'cover') document.body.classList.add(fit);

  // Fetch the image exactly once per load — each fetch advances this device's
  // rotation cursor server-side, so one fetch == one advance. We ask only for a
  // viewport-width source (no server framing: margin=0); CSS handles fit/margin,
  // and reflows on resize on its own, so we don't re-fetch on resize. sources
  // and device pass through from the page URL.
  var img = document.getElementById('p');
  function load() {
    var p = new URLSearchParams();
    ['sources', 'device'].forEach(function (k) { if (q.has(k)) p.set(k, q.get(k)); });
    p.set('w', Math.round(window.innerWidth * (window.devicePixelRatio || 1)));
    p.set('margin', '0');
    p.set('_', Date.now());
    img.src = '/current.png?' + p.toString();
  }
  load();

  // Optional auto-advance for always-on browsers: ?refresh=<seconds>.
  var refresh = parseInt(q.get('refresh') || '0', 10);
  if (refresh > 0) setInterval(load, refresh * 1000);
})();
</script>
</body>
</html>
`

func handleDisplay(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(displayHTML))
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func handleReadiness(p *broadsheet.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if p.Ready() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ready\n"))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready: no editions archived yet\n"))
	}
}

type sourcesResp struct {
	Sources []sourceRespEntry `json:"sources"`
}

type sourceRespEntry struct {
	ID          string                  `json:"id"`
	DisplayName string                  `json:"display_name"`
	Health      broadsheet.SourceHealth `json:"health"`
}

func handleSources(p *broadsheet.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		srcs := p.ListSources()
		h := p.HealthSnapshot()
		resp := sourcesResp{Sources: make([]sourceRespEntry, 0, len(srcs))}
		for _, s := range srcs {
			resp.Sources = append(resp.Sources, sourceRespEntry{
				ID: s.ID, DisplayName: s.DisplayName,
				Health: h.Sources[s.ID],
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

func handleCurrent(p *broadsheet.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		opts, err := parseRenderOpts(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		opts.Device = deviceID(r)
		res, err := p.RenderCurrent(r.Context(), opts)
		if err != nil {
			writeEngineError(w, err)
			return
		}
		// Advance-on-GET must never be cached: every fetch is a state change.
		w.Header().Set("Cache-Control", "no-store")
		writeImageBody(w, res)
	}
}

func handlePaper(p *broadsheet.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		opts, err := parseRenderOpts(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		res, err := p.RenderFor(r.Context(), id, opts)
		if err != nil {
			writeEngineError(w, err)
			return
		}
		// A pure read: cacheable briefly, revalidatable by ETag so pull clients
		// can conditional-GET and skip the repaint when nothing changed.
		w.Header().Set("ETag", res.ETag)
		w.Header().Set("Cache-Control", "public, max-age=300")
		if etagMatches(r.Header.Get("If-None-Match"), res.ETag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		writeImageBody(w, res)
	}
}

// parseRenderOpts extracts per-request rendering options from query params.
//
// Supported params:
//
//	?w=, ?h=      target canvas in pixels. Both -> fit onto that exact canvas;
//	              one -> the other follows the page aspect. The page is never
//	              upscaled past its master resolution.
//	?fit=         contain (default) or cover — only when both w and h are set.
//	?margin=      background border, percent of the shorter side. 0 disables it.
//	?sources=     comma-separated source IDs (rotation endpoints + /current.png).
//	              Unknown IDs are skipped; all-unknown is an error.
//	?device=      device id for per-device rotation (/current.png only); if
//	              absent the client IP is used. Set on opts by the handler.
func parseRenderOpts(r *http.Request) (broadsheet.RenderOptions, error) {
	q := r.URL.Query()
	var opts broadsheet.RenderOptions

	posInt := func(key string) (int, error) {
		s := q.Get(key)
		if s == "" {
			return 0, nil
		}
		v, err := strconv.Atoi(s)
		if err != nil || v <= 0 {
			return 0, fmt.Errorf("invalid %s=%q (want a positive integer)", key, s)
		}
		return v, nil
	}

	var err error
	if opts.OutputWidth, err = posInt("w"); err != nil {
		return opts, err
	}
	if opts.OutputHeight, err = posInt("h"); err != nil {
		return opts, err
	}
	if fit := q.Get("fit"); fit != "" {
		switch broadsheet.FitMode(fit) {
		case broadsheet.FitContain, broadsheet.FitCover:
			opts.Fit = broadsheet.FitMode(fit)
		default:
			return opts, fmt.Errorf("invalid fit=%q (want contain or cover)", fit)
		}
	}
	if ms := q.Get("margin"); ms != "" {
		v, err := strconv.ParseFloat(ms, 64)
		if err != nil || v < 0 {
			return opts, fmt.Errorf("invalid margin=%q (want a non-negative percent)", ms)
		}
		if v == 0 {
			opts.MarginPct = -1 // explicit "no margin"
		} else {
			opts.MarginPct = v
		}
	}
	if src := q.Get("sources"); src != "" {
		opts.Sources = splitSources(src)
	}
	return opts, nil
}

// splitSources parses a comma-separated source-ID list, trimming blanks.
func splitSources(s string) []string {
	var out []string
	for _, id := range strings.Split(s, ",") {
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// deviceID resolves the requester's rotation identity: an explicit ?device=
// wins (stable, user-chosen); otherwise the connecting IP (r.RemoteAddr) —
// zero-config on a LAN, where that's the device itself. We don't trust
// X-Forwarded-For, so behind a proxy set ?device= per display. The scheme
// prefix keeps strategies from colliding and leaves room for more (cookie,
// signed header, token) without touching callers.
func deviceID(r *http.Request) string {
	if d := strings.TrimSpace(r.URL.Query().Get("device")); d != "" {
		return "name:" + d
	}
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return "ip:" + host
}

// writeImageBody sets the X-Broadsheet-* metadata headers and writes the PNG.
// Caching headers (Cache-Control, ETag) are the caller's business — they
// differ per endpoint semantics.
func writeImageBody(w http.ResponseWriter, res *broadsheet.Result) {
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", strconv.Itoa(len(res.Image)))
	w.Header().Set("X-Broadsheet-Source", res.SourceID)
	w.Header().Set("X-Broadsheet-Days-Old", fmt.Sprintf("%d", res.DaysOld))
	w.Header().Set("X-Broadsheet-Width", fmt.Sprintf("%d", res.Width))
	w.Header().Set("X-Broadsheet-Height", fmt.Sprintf("%d", res.Height))
	if res.Stale {
		w.Header().Set("X-Broadsheet-Stale", "true")
	}
	if c := res.Crop; !c.IsEffectivelyFull() {
		// Normalized crop applied before framing: x,y,w,h (2 decimals).
		w.Header().Set("X-Broadsheet-Crop", fmt.Sprintf("%.2f,%.2f,%.2f,%.2f", c.X, c.Y, c.W, c.H))
	}
	_, _ = w.Write(res.Image) //nolint:gosec // G705: res.Image is server-rendered PNG bytes served as image/png, not user-controlled markup
}

func loggingMW(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)
			logger.Info("http",
				"method", r.Method,
				"path", r.URL.Path,
				"status", ww.Status(),
				"bytes", ww.BytesWritten(),
				"dur_ms", time.Since(start).Milliseconds(),
				"reqid", middleware.GetReqID(r.Context()),
			)
		})
	}
}
