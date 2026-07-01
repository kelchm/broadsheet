// Package buildinfo carries the paperboy build version, shared by the binaries,
// the embeddable library, and the upstream User-Agent so they never drift.
package buildinfo

// Version is the paperboy release version. It can be overridden at build time:
//
//	go build -ldflags "-X github.com/kelchm/paperboy/internal/buildinfo.Version=1.2.3"
var Version = "0.0.1"

// UserAgent is the HTTP User-Agent paperboy presents to upstream servers.
func UserAgent() string {
	return "paperboy/" + Version + " (+https://github.com/kelchm/paperboy)"
}
