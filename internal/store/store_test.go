package store

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "paperboy.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpen_MigratesAndReopens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "paperboy.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Reopen: migrations must be idempotent.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s2.Close() }()
	if n, err := s2.CountSources(); err != nil || n != 0 {
		t.Fatalf("CountSources = %d, %v", n, err)
	}
}

func TestSeedSources_ExistingRowsWin(t *testing.T) {
	s := open(t)
	rows := []SourceRow{
		{ID: "a", DisplayName: "Paper A", ProviderType: "freedomforum",
			ProviderConfig: json.RawMessage(`{"prefix":"A_A"}`), Enabled: true, Position: 1},
		{ID: "b", DisplayName: "Paper B", ProviderType: "freedomforum", Enabled: false, Position: 2},
	}
	if err := s.SeedSources(rows); err != nil {
		t.Fatalf("SeedSources: %v", err)
	}
	// Re-seeding with different values must NOT clobber user state.
	if err := s.SeedSources([]SourceRow{
		{ID: "a", DisplayName: "RENAMED", ProviderType: "freedomforum", Enabled: false},
	}); err != nil {
		t.Fatalf("re-seed: %v", err)
	}

	all, err := s.ListSources(false)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d sources, want 2", len(all))
	}
	if all[0].ID != "a" || all[0].DisplayName != "Paper A" || !all[0].Enabled {
		t.Errorf("row a = %+v, want original values preserved (seed must not clobber)", all[0])
	}
	if string(all[1].ProviderConfig) != "{}" {
		t.Errorf("empty config = %q, want {}", all[1].ProviderConfig)
	}

	enabled, err := s.ListSources(true)
	if err != nil || len(enabled) != 1 || enabled[0].ID != "a" {
		t.Errorf("enabled set = %+v (err %v), want just a", enabled, err)
	}
}

func TestVersionsRoundTrip(t *testing.T) {
	s := open(t)
	if v := s.Versions("x"); len(v) != 0 {
		t.Fatalf("fresh Versions = %v, want empty", v)
	}
	want := map[string]string{"url1": "e1", "url2": "e2"}
	if err := s.SetVersions("x", want); err != nil {
		t.Fatalf("SetVersions: %v", err)
	}
	got := s.Versions("x")
	if len(got) != 2 || got["url1"] != "e1" || got["url2"] != "e2" {
		t.Errorf("Versions = %v, want %v", got, want)
	}
	// Replace semantics: old keys vanish.
	if err := s.SetVersions("x", map[string]string{"url3": "e3"}); err != nil {
		t.Fatalf("SetVersions replace: %v", err)
	}
	if got := s.Versions("x"); len(got) != 1 || got["url3"] != "e3" {
		t.Errorf("after replace Versions = %v, want only url3", got)
	}
}

func TestHealthSnapshot_LegacySurface(t *testing.T) {
	s := open(t)
	t0 := time.Date(2026, 6, 30, 10, 0, 0, 0, time.UTC)

	// Failure then success: message clears, both timestamps survive.
	if err := s.RecordFailure("a", "boom", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSuccess("a", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Success then failure: message present.
	if err := s.RecordSuccess("b", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordFailure("b", "down", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	h, err := s.HealthSnapshot()
	if err != nil {
		t.Fatalf("HealthSnapshot: %v", err)
	}
	a := h["a"]
	if a.LastFetchOK == nil || !a.LastFetchOK.Equal(t0.Add(time.Hour)) {
		t.Errorf("a.LastFetchOK = %v, want %v", a.LastFetchOK, t0.Add(time.Hour))
	}
	if a.LastFetchError == nil || !a.LastFetchError.Equal(t0) {
		t.Errorf("a.LastFetchError = %v, want %v", a.LastFetchError, t0)
	}
	if a.LastErrorMsg != "" {
		t.Errorf("a.LastErrorMsg = %q, want cleared after newer success", a.LastErrorMsg)
	}
	b := h["b"]
	if b.LastErrorMsg != "down" {
		t.Errorf("b.LastErrorMsg = %q, want down", b.LastErrorMsg)
	}
}

func TestPruneFetchEvents(t *testing.T) {
	s := open(t)
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	if err := s.RecordSuccess("a", now.AddDate(0, 0, -30)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSuccess("a", now); err != nil {
		t.Fatal(err)
	}
	n, err := s.PruneFetchEvents(14*24*time.Hour, now)
	if err != nil || n != 1 {
		t.Fatalf("PruneFetchEvents = %d, %v; want 1, nil", n, err)
	}
	h, _ := s.HealthSnapshot()
	if h["a"].LastFetchOK == nil || !h["a"].LastFetchOK.Equal(now) {
		t.Errorf("recent event must survive prune")
	}
}

func TestHealthSnapshot_TieKeepsFailureMessage(t *testing.T) {
	// A reconcile cycle that stores some editions AND fails a Put records both
	// events with the same clock reading; the failure must stay visible.
	s := open(t)
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	if err := s.RecordSuccess("a", now); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordFailure("a", "archive put: disk full", now); err != nil {
		t.Fatal(err)
	}
	h, err := s.HealthSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if h["a"].LastErrorMsg != "archive put: disk full" {
		t.Errorf("LastErrorMsg = %q, want the tied failure message kept", h["a"].LastErrorMsg)
	}
}

func TestHealthSnapshot_PicksNewestOfMany(t *testing.T) {
	// Multiple events per (source, ok) group: MAX(at) and the message subquery
	// must pick the newest, including sub-second orderings that break with
	// trimmed (RFC3339Nano) timestamps.
	s := open(t)
	base := time.Date(2026, 6, 30, 10, 0, 0, 0, time.UTC)
	for _, d := range []time.Duration{0, 500 * time.Millisecond, 510 * time.Millisecond} {
		if err := s.RecordSuccess("a", base.Add(d)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecordFailure("a", "old", base.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordFailure("a", "newest failure", base.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	h, err := s.HealthSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got := h["a"].LastFetchOK; got == nil || !got.Equal(base.Add(510*time.Millisecond)) {
		t.Errorf("LastFetchOK = %v, want %v (newest, sub-second ordering)", got, base.Add(510*time.Millisecond))
	}
	if got := h["a"].LastFetchError; got == nil || !got.Equal(base.Add(-time.Minute)) {
		t.Errorf("LastFetchError = %v, want newest failure time", got)
	}
	// Success is newer than the last failure -> message cleared (legacy surface).
	if h["a"].LastErrorMsg != "" {
		t.Errorf("LastErrorMsg = %q, want cleared", h["a"].LastErrorMsg)
	}
}

func TestPruneFetchEvents_KeepsNewestSuccessAndFailure(t *testing.T) {
	// A source failing for longer than the retention keeps its last success —
	// that's exactly the datum you want when things have been broken a while.
	s := open(t)
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	lastGood := now.AddDate(0, 0, -30)
	if err := s.RecordSuccess("a", lastGood.AddDate(0, 0, -5)); err != nil {
		t.Fatal(err) // an even older success: prunable
	}
	if err := s.RecordSuccess("a", lastGood); err != nil {
		t.Fatal(err) // newest success: kept despite age
	}
	if err := s.RecordFailure("a", "down", now.AddDate(0, 0, -20)); err != nil {
		t.Fatal(err) // old failure, superseded by a newer one: prunable
	}
	if err := s.RecordFailure("a", "still down", now); err != nil {
		t.Fatal(err) // newest failure (recent anyway)
	}

	if _, err := s.PruneFetchEvents(14*24*time.Hour, now); err != nil {
		t.Fatal(err)
	}
	h, _ := s.HealthSnapshot()
	if got := h["a"].LastFetchOK; got == nil || !got.Equal(lastGood) {
		t.Errorf("LastFetchOK = %v, want %v preserved past retention", got, lastGood)
	}
}

func TestHealthSnapshot_PollVsStore(t *testing.T) {
	s := open(t)
	base := time.Date(2026, 6, 30, 10, 0, 0, 0, time.UTC)
	// An edition stored two days ago, clean polls since (weekend of 304s).
	if err := s.RecordSuccess("a", base.AddDate(0, 0, -2)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordPoll("a", base); err != nil {
		t.Fatal(err)
	}

	h, err := s.HealthSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got := h["a"].LastPollOK; got == nil || !got.Equal(base) {
		t.Errorf("LastPollOK = %v, want %v (clean poll proves reachability)", got, base)
	}
	if got := h["a"].LastFetchOK; got == nil || !got.Equal(base.AddDate(0, 0, -2)) {
		t.Errorf("LastFetchOK = %v, want the store event, not the poll", got)
	}

	// A source with only poll events (nothing ever stored) still shows reachable.
	if err := s.RecordPoll("b", base); err != nil {
		t.Fatal(err)
	}
	h, _ = s.HealthSnapshot()
	if h["b"].LastPollOK == nil || h["b"].LastFetchOK != nil {
		t.Errorf("poll-only source = %+v, want LastPollOK set and LastFetchOK nil", h["b"])
	}
}
