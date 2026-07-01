// Package freedomforum implements source.Provider for freedomforum.org's daily
// front-page archive.
//
// All of freedomforum's quirks are isolated here: the day-of-month URL scheme,
// the timezone-universal 3-folder probe window, conditional-GET/ETag freshness,
// and taking the edition date from HTTP Last-Modified. See docs/architecture.md.
package freedomforum

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/kelchm/paperboy/internal/buildinfo"
	"github.com/kelchm/paperboy/internal/source"
)

// baseURL is the CDN root. A package var (not a const) so tests can point the
// provider at an httptest server without a production config knob.
var baseURL = "https://cdn.freedomforum.org/dfp"

// FreedomForum backs sources hosted on freedomforum.org. The zero value is not
// usable; set Prefix to the paper's freedomforum code, e.g. "NY_NYT".
type FreedomForum struct {
	Prefix string
}

// url is the CDN URL for this source on the given day. The path carries only
// the day-of-month; the CDN keeps ~2 days live and 404s the rest.
func (f FreedomForum) url(day time.Time) string {
	return fmt.Sprintf("%s/pdf%d/%s.pdf", baseURL, day.Day(), f.Prefix)
}

// Poll probes the three day-of-month folders that could hold this source's
// current edition — UTC yesterday, today, and tomorrow — which is the smallest
// window that covers any newspaper's timezone (Earth spans UTC-12..UTC+14).
// Each probe is a conditional GET keyed by the previously-seen ETag, so an
// unchanged folder costs a 304 (no body) and a missing one a 404.
func (f FreedomForum) Poll(ctx context.Context, deps source.Deps, seen map[string]string, now time.Time) (
	[]source.Edition, map[string]string, error) {

	client := deps.HTTP
	if client == nil {
		client = http.DefaultClient
	}

	versions := make(map[string]string)
	var editions []source.Edition

	deltas := []int{-1, 0, 1} // yesterday, today, tomorrow (UTC)
	probeErrs := 0
	var lastErr error

	for _, delta := range deltas {
		day := now.UTC().AddDate(0, 0, delta)
		url := f.url(day)

		ed, etag, status, err := fetchConditional(ctx, client, url, seen[url])
		if err != nil {
			// A transient error on one folder must not sink the others; keep the
			// version we had so a later poll can still short-circuit.
			probeErrs++
			lastErr = err
			if deps.Logger != nil {
				deps.Logger.Debug("freedomforum probe failed", "url", url, "err", err)
			}
			if v, ok := seen[url]; ok {
				versions[url] = v
			}
			continue
		}

		switch status {
		case http.StatusOK:
			editions = append(editions, *ed)
			versions[url] = etag
		case http.StatusNotModified:
			versions[url] = seen[url] // unchanged; retain the token
		case http.StatusNotFound:
			// Nothing there — drop any stale token so we re-probe cleanly.
		default:
			if deps.Logger != nil {
				deps.Logger.Warn("freedomforum unexpected status", "url", url, "status", status)
			}
			if v, ok := seen[url]; ok {
				versions[url] = v
			}
		}
	}

	// Only a hard failure — every probe failed at the transport level (upstream
	// unreachable) — is an error. A mix of 200/304/404 is a healthy poll.
	if probeErrs == len(deltas) && lastErr != nil {
		return editions, versions, fmt.Errorf("freedomforum: all probes failed: %w", lastErr)
	}
	return editions, versions, nil
}

// fetchConditional issues a conditional GET. On 200 it returns the edition; on
// 304/404/other it returns a nil edition and the status for the caller to act
// on. etag is the token to send as If-None-Match (empty to skip).
func fetchConditional(ctx context.Context, client *http.Client, url, etag string) (
	*source.Edition, string, int, error) {

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", 0, fmt.Errorf("freedomforum: build request: %w", err)
	}
	req.Header.Set("User-Agent", buildinfo.UserAgent())
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, "", 0, fmt.Errorf("freedomforum: GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body) // allow connection reuse
		return nil, "", resp.StatusCode, nil
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", resp.StatusCode, fmt.Errorf("freedomforum: read body: %w", err)
	}
	newETag := resp.Header.Get("ETag")
	ed := &source.Edition{
		Date:    editionDate(resp.Header.Get("Last-Modified")),
		Version: newETag,
		Media:   source.MediaPDF,
		Data:    data,
	}
	return ed, newETag, resp.StatusCode, nil
}

// editionDate reads the edition date from Last-Modified. If it is missing or
// unparseable we fall back to a zero time; the caller keys the archive by the
// date's calendar day, and a zero date sorts oldest so a real edition always
// wins — a conservative failure mode.
func editionDate(lastModified string) time.Time {
	if lastModified == "" {
		return time.Time{}
	}
	t, err := http.ParseTime(lastModified)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}
