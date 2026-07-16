// Package washingtonpost implements source.Provider for The Washington Post's
// daily print edition, served as per-page PDFs from its CloudFront CDN.
//
// Unlike freedomforum, the Post is not a day-of-month archive: each edition
// lives in a full-date folder (YYYYMMDD) and the front page's filename carries a
// zone code that rotates day to day between a small set (observed: SU or RE),
// with the edition/product codes stable (EZ/DAILY). WaPo publishes exactly one
// of those zones per day, so a poll probes the zone candidates against the open
// CDN and takes whichever one exists — the "today's paper" HTML page that lists
// the exact URL is Akamai bot-protected and unreliable for a headless poller,
// while the CDN itself is open and honors conditional GET. Because the folder is
// the full edition date, that date is authoritative — there's no Last-Modified
// guessing. See docs/architecture.md.
package washingtonpost

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/kelchm/broadsheet/internal/buildinfo"
	"github.com/kelchm/broadsheet/internal/source"
)

// baseURL is the CloudFront distribution root. A package var (not a const) so
// tests can point the provider at an httptest server, mirroring freedomforum.
var baseURL = "https://dedq39jz5ilmb.cloudfront.net"

// Filename defaults. Only the zone rotates across days; the rest have held
// constant across every observed edition (weekdays, weekends, and holidays
// alike). They are still overridable via config so a scheme tweak upstream can
// be absorbed by editing the catalog rather than shipping a new binary.
const (
	defaultPage    = "A01" // front page (section A, page 1)
	defaultEdition = "EZ"
	defaultProduct = "DAILY"
)

// defaultZones are the front-page zone codes to probe. WaPo publishes exactly
// one per day; which one rotates for reasons that don't track the weekday, so
// both are always tried.
var defaultZones = []string{"SU", "RE"}

// WashingtonPost backs the Post's front page. The zero value is usable — Poll
// fills blank fields with the defaults above — and the fields are exported so
// the registry can decode a config override into them.
type WashingtonPost struct {
	Page    string   `json:"page,omitempty"`
	Zones   []string `json:"zones,omitempty"`
	Edition string   `json:"edition,omitempty"`
	Product string   `json:"product,omitempty"`
}

// New returns a fully-defaulted provider. Passing zero values selects the
// defaults for each field.
func New(page string, zones []string, edition, product string) WashingtonPost {
	return WashingtonPost{Page: page, Zones: zones, Edition: edition, Product: product}.defaulted()
}

// defaulted returns a copy with blank fields filled from the defaults. Keeping
// this the single source of defaults means a config that omits a field and a
// caller that constructs the zero value land on the same behavior.
func (w WashingtonPost) defaulted() WashingtonPost {
	if w.Page == "" {
		w.Page = defaultPage
	}
	if len(w.Zones) == 0 {
		w.Zones = defaultZones
	}
	if w.Edition == "" {
		w.Edition = defaultEdition
	}
	if w.Product == "" {
		w.Product = defaultProduct
	}
	return w
}

// url is the CDN URL for one day/zone, e.g.
// https://<dist>/20260713/A01_SU_EZ_DAILY_20260713.pdf. The date appears twice:
// as the folder and inside the filename, both the edition's calendar date.
func (w WashingtonPost) url(day time.Time, zone string) string {
	d := day.UTC().Format("20060102")
	return fmt.Sprintf("%s/%s/%s_%s_%s_%s_%s.pdf", baseURL, d, w.Page, zone, w.Edition, w.Product, d)
}

// Poll probes each candidate (day, zone) for the current edition. The day window
// is UTC yesterday/today/tomorrow — the same belt-and-suspenders span
// freedomforum uses; here it covers the pre-dawn window before a fresh edition
// posts (~00:15 ET) plus any clock skew, since the Post's folder date is always
// UTC-today or UTC-yesterday. Each probe is a conditional GET keyed by the
// previously-seen ETag, so an unchanged file costs a 304 and a nonexistent one a
// 403 (S3 AccessDenied) or 404.
func (w WashingtonPost) Poll(ctx context.Context, deps source.Deps, seen map[string]string, now time.Time) (
	[]source.Edition, map[string]string, error) {

	w = w.defaulted()

	client := deps.HTTP
	if client == nil {
		client = http.DefaultClient
	}

	versions := make(map[string]string)
	var editions []source.Edition

	deltas := []int{-1, 0, 1} // yesterday, today, tomorrow (UTC)
	probes := 0
	probeErrs := 0
	var lastErr error

	for _, delta := range deltas {
		day := now.UTC().AddDate(0, 0, delta)
		for _, zone := range w.Zones {
			url := w.url(day, zone)
			probes++

			ed, etag, status, err := fetchConditional(ctx, client, url, seen[url], day)
			if err != nil {
				// A transient error on one probe must not sink the others; keep the
				// version we had so a later poll can still short-circuit.
				probeErrs++
				lastErr = err
				if deps.Logger != nil {
					deps.Logger.Debug("washingtonpost probe failed", "url", url, "err", err)
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
			case http.StatusForbidden, http.StatusNotFound:
				// Nothing there (S3 returns 403 for a missing key, CloudFront may
				// 404) — drop any stale token so we re-probe cleanly.
			default:
				// An unexpected status (typically an upstream 5xx) is a failed
				// probe, not a determinate answer: count it toward the failure gate
				// so an all-error poll surfaces as a failure instead of a silent
				// healthy no-op, and retain the token so we retry next cycle.
				probeErrs++
				lastErr = fmt.Errorf("washingtonpost: %s returned unexpected status %d", url, status)
				if deps.Logger != nil {
					deps.Logger.Warn("washingtonpost unexpected status", "url", url, "status", status)
				}
				if v, ok := seen[url]; ok {
					versions[url] = v
				}
			}
		}
	}

	// Only a hard failure — every probe failed at the transport level (upstream
	// unreachable) — is an error. A mix of 200/304/403 is a healthy poll.
	if probeErrs == probes && lastErr != nil {
		return editions, versions, fmt.Errorf("washingtonpost: all probes failed: %w", lastErr)
	}
	return editions, versions, nil
}

// fetchConditional issues a conditional GET. On 200 it returns the edition; on
// 304/403/404/other it returns a nil edition and the status for the caller to
// act on. etag is the token to send as If-None-Match (empty to skip). day is the
// folder date being probed, which is the edition's calendar date.
func fetchConditional(ctx context.Context, client *http.Client, url, etag string, day time.Time) (
	*source.Edition, string, int, error) {

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", 0, fmt.Errorf("washingtonpost: build request: %w", err)
	}
	req.Header.Set("User-Agent", buildinfo.UserAgent())
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, "", 0, fmt.Errorf("washingtonpost: GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body) // allow connection reuse
		return nil, "", resp.StatusCode, nil
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", resp.StatusCode, fmt.Errorf("washingtonpost: read body: %w", err)
	}
	// A 200 whose body isn't a PDF (an HTML/XML error page slipping through with
	// a 200, a captive portal, an empty body) must not become an edition — and
	// must not burn the ETag, or the next poll 304s past the real file forever.
	// Treat it as a probe error so the caller retains the old token and retries
	// next cycle. Per the PDF spec's reader tolerance the header may sit anywhere
	// in the first 1024 bytes, so sniff that window rather than byte 0 only.
	head := data
	if len(head) > 1024 {
		head = head[:1024]
	}
	if !bytes.Contains(head, []byte("%PDF")) {
		return nil, "", resp.StatusCode, fmt.Errorf("washingtonpost: %s returned %d bytes that are not a PDF", url, len(data))
	}
	newETag := resp.Header.Get("ETag")
	ed := &source.Edition{
		Date:    editionDate(day),
		Version: newETag,
		Media:   source.MediaPDF,
		Data:    data,
	}
	return ed, newETag, resp.StatusCode, nil
}

// editionDate is the folder's calendar date at day precision in UTC. Unlike
// freedomforum, the Post's URL carries the full date, so this is exact rather
// than a Last-Modified reading — and never zero, which the archive rejects.
func editionDate(day time.Time) time.Time {
	d := day.UTC()
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
}
