// Package buildinfo carries the paperboy build version, shared by the binaries,
// the embeddable library, and the upstream User-Agent so they never drift.
package buildinfo

// These are overridden at build time via -ldflags -X (see docker/Dockerfile).
// Plain `go build` leaves the defaults, which is the intended signal for a
// local/dev binary.
//
//	go build -ldflags "\
//	  -X github.com/kelchm/paperboy/internal/buildinfo.Version=1.2.3 \
//	  -X github.com/kelchm/paperboy/internal/buildinfo.Commit=$(git rev-parse HEAD) \
//	  -X github.com/kelchm/paperboy/internal/buildinfo.Date=$(date -u +%FT%TZ)"
var (
	// Version is the paperboy release version.
	Version = "0.0.1"
	// Commit is the git revision the binary was built from.
	Commit = "none"
	// Date is the build timestamp (RFC 3339).
	Date = "unknown"
)

// String renders the full build identity, e.g. "0.0.1 (abc1234, 2026-07-01T12:00:00Z)".
func String() string {
	return Version + " (" + Commit + ", " + Date + ")"
}

// UserAgent is the HTTP User-Agent paperboy presents to upstream servers. It
// stays Version-only — the commit/date are noise to a CDN operator.
func UserAgent() string {
	return "paperboy/" + Version + " (+https://github.com/kelchm/paperboy)"
}
