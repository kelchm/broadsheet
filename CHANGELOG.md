# Changelog

All notable changes to paperboy are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project aims to
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.0.1] — Unreleased

Initial release — inspired by [newsprint](https://github.com/graiz/newsprint).

### Added
- HTTP server (`paperboy-server`) exposing `/current.png`, `/paper/{id}.png`,
  `/sources`, `/health`, and `/healthz`.
- Debug/ops CLI (`paperboy`) with `list`, `fetch`, and `health`, plus `--version`.
- Embeddable Go library (`pkg/paperboy`): `RenderCurrent`, `RenderFor`,
  `StartReconciler`, `ListSources`, `HealthSnapshot`, `Ready`.
- Eager mirror architecture: a background reconciler keeps a local, multi-day
  PDF archive current; HTTP handlers are pure reads over the archive and never
  fetch. See [docs/architecture.md](docs/architecture.md).
- Graceful fallback with no "Not Found": serve the device's current source's
  newest archived edition, else the newest from any source (with
  `X-Paperboy-Stale`), else 503 only on a cold, empty archive.
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
- Configuration via `PAPERBOY_PORT`, `PAPERBOY_DATA_DIR`, `PAPERBOY_WIDTH`,
  `PAPERBOY_POLL_INTERVAL`, `PAPERBOY_ARCHIVE_DAYS`, and `PAPERBOY_LOG_LEVEL`.
- Production Docker image on `distroless/cc` (cgo build, no system PDF/vision
  libraries) and a cross-platform dev container.

[0.0.1]: https://github.com/kelchm/paperboy/releases/tag/v0.0.1
