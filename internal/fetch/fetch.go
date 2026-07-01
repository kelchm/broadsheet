// Package fetch downloads newspaper PDFs from upstream (freedomforum).
package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/kelchm/paperboy/internal/buildinfo"
)

// ErrNotFound is returned when the upstream returns 404 for a given source/date.
var ErrNotFound = errors.New("fetch: paper not found upstream")

// Fetcher knows how to download a newspaper PDF for a (prefix, day-of-month).
type Fetcher struct {
	Client    *http.Client
	UserAgent string
}

// New returns a Fetcher with sensible defaults.
func New() *Fetcher {
	return &Fetcher{
		Client:    &http.Client{Timeout: 30 * time.Second},
		UserAgent: buildinfo.UserAgent(),
	}
}

// URL is the freedomforum URL for a given source prefix on a given day.
// The path uses the day-of-month (1-31) as a folder name; the file at that
// location is overwritten each day, so we always pass today's day-of-month for
// today's paper, yesterday's for yesterday's, etc.
func URL(prefix string, day time.Time) string {
	return fmt.Sprintf(
		"https://cdn.freedomforum.org/dfp/pdf%d/%s.pdf",
		day.Day(), prefix,
	)
}

// Download fetches the PDF for the given prefix/day and writes it to dest.
// Returns ErrNotFound on 404.
func (f *Fetcher) Download(ctx context.Context, prefix string, day time.Time, dest string) error {
	url := URL(prefix, day)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("fetch: build request: %w", err)
	}
	req.Header.Set("User-Agent", f.UserAgent)

	resp, err := f.Client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch: GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// continue
	case http.StatusNotFound:
		return ErrNotFound
	default:
		return fmt.Errorf("fetch: %s returned %d", url, resp.StatusCode)
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("fetch: mkdir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".pdf-*.tmp")
	if err != nil {
		return fmt.Errorf("fetch: tmp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("fetch: copy: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("fetch: close: %w", err)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return fmt.Errorf("fetch: rename: %w", err)
	}
	return nil
}
