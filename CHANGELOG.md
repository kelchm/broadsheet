# Changelog

All notable changes to paperboy are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project aims to
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.0.1] - 2026-06-30

Initial release — a Go rewrite of [newsprint](https://github.com/kelchm/newsprint).

### Added
- HTTP server (`paperboy-server`) exposing `/current.png`, `/paper/{id}.png`,
  `/sources`, `/health`, and `/healthz`.
- Debug/ops CLI (`paperboy`) with `list`, `fetch`, and `health` subcommands, plus
  `--version`.
- Embeddable Go library (`pkg/paperboy`) with `RenderNext`, `RenderFor`,
  `ListSources`, and `HealthSnapshot`.
- Cross-source graceful fallback: today → yesterday → 2 days ago → next source →
  most recent cached image, so the server never serves "Not Found".
- Client-controlled output width via the `?w=` query param, resized down from a
  cached master render (upscaling past the master is rejected).
- Per-source health tracking, surfaced via `/sources` and `/healthz` and recorded
  for both rotation and direct `/paper/{id}` traffic.
- Atomic on-disk cache writes (fetch and rasterize both write tmp + rename).
- Production Docker image on `distroless/cc` and a cross-platform dev container.

[Unreleased]: https://github.com/kelchm/paperboy/compare/v0.0.1...HEAD
[0.0.1]: https://github.com/kelchm/paperboy/releases/tag/v0.0.1
