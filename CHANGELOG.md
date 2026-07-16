# Changelog

All notable changes to broadsheet are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project aims to
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## Unreleased

### Changed: HTML display pages now fit to width, top-aligned

The HTML display pages (`/rotation` for Visionect and the zero-config root `/`)
default to fitting the image to the viewport **width**, keeping the aspect ratio,
pinning the top of the page to the top of the viewport, and clipping whatever
runs past the bottom. This keeps the masthead and lead story large and legible on
panels whose aspect ratio differs from the paper's, instead of shrinking the whole
page to a letterboxed thumbnail.

- New `?fit=` values on the HTML pages: default (width, top-aligned),
  `?fit=contain` (the previous behavior — whole page, letterboxed), and
  `?fit=cover` (fill, center-crop). Pair with `?margin=0` to pin the image flush
  to the top-left corner. `fit=width` is client-side CSS only; the raw-image
  endpoint (`/rotation.png`) still frames with `contain`/`cover` server-side.
- The URL builder's Fit control offers all three for the Visionect target
  (defaulting to width) and `contain`/`cover` for the image/TRMNL targets.
- **Behavior change:** existing HTML displays render fit-to-width on upgrade.
  Add `?fit=contain` to the page URL to restore the previous letterboxed layout.

### Added: content-aware cropping

Served pages are now trimmed to their content bounds before framing
(`internal/crop`). The default `content-trim` detector removes whitespace
margins and steps over top-bleed printer's marks (registration / CMYK bars,
plate-ident codes) — safely: it only ever removes rows/columns with no content,
so it can never cut into the page. It does **not** yet remove ad or promo
skyboxes above the masthead; the crop seam is built so a smarter top-edge
detector plugs in later.

- On by default. `BROADSHEET_CROP=off` restores full, uncropped pages.
- The applied box is echoed in the `X-Broadsheet-Crop` response header and folded
  into the ETag (so a re-crop invalidates caches). A stored per-source
  `crop_overrides` box takes precedence over the auto-detector.
- **Behavior change:** existing deployments start serving cropped pages on
  upgrade. It's safe (whitespace and printer's-marks only); set
  `BROADSHEET_CROP=off` to keep full pages.

### Renamed: paperboy is now broadsheet

The project, module path (`github.com/kelchm/broadsheet`), binaries
(`broadsheet`, `broadsheet-server`), container image, env prefix
(`BROADSHEET_*`), response headers (`X-Broadsheet-*`), and database file
(`broadsheet.db`) are all renamed. The public engine type is now
`broadsheet.Engine` (was `paperboy.Paperboy`).

Upgrading an existing deployment:
- `PAPERBOY_*` env vars still work with a deprecation warning (removed at 1.0).
- `paperboy.db` is renamed to `broadsheet.db` automatically on first boot.
- Anything reading `X-Paperboy-*` response headers must switch to
  `X-Broadsheet-*` — no fallback is emitted.
- Pull images from `ghcr.io/kelchm/broadsheet`.

## [0.0.1] — Unreleased

Initial release — inspired by [newsprint](https://github.com/graiz/newsprint).

### Added
- HTTP server (`broadsheet-server`) exposing `/current.png`, `/paper/{id}.png`,
  `/sources`, `/health`, and `/healthz`.
- Debug/ops CLI (`broadsheet`) with `list`, `fetch`, and `health`, plus `--version`.
- Embeddable Go library (`pkg/broadsheet`): `RenderCurrent`, `RenderFor`,
  `StartReconciler`, `ListSources`, `HealthSnapshot`, `Ready`.
- Eager mirror architecture: a background reconciler keeps a local, multi-day
  PDF archive current; HTTP handlers are pure reads over the archive and never
  fetch. See [docs/architecture.md](docs/architecture.md).
- Graceful fallback with no "Not Found": serve the device's current source's
  newest archived edition, else the newest from any source (with
  `X-Broadsheet-Stale`), else 503 only on a cold, empty archive.
- A display page (`GET /`) that fills the viewport and frames the current paper
  in CSS — zero device config — for HTML-rendering displays (Visionect, browsers).
- Server-side sizing/framing on the image endpoints (`?w=`, `?h=`,
  `?fit=contain|cover`, `?margin=`) for devices that pull a raw, exactly-sized
  image. Never upscaled past the cached master.
- Per-device rotation that advances on each load: a device (identified by
  `?device=` or its IP) steps to the next paper every fetch, so its own refresh
  cadence sets the pace.
- Pluggable upstreams behind a typed `Provider` seam (`FreedomForum` first);
  editions carry a `MediaType`, so non-PDF sources are possible.
- Editions keyed by their real edition date (HTTP `Last-Modified`), fetched with
  conditional GET / ETags across a timezone-universal 3-folder window.
- Per-source health, surfaced via `/sources` and `/healthz`.
- Atomic on-disk writes throughout (fetch, archive, render).
- Configuration via `BROADSHEET_PORT`, `BROADSHEET_DATA_DIR`, `BROADSHEET_WIDTH`,
  `BROADSHEET_POLL_INTERVAL`, `BROADSHEET_ARCHIVE_DAYS`, and `BROADSHEET_LOG_LEVEL`.
- Production Docker image on `distroless/cc` (cgo build, no system PDF/vision
  libraries) and a cross-platform dev container.

[0.0.1]: https://github.com/kelchm/broadsheet/releases/tag/v0.0.1
