# Changelog

All notable changes to paperboy are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project aims to
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.0.1] — Unreleased

Initial release — a Go rewrite of [newsprint](https://github.com/kelchm/newsprint).

### Added
- HTTP server (`paperboy-server`) exposing `/current.png`, `/paper/{id}.png`,
  `/sources`, `/health`, and `/healthz`.
- Debug/ops CLI (`paperboy`) with `list`, `fetch`, and `health`, plus `--version`.
- Embeddable Go library (`pkg/paperboy`): `RenderCurrent`, `RenderFor`,
  `StartReconciler`, `ListSources`, `HealthSnapshot`, `Ready`.
- Eager mirror architecture: a background reconciler keeps a local, multi-day
  PDF archive current; HTTP handlers are pure reads over the archive and never
  fetch. See [docs/architecture.md](docs/architecture.md).
- Graceful fallback with no "Not Found": serve the rotation slot's newest
  archived edition, else the newest from any source (with `X-Paperboy-Stale`),
  else 503 only on a cold, empty archive.
- Deterministic, time-based `/current.png` rotation — a safe read that does not
  mutate state.
- Pluggable upstreams behind a typed `Provider` seam (`FreedomForum` first);
  editions carry a `MediaType`, so non-PDF sources are possible.
- Editions keyed by their real edition date (HTTP `Last-Modified`), fetched with
  conditional GET / ETags across a timezone-universal 3-folder window.
- Client-controlled output width via `?w=`, resized down from a cached master
  render (upscaling rejected).
- Per-source health, surfaced via `/sources` and `/healthz`.
- Atomic on-disk writes throughout (fetch, archive, render).
- Configuration via `PAPERBOY_PORT`, `PAPERBOY_DATA_DIR`, `PAPERBOY_WIDTH`,
  `PAPERBOY_POLL_INTERVAL`, `PAPERBOY_ROTATE_INTERVAL`, `PAPERBOY_ARCHIVE_DAYS`,
  and `PAPERBOY_LOG_LEVEL`.
- Production Docker image on `distroless/cc` (cgo build, no system PDF/vision
  libraries) and a cross-platform dev container.

[0.0.1]: https://github.com/kelchm/paperboy/releases/tag/v0.0.1
