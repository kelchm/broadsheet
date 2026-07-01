// Command paperboy-server runs the HTTP API for paperboy.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/kelchm/paperboy/internal/buildinfo"
	"github.com/kelchm/paperboy/pkg/paperboy"
)

type envConfig struct {
	Port     int    `env:"PAPERBOY_PORT" envDefault:"8080"`
	DataDir  string `env:"PAPERBOY_DATA_DIR" envDefault:"./data"`
	Width    int    `env:"PAPERBOY_WIDTH" envDefault:"1600"`
	LogLevel string `env:"PAPERBOY_LOG_LEVEL" envDefault:"info"`
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version", "-v", "version":
			fmt.Println("paperboy-server", buildinfo.Version)
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
		DataDir: ec.DataDir,
		Width:   ec.Width,
		Logger:  logger,
	})
	if err != nil {
		logger.Error("init paperboy", "err", err)
		os.Exit(1)
	}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(loggingMW(logger))

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
		logger.Info("paperboy-server listening", "addr", addr, "version", buildinfo.Version, "data_dir", ec.DataDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server stopped", "err", err)
			os.Exit(1)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	logger.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown error", "err", err)
	}
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func handleReadiness(p *paperboy.Paperboy) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		h := p.HealthSnapshot()
		for _, sh := range h.Sources {
			if sh.LastFetchOK != nil {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("ready\n"))
				return
			}
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready: no successful fetch yet\n"))
	}
}

type sourcesResp struct {
	Sources []sourceRespEntry `json:"sources"`
}

type sourceRespEntry struct {
	ID          string                `json:"id"`
	DisplayName string                `json:"display_name"`
	Prefix      string                `json:"prefix"`
	Health      paperboy.SourceHealth `json:"health"`
}

func handleSources(p *paperboy.Paperboy) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		srcs := p.ListSources()
		h := p.HealthSnapshot()
		resp := sourcesResp{Sources: make([]sourceRespEntry, 0, len(srcs))}
		for _, s := range srcs {
			resp.Sources = append(resp.Sources, sourceRespEntry{
				ID: s.ID, DisplayName: s.DisplayName, Prefix: s.Prefix,
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
		res, err := p.RenderNext(r.Context(), opts)
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
//	?w=<int>   output width in pixels; aspect ratio preserved.
//	           Capped at the master width (no upscaling).
func parseRenderOpts(r *http.Request) (paperboy.RenderOptions, error) {
	var opts paperboy.RenderOptions
	if ws := r.URL.Query().Get("w"); ws != "" {
		v, err := strconv.Atoi(ws)
		if err != nil || v <= 0 {
			return opts, fmt.Errorf("invalid w=%q (want positive integer)", ws)
		}
		opts.OutputWidth = v
	}
	return opts, nil
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
	_, _ = w.Write(res.Image)
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
