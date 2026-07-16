// Package store is broadsheet's persistent state substrate: one SQLite file in
// the data directory holding everything a UI can eventually mutate — the
// source catalog's enabled set, provider version tokens, fetch-event health
// history, and crop overrides. Artifacts (PDFs, rendered PNGs) stay on the
// filesystem; this file holds records.
//
// The driver is modernc.org/sqlite (pure Go): no new native linkage on top of
// the MuPDF the project already carries, and CGO_ENABLED=0 builds keep working.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // database/sql driver
)

// migrations are applied in order; PRAGMA user_version tracks progress. Append
// only — never edit a shipped entry.
var migrations = []string{
	`
CREATE TABLE sources (
	id              TEXT PRIMARY KEY,
	display_name    TEXT NOT NULL,
	provider_type   TEXT NOT NULL,
	provider_config TEXT NOT NULL DEFAULT '{}',
	enabled         INTEGER NOT NULL DEFAULT 0,
	position        INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE provider_versions (
	source_id TEXT NOT NULL,
	key       TEXT NOT NULL,
	token     TEXT NOT NULL,
	PRIMARY KEY (source_id, key)
);
CREATE TABLE fetch_events (
	id        INTEGER PRIMARY KEY AUTOINCREMENT,
	source_id TEXT NOT NULL,
	at        TEXT NOT NULL,
	ok        INTEGER NOT NULL,
	error_msg TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_fetch_events_source_at ON fetch_events(source_id, at DESC);
CREATE TABLE crop_overrides (
	source_id  TEXT PRIMARY KEY,
	x REAL NOT NULL, y REAL NOT NULL, w REAL NOT NULL, h REAL NOT NULL,
	mode       TEXT NOT NULL CHECK (mode IN ('auto','manual','approved')),
	updated_at TEXT NOT NULL
);
`,
	// v2: distinguish "the poll reached upstream cleanly" from "an edition was
	// stored" — a weekend of 304s is healthy, and a poll that succeeds while
	// nothing stores must not look dead.
	`
ALTER TABLE fetch_events ADD COLUMN kind TEXT NOT NULL DEFAULT 'store';
`,
}

// timeLayout is fixed-width on purpose: RFC3339Nano trims trailing fractional
// zeros, which breaks the lexicographic-equals-chronological property that
// MAX(at), ORDER BY at, and the prune's string comparison all rely on. All
// stored times are UTC, so the literal Z is correct.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

// ErrNotFound reports that a row doesn't exist — distinct from I/O failures so
// callers can map it to a 404 instead of a 500.
var ErrNotFound = errors.New("store: not found")

// Store wraps the SQLite database. Safe for concurrent use.
type Store struct {
	db *sql.DB
}

// Open opens (creating and migrating as needed) the database at path.
func Open(path string) (*Store, error) {
	// Pragmas ride the DSN so every connection gets them — busy_timeout and
	// foreign_keys are per-connection settings, and database/sql may replace a
	// connection at any time. journal_mode=WAL persists in the file but is
	// idempotent here.
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// One connection: SQLite is single-writer, our load is tiny, and this
	// sidesteps SQLITE_BUSY entirely.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("store: begin migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: migration %d: %w", i+1, err)
		}
		// PRAGMA can't be parameterized; the value is a loop index.
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: bump schema version: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: commit migration %d: %w", i+1, err)
		}
	}
	return nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// SourceRow is a catalog entry as stored: pure data, decoded into a typed
// provider by the registry at load time.
type SourceRow struct {
	ID             string
	DisplayName    string
	ProviderType   string
	ProviderConfig json.RawMessage
	Enabled        bool
	Position       int
}

// SeedSources reconciles the store against the catalog. The catalog owns a
// paper's identity and wiring (display name, provider type + config, catalog
// position); the store owns the user's one choice, its enabled flag. Every boot
// it fully reconciles: a new paper is inserted with its catalog default; an
// existing paper's wiring is refreshed from the catalog (that is how a catalog
// fix — a repointed provider, a corrected config, a rename — reaches installs
// that already have the row) while the user's enabled toggle is never clobbered
// by the catalog's default; and a paper the catalog has dropped is deleted, so a
// removed or permanently-broken source disappears from existing installs, not
// only from fresh ones. (The Enabled field carried in each row is only the
// catalog default; SetSourceEnabled is the sole writer of a user's choice.)
//
// Deletion is safe because the sources table is wholly catalog-derived — the
// only writer of a row is this method, and embedders that supply their own
// sources bypass the store entirely (Config.Sources) — so an id absent from the
// catalog is unambiguously a removed paper. The one caveat is a downgrade:
// running an older binary whose embedded catalog predates papers a newer binary
// added would prune those rows (they return, at their catalog default, on the
// next upgrade). An empty catalog never prunes, so a caller passing nothing
// can't wipe the table.
//
// When prune is false the drop step is skipped (upsert only). The caller uses
// this to avoid deleting a dropped paper's row before its archived display name
// was safely preserved — losing the name is worse than a stale row that the next
// (pruning) boot cleans up.
func (s *Store) SeedSources(rows []SourceRow, prune bool) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: seed sources: %w", err)
	}

	// Snapshot existing ids up front so we can prune the ones the catalog no
	// longer carries. Read fully and close before issuing writes on the tx.
	existing := map[string]bool{}
	idRows, err := tx.Query(`SELECT id FROM sources`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: seed sources: read ids: %w", err)
	}
	for idRows.Next() {
		var id string
		if err := idRows.Scan(&id); err != nil {
			_ = idRows.Close()
			_ = tx.Rollback()
			return fmt.Errorf("store: seed sources: scan id: %w", err)
		}
		existing[id] = true
	}
	if err := idRows.Err(); err != nil {
		_ = idRows.Close()
		_ = tx.Rollback()
		return fmt.Errorf("store: seed sources: read ids: %w", err)
	}
	_ = idRows.Close()

	incoming := make(map[string]bool, len(rows))
	for _, r := range rows {
		incoming[r.ID] = true
		cfg := r.ProviderConfig
		if len(cfg) == 0 {
			cfg = json.RawMessage("{}")
		}
		if _, err := tx.Exec(
			`INSERT INTO sources (id, display_name, provider_type, provider_config, enabled, position)
			 VALUES (?, ?, ?, ?, ?, ?)
			 ON CONFLICT(id) DO UPDATE SET
			     display_name    = excluded.display_name,
			     provider_type   = excluded.provider_type,
			     provider_config = excluded.provider_config,
			     position        = excluded.position`,
			r.ID, r.DisplayName, r.ProviderType, string(cfg), r.Enabled, r.Position,
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: seed source %s: %w", r.ID, err)
		}
	}

	// Prune papers the catalog has dropped, plus their dependent state, so a
	// removed source leaves nothing behind (a stale row would keep the reconciler
	// polling a dead feed forever and clutter the catalog UI). Skipped when the
	// caller couldn't first preserve dropped papers' archived names.
	if prune {
		for id := range existing {
			if incoming[id] {
				continue
			}
			for _, stmt := range []string{
				`DELETE FROM sources WHERE id = ?`,
				`DELETE FROM provider_versions WHERE source_id = ?`,
				`DELETE FROM fetch_events WHERE source_id = ?`,
				`DELETE FROM crop_overrides WHERE source_id = ?`,
			} {
				if _, err := tx.Exec(stmt, id); err != nil {
					_ = tx.Rollback()
					return fmt.Errorf("store: prune source %s: %w", id, err)
				}
			}
		}
	}
	return tx.Commit()
}

// ListSources returns catalog rows ordered by position then ID; enabledOnly
// restricts to the user's enabled set.
func (s *Store) ListSources(enabledOnly bool) ([]SourceRow, error) {
	q := `SELECT id, display_name, provider_type, provider_config, enabled, position FROM sources`
	if enabledOnly {
		q += ` WHERE enabled = 1`
	}
	q += ` ORDER BY position, id`
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("store: list sources: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SourceRow
	for rows.Next() {
		var r SourceRow
		var cfg string
		if err := rows.Scan(&r.ID, &r.DisplayName, &r.ProviderType, &cfg, &r.Enabled, &r.Position); err != nil {
			return nil, fmt.Errorf("store: scan source: %w", err)
		}
		r.ProviderConfig = json.RawMessage(cfg)
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountSources reports how many catalog rows exist (seeding guard).
func (s *Store) CountSources() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM sources`).Scan(&n)
	return n, err
}

// Versions returns the persisted provider version tokens for a source. Errors
// degrade to an empty map — the worst case is an unconditional refetch.
func (s *Store) Versions(sourceID string) map[string]string {
	out := map[string]string{}
	rows, err := s.db.Query(`SELECT key, token FROM provider_versions WHERE source_id = ?`, sourceID)
	if err != nil {
		return out
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var k, v string
		if rows.Scan(&k, &v) == nil {
			out[k] = v
		}
	}
	return out
}

// SetVersions replaces the persisted version tokens for a source.
func (s *Store) SetVersions(sourceID string, versions map[string]string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: set versions: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM provider_versions WHERE source_id = ?`, sourceID); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: clear versions: %w", err)
	}
	for k, v := range versions {
		if _, err := tx.Exec(
			`INSERT INTO provider_versions (source_id, key, token) VALUES (?, ?, ?)`,
			sourceID, k, v,
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: insert version: %w", err)
		}
	}
	return tx.Commit()
}

// RecordSuccess appends a success fetch event (an edition was stored).
func (s *Store) RecordSuccess(sourceID string, when time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO fetch_events (source_id, at, ok, kind) VALUES (?, ?, 1, 'store')`,
		sourceID, when.UTC().Format(timeLayout),
	)
	return err
}

// RecordPoll appends a clean-poll event: upstream was reachable and answered
// coherently, whether or not anything new was stored.
func (s *Store) RecordPoll(sourceID string, when time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO fetch_events (source_id, at, ok, kind) VALUES (?, ?, 1, 'poll')`,
		sourceID, when.UTC().Format(timeLayout),
	)
	return err
}

// RecordFailure appends a failure fetch event with its message.
func (s *Store) RecordFailure(sourceID, msg string, when time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO fetch_events (source_id, at, ok, error_msg) VALUES (?, ?, 0, ?)`,
		sourceID, when.UTC().Format(timeLayout), msg,
	)
	return err
}

// PruneFetchEvents removes events older than retention, always keeping each
// source's newest success and newest failure: "last good fetch was June 1" is
// most useful precisely when a source has been failing longer than the
// retention window.
func (s *Store) PruneFetchEvents(retention time.Duration, now time.Time) (int, error) {
	cutoff := now.Add(-retention).UTC().Format(timeLayout)
	res, err := s.db.Exec(`
		DELETE FROM fetch_events WHERE at < ? AND id NOT IN (
			SELECT id FROM (
				SELECT id, ROW_NUMBER() OVER (
					PARTITION BY source_id, ok, kind ORDER BY at DESC, id DESC
				) AS rn FROM fetch_events
			) WHERE rn = 1
		)`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: prune fetch events: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// SourceHealth is the two-timestamp model: LastPollOK proves upstream
// reachability (any clean poll, including all-304 no-op cycles), LastFetchOK
// is when an edition last actually stored, and LastFetchError/LastErrorMsg
// carry the newest failure (message blanked once a strictly newer store
// exists — ties keep it, so a cycle that stores some editions AND fails a Put
// stays visible).
type SourceHealth struct {
	LastPollOK     *time.Time
	LastFetchOK    *time.Time
	LastFetchError *time.Time
	LastErrorMsg   string
}

// HealthSnapshot returns per-source health derived from the event history.
func (s *Store) HealthSnapshot() (map[string]SourceHealth, error) {
	rows, err := s.db.Query(`
		SELECT source_id,
		       MAX(CASE WHEN ok = 1 THEN at END)                    AS poll_at,
		       MAX(CASE WHEN ok = 1 AND kind = 'store' THEN at END) AS store_at,
		       MAX(CASE WHEN ok = 0 THEN at END)                    AS fail_at,
		       (SELECT error_msg FROM fetch_events e2
		        WHERE e2.source_id = e.source_id AND e2.ok = 0
		        ORDER BY e2.at DESC, e2.id DESC LIMIT 1)            AS msg
		FROM fetch_events e
		GROUP BY source_id`)
	if err != nil {
		return nil, fmt.Errorf("store: health snapshot: %w", err)
	}
	defer func() { _ = rows.Close() }()

	parse := func(ns sql.NullString) *time.Time {
		if !ns.Valid {
			return nil
		}
		t, err := time.Parse(timeLayout, ns.String)
		if err != nil {
			return nil
		}
		return &t
	}

	out := map[string]SourceHealth{}
	for rows.Next() {
		var id string
		var pollAt, storeAt, failAt, msg sql.NullString
		if err := rows.Scan(&id, &pollAt, &storeAt, &failAt, &msg); err != nil {
			return nil, fmt.Errorf("store: scan health: %w", err)
		}
		h := SourceHealth{
			LastPollOK:     parse(pollAt),
			LastFetchOK:    parse(storeAt),
			LastFetchError: parse(failAt),
		}
		if h.LastFetchError != nil &&
			(h.LastFetchOK == nil || !h.LastFetchError.Before(*h.LastFetchOK)) {
			h.LastErrorMsg = msg.String
		}
		out[id] = h
	}
	return out, rows.Err()
}

// SetSourceEnabled flips a source's membership in the enabled set.
func (s *Store) SetSourceEnabled(sourceID string, enabled bool) error {
	res, err := s.db.Exec(`UPDATE sources SET enabled = ? WHERE id = ?`, enabled, sourceID)
	if err != nil {
		return fmt.Errorf("store: set enabled: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: source %q", ErrNotFound, sourceID)
	}
	return nil
}
