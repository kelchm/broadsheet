package cache

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FilesystemLookup implements rotation.CacheLookup by scanning the disk
// image cache for the most recent PNG across any source.
//
// We use the filesystem as the source of truth (rather than state.json's
// Cache map) so that the lookup remains accurate even if state.json gets
// out of sync with the disk — e.g., manual cleanup, restored from backup.
type FilesystemLookup struct {
	Images *Images
}

// MostRecent walks <root>/<sourceID>/*.png and returns the newest entry by
// modification time.
func (l *FilesystemLookup) MostRecent() (sourceID, pngPath string, fetchedAt time.Time, ok bool) {
	entries, err := os.ReadDir(l.Images.Root)
	if err != nil {
		return
	}
	var newest time.Time
	var newestPath, newestSource string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sid := e.Name()
		srcDir := filepath.Join(l.Images.Root, sid)
		files, err := os.ReadDir(srcDir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".png") {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			if info.ModTime().After(newest) {
				newest = info.ModTime()
				newestPath = filepath.Join(srcDir, f.Name())
				newestSource = sid
			}
		}
	}
	if newestPath == "" {
		return
	}
	return newestSource, newestPath, newest, true
}
