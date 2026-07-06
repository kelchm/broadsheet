// Package store is paperboy's persistent state substrate: one SQLite file in
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
}

// timeLayout is fixed-width on purpose: RFC3339Nano trims trailing fractional
// zeros, which breaks the lexicographic-equals-chronological property that
// MAX(at), ORDER BY at, and the prune's string comparison all rely on. All
// stored times are UTC, so the literal Z is correct.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

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

// SeedSources inserts rows that don't already exist (by ID). Existing rows are
// left untouched — user edits win over catalog updates.
func (s *Store) SeedSources(rows []SourceRow) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: seed sources: %w", err)
	}
	for _, r := range rows {
		cfg := r.ProviderConfig
		if len(cfg) == 0 {
			cfg = json.RawMessage("{}")
		}
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO sources (id, display_name, provider_type, provider_config, enabled, position)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			r.ID, r.DisplayName, r.ProviderType, string(cfg), r.Enabled, r.Position,
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: seed source %s: %w", r.ID, err)
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

// RecordSuccess appends a success fetch event.
func (s *Store) RecordSuccess(sourceID string, when time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO fetch_events (source_id, at, ok) VALUES (?, ?, 1)`,
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
					PARTITION BY source_id, ok ORDER BY at DESC, id DESC
				) AS rn FROM fetch_events
			) WHERE rn = 1
		)`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: prune fetch events: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// SourceHealth mirrors the legacy state.json surface: latest success and
// failure timestamps, with the failure message blanked once a newer success
// exists. (The richer two-timestamp poll/store model arrives with the API
// phase; the events table already carries what it needs.)
type SourceHealth struct {
	LastFetchOK    *time.Time
	LastFetchError *time.Time
	LastErrorMsg   string
}

// HealthSnapshot returns per-source health derived from the event history.
func (s *Store) HealthSnapshot() (map[string]SourceHealth, error) {
	rows, err := s.db.Query(`
		SELECT source_id, ok, MAX(at) AS at,
		       (SELECT error_msg FROM fetch_events e2
		        WHERE e2.source_id = e.source_id AND e2.ok = e.ok
		        ORDER BY e2.at DESC, e2.id DESC LIMIT 1) AS msg
		FROM fetch_events e
		GROUP BY source_id, ok`)
	if err != nil {
		return nil, fmt.Errorf("store: health snapshot: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]SourceHealth{}
	type lastMsg struct {
		failAt time.Time
		okAt   time.Time
		msg    string
	}
	acc := map[string]*lastMsg{}
	for rows.Next() {
		var id, at, msg string
		var ok bool
		if err := rows.Scan(&id, &ok, &at, &msg); err != nil {
			return nil, fmt.Errorf("store: scan health: %w", err)
		}
		t, err := time.Parse(timeLayout, at)
		if err != nil {
			continue
		}
		a := acc[id]
		if a == nil {
			a = &lastMsg{}
			acc[id] = a
		}
		if ok {
			a.okAt = t
		} else {
			a.failAt = t
			a.msg = msg
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for id, a := range acc {
		h := SourceHealth{}
		if !a.okAt.IsZero() {
			t := a.okAt
			h.LastFetchOK = &t
		}
		if !a.failAt.IsZero() {
			t := a.failAt
			h.LastFetchError = &t
			// Legacy surface: a success strictly newer than the last failure
			// clears the message but keeps the failure timestamp. Ties keep the
			// message — a reconcile cycle that stores some editions AND fails a
			// Put records both with the same clock reading, and hiding an active
			// archive-write failure behind a clean record is the wrong call.
			if !a.failAt.Before(a.okAt) {
				h.LastErrorMsg = a.msg
			}
		}
		out[id] = h
	}
	return out, nil
}
