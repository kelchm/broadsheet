// Command paperboy-server runs the HTTP API for paperboy.
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

	"github.com/kelchm/paperboy/internal/buildinfo"
	"github.com/kelchm/paperboy/pkg/paperboy"
)

type envConfig struct {
	Port         int           `env:"PAPERBOY_PORT" envDefault:"8080"`
	DataDir      string        `env:"PAPERBOY_DATA_DIR" envDefault:"./data"`
	Width        int           `env:"PAPERBOY_WIDTH" envDefault:"1600"`
	LogLevel     string        `env:"PAPERBOY_LOG_LEVEL" envDefault:"info"`
	PollInterval time.Duration `env:"PAPERBOY_POLL_INTERVAL" envDefault:"30m"`
	ArchiveDays  int           `env:"PAPERBOY_ARCHIVE_DAYS" envDefault:"14"`
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version", "-v", "version":
			fmt.Println("paperboy-server", buildinfo.String())
			return
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

	p, err := paperboy.New(paperboy.Config{
		DataDir:      ec.DataDir,
		Width:        ec.Width,
		PollInterval: ec.PollInterval,
		ArchiveDays:  ec.ArchiveDays,
		Logger:       logger,
	})
	if err != nil {
		logger.Error("init paperboy", "err", err)
		os.Exit(1)
	}

	// Start the background mirror loop, tied to the process lifecycle.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	p.StartReconciler(ctx)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(loggingMW(logger))

	r.Get("/", handleDisplay)
	r.Get("/health", handleHealth)
	r.Get("/healthz", handleReadiness(p))
	r.Get("/sources", handleSources(p))
	r.Get("/current.png", handleCurrent(p))
	r.Get("/paper/{id}.png", handlePaper(p))

	addr := fmt.Sprintf(":%d", ec.Port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		logger.Info("paperboy-server listening", "addr", addr, "version", buildinfo.Version, "commit", buildinfo.Commit, "data_dir", ec.DataDir)
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
<title>paperboy</title>
<style>
  :root { --pad: 3vmin; }
  html, body { margin: 0; height: 100%; background: #fff; }
  body { box-sizing: border-box; padding: var(--pad); }
  .sheet { display: block; width: 100%; height: 100%; object-fit: contain; background: #fff; }
  body.cover .sheet { object-fit: cover; }
</style>
</head>
<body>
<img id="p" class="sheet" alt="">
<noscript><img class="sheet" src="/current.png" alt=""></noscript>
<script>
(function () {
  var q = new URLSearchParams(window.location.search);

  // Framing is done here in CSS, driven by the page URL:
  //   ?margin=<n>  -> padding, in vmin (default 3)
  //   ?fit=cover   -> object-fit: cover (crop to fill) instead of contain
  if (q.has('margin')) {
    document.documentElement.style.setProperty('--pad', (parseFloat(q.get('margin')) || 0) + 'vmin');
  }
  if (q.get('fit') === 'cover') document.body.classList.add('cover');

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

func handleReadiness(p *paperboy.Paperboy) http.HandlerFunc {
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
	ID          string                `json:"id"`
	DisplayName string                `json:"display_name"`
	Health      paperboy.SourceHealth `json:"health"`
}

func handleSources(p *paperboy.Paperboy) http.HandlerFunc {
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

func handleCurrent(p *paperboy.Paperboy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		opts, err := parseRenderOpts(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		opts.Device = deviceID(r)
		res, err := p.RenderCurrent(r.Context(), opts)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		writeImage(w, res)
	}
}

func handlePaper(p *paperboy.Paperboy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		opts, err := parseRenderOpts(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		res, err := p.RenderFor(r.Context(), id, opts)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeImage(w, res)
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
//	?sources=     comma-separated source IDs to rotate over (/current.png only).
//	?device=      device id for per-device rotation (/current.png only); if
//	              absent the client IP is used. Set on opts by the handler.
func parseRenderOpts(r *http.Request) (paperboy.RenderOptions, error) {
	q := r.URL.Query()
	var opts paperboy.RenderOptions

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
		switch paperboy.FitMode(fit) {
		case paperboy.FitContain, paperboy.FitCover:
			opts.Fit = paperboy.FitMode(fit)
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
		for _, id := range strings.Split(src, ",") {
			if id = strings.TrimSpace(id); id != "" {
				opts.Sources = append(opts.Sources, id)
			}
		}
	}
	return opts, nil
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

func writeImage(w http.ResponseWriter, res *paperboy.Result) {
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Paperboy-Source", res.SourceID)
	w.Header().Set("X-Paperboy-Days-Old", fmt.Sprintf("%d", res.DaysOld))
	w.Header().Set("X-Paperboy-Width", fmt.Sprintf("%d", res.Width))
	w.Header().Set("X-Paperboy-Height", fmt.Sprintf("%d", res.Height))
	if res.Stale {
		w.Header().Set("X-Paperboy-Stale", "true")
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
