// Command paperboy is the debug / ops CLI.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/caarlos0/env/v11"
	"github.com/spf13/cobra"

	"github.com/kelchm/paperboy/internal/buildinfo"
	"github.com/kelchm/paperboy/pkg/paperboy"
)

type cliEnv struct {
	DataDir string `env:"PAPERBOY_DATA_DIR" envDefault:"./data"`
	Width   int    `env:"PAPERBOY_WIDTH" envDefault:"1600"`
}

func main() {
	rootCmd := &cobra.Command{
		Use:     "paperboy",
		Short:   "Newspaper front-page rotator for e-ink displays",
		Version: buildinfo.String(),
		Long: `paperboy fetches newspaper front pages from freedomforum.org, rasterizes
them, and serves them on rotation.

This CLI is for ops/debug. The server lives in cmd/paperboy-server.`,
	}

	rootCmd.AddCommand(
		newListCmd(),
		newFetchCmd(),
		newHealthCmd(),
	)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func mustPaperboy() *paperboy.Paperboy {
	var ce cliEnv
	if err := env.Parse(&ce); err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(2)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	p, err := paperboy.New(paperboy.Config{
		DataDir: ce.DataDir,
		Width:   ce.Width,
		Logger:  logger,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "init paperboy:", err)
		os.Exit(1)
	}
	return p
}

func newListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List configured sources",
		Run: func(_ *cobra.Command, _ []string) {
			p := mustPaperboy()
			for _, s := range p.ListSources() {
				fmt.Printf("%-10s  %s\n", s.ID, s.DisplayName)
			}
		},
	}
}

func newFetchCmd() *cobra.Command {
	var outWidth int
	cmd := &cobra.Command{
		Use:   "fetch <source-id>",
		Short: "Poll a source into the archive and render its newest edition",
		Args:  cobra.ExactArgs(1),
		Run: func(_ *cobra.Command, args []string) {
			p := mustPaperboy()
			ctx := context.Background()
			if err := p.Refresh(ctx, args[0]); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			opts := paperboy.RenderOptions{OutputWidth: outWidth}
			res, err := p.RenderFor(ctx, args[0], opts)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			fmt.Printf("rendered %s (%d days old, %dx%d, %d bytes, edition=%s)\n",
				res.SourceID, res.DaysOld, res.Width, res.Height, len(res.Image),
				res.FetchedAt.Format("2006-01-02"))
		},
	}
	cmd.Flags().IntVarP(&outWidth, "width", "w", 0,
		"output width in pixels (default = master cache width)")
	return cmd
}

func newHealthCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "health",
		Short: "Print per-source health as JSON",
		Run: func(_ *cobra.Command, _ []string) {
			p := mustPaperboy()
			h := p.HealthSnapshot()
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(h)
		},
	}
}
