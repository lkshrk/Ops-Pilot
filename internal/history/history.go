// Package history is the passive run log. It records what happened and never
// gates behaviour: the ops-pilot/reverted label is the only durable decision.
package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lkshrk/ops-pilot/internal/domain"
	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS runs (
	id           TEXT PRIMARY KEY,
	repository   TEXT NOT NULL,
	mode         TEXT NOT NULL,
	started_at   TEXT NOT NULL,
	finished_at  TEXT,
	halted       TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS attempts (
	id               INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id           TEXT NOT NULL REFERENCES runs(id),
	pull_request     INTEGER NOT NULL,
	dependency       TEXT NOT NULL,
	kind             TEXT NOT NULL,
	from_version     TEXT NOT NULL,
	to_version       TEXT NOT NULL,
	bump             TEXT NOT NULL,
	decision         TEXT NOT NULL,
	reason           TEXT NOT NULL DEFAULT '',
	changelog_source TEXT NOT NULL DEFAULT '',
	merge_sha        TEXT NOT NULL DEFAULT '',
	watch            TEXT NOT NULL DEFAULT '',
	verdict          TEXT NOT NULL,
	fix_attempts     INTEGER NOT NULL DEFAULT 0,
	revert_sha       TEXT NOT NULL DEFAULT '',
	duration_ms      INTEGER NOT NULL DEFAULT 0,
	error            TEXT NOT NULL DEFAULT '',
	changelog_url    TEXT NOT NULL DEFAULT '',
	head_sha         TEXT NOT NULL DEFAULT '',
	pre_merge_sha    TEXT NOT NULL DEFAULT '',
	broken           TEXT NOT NULL DEFAULT '',
	diagnosis_cause  TEXT NOT NULL DEFAULT '',
	fixes            TEXT NOT NULL DEFAULT '',
	waited           INTEGER NOT NULL DEFAULT 0,
	evidence         TEXT NOT NULL DEFAULT '',
	started_at       TEXT NOT NULL DEFAULT '',
	finished_at      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS attempts_by_run ON attempts(run_id);
`

type Store struct{ db *sql.DB }

// Open creates the database and its schema if they do not exist.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("history database path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create history directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open history database: %w", err)
	}
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA foreign_keys=ON", "PRAGMA busy_timeout=5000"} {
		if _, err := db.Exec(pragma); err != nil {
			return nil, errors.Join(fmt.Errorf("configure history database: %w", err), db.Close())
		}
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, errors.Join(fmt.Errorf("create history schema: %w", err), db.Close())
	}
	if err := migrate(db); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return &Store{db: db}, nil
}

// migrate adds columns to a database created by an earlier version. CREATE
// TABLE IF NOT EXISTS silently leaves an existing table alone, so a new column
// has to be added explicitly; SQLite has no ADD COLUMN IF NOT EXISTS, and a
// duplicate is the expected outcome on an up-to-date database.
func migrate(db *sql.DB) error {
	for _, column := range []struct{ table, definition string }{
		{"runs", "halted TEXT NOT NULL DEFAULT ''"},
		{"attempts", "changelog_url TEXT NOT NULL DEFAULT ''"},
		{"attempts", "head_sha TEXT NOT NULL DEFAULT ''"},
		{"attempts", "pre_merge_sha TEXT NOT NULL DEFAULT ''"},
		{"attempts", "broken TEXT NOT NULL DEFAULT ''"},
		{"attempts", "diagnosis_cause TEXT NOT NULL DEFAULT ''"},
		{"attempts", "fixes TEXT NOT NULL DEFAULT ''"},
		{"attempts", "waited INTEGER NOT NULL DEFAULT 0"},
		{"attempts", "evidence TEXT NOT NULL DEFAULT ''"},
		{"attempts", "started_at TEXT NOT NULL DEFAULT ''"},
		{"attempts", "finished_at TEXT NOT NULL DEFAULT ''"},
	} {
		statement := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s", column.table, column.definition)
		if _, err := db.Exec(statement); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return fmt.Errorf("add %s to %s: %w", column.definition, column.table, err)
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) StartRun(ctx context.Context, run domain.Run) error {
	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO runs (id, repository, mode, started_at) VALUES (?, ?, ?, ?)`,
		run.ID, run.Repository.String(), run.Mode, run.StartedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("record run start: %w", err)
	}
	return nil
}

func (s *Store) FinishRun(ctx context.Context, id string, at time.Time, halted string) error {
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE runs SET finished_at = ?, halted = ? WHERE id = ?`,
		at.UTC().Format(time.RFC3339Nano), halted, id,
	)
	if err != nil {
		return fmt.Errorf("record run finish: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("record run finish: %w", err)
	}
	// SQLite counts rows matched, not rows changed, so a repeated finish reports one.
	if affected == 0 {
		return fmt.Errorf("record run finish: no run %q", id)
	}
	return nil
}

func (s *Store) RecordAttempt(ctx context.Context, attempt domain.Attempt) error {
	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO attempts (
			run_id, pull_request, dependency, kind, from_version, to_version, bump,
			decision, reason, changelog_source, merge_sha, watch, verdict,
			fix_attempts, revert_sha, duration_ms, error,
			changelog_url, head_sha, pre_merge_sha, broken, diagnosis_cause,
			fixes, waited, evidence, started_at, finished_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		attempt.RunID, attempt.PullRequest,
		attempt.Dependency.Name, attempt.Dependency.Kind,
		attempt.Dependency.FromVersion, attempt.Dependency.ToVersion, string(attempt.Dependency.Bump),
		string(attempt.Decision), attempt.Reason, string(attempt.ChangelogSource),
		attempt.MergeSHA, string(attempt.Watch), string(attempt.Verdict),
		attempt.FixAttempts, attempt.RevertSHA,
		attempt.Duration.Milliseconds(), attempt.Error,
		attempt.ChangelogURL, attempt.HeadSHA, attempt.PreMergeSHA,
		encode(attempt.Broken), attempt.DiagnosisCause,
		encode(attempt.Fixes), attempt.Waited, encode(attempt.Evidence),
		stamp(attempt.StartedAt), stamp(attempt.FinishedAt),
	)
	if err != nil {
		return fmt.Errorf("record attempt: %w", err)
	}
	return nil
}

// Runs returns the most recent runs with their attempts, newest first.
func (s *Store) Runs(ctx context.Context, limit int) ([]domain.Run, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT id, repository, mode, started_at, COALESCE(finished_at, ''), halted
		 FROM runs ORDER BY started_at DESC, rowid DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var runs []domain.Run
	for rows.Next() {
		var (
			run               domain.Run
			repository        string
			started, finished string
		)
		if err := rows.Scan(&run.ID, &repository, &run.Mode, &started, &finished, &run.Halted); err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		run.Repository = parseRepository(repository)
		run.StartedAt = parseTime(started)
		run.FinishedAt = parseTime(finished)
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read runs: %w", err)
	}
	for i := range runs {
		attempts, err := s.Attempts(ctx, runs[i].ID)
		if err != nil {
			return nil, err
		}
		runs[i].Attempts = attempts
	}
	return runs, nil
}

func (s *Store) Attempts(ctx context.Context, runID string) ([]domain.Attempt, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT pull_request, dependency, kind, from_version, to_version, bump,
		        decision, reason, changelog_source, merge_sha, watch, verdict,
		        fix_attempts, revert_sha, duration_ms, error,
		        changelog_url, head_sha, pre_merge_sha, broken, diagnosis_cause,
		        fixes, waited, evidence, started_at, finished_at
		 FROM attempts WHERE run_id = ? ORDER BY id`,
		runID,
	)
	if err != nil {
		return nil, fmt.Errorf("query attempts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	attempts := make([]domain.Attempt, 0)
	for rows.Next() {
		attempt := domain.Attempt{RunID: runID}
		var (
			milliseconds            int64
			broken, fixes, evidence string
			started, finished       string
		)
		err := rows.Scan(
			&attempt.PullRequest, &attempt.Dependency.Name, &attempt.Dependency.Kind,
			&attempt.Dependency.FromVersion, &attempt.Dependency.ToVersion, &attempt.Dependency.Bump,
			&attempt.Decision, &attempt.Reason, &attempt.ChangelogSource,
			&attempt.MergeSHA, &attempt.Watch, &attempt.Verdict,
			&attempt.FixAttempts, &attempt.RevertSHA,
			&milliseconds, &attempt.Error,
			&attempt.ChangelogURL, &attempt.HeadSHA, &attempt.PreMergeSHA,
			&broken, &attempt.DiagnosisCause,
			&fixes, &attempt.Waited, &evidence,
			&started, &finished,
		)
		if err != nil {
			return nil, fmt.Errorf("scan attempt: %w", err)
		}
		attempt.Duration = time.Duration(milliseconds) * time.Millisecond
		attempt.Error = noteFaults(
			attempt.Error,
			fault{"broken", decode(broken, &attempt.Broken)},
			fault{"fixes", decode(fixes, &attempt.Fixes)},
			fault{"evidence", decode(evidence, &attempt.Evidence)},
		)
		attempt.StartedAt, attempt.FinishedAt = parseTime(started), parseTime(finished)
		attempts = append(attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read attempts: %w", err)
	}
	return attempts, nil
}

func (s *Store) Run(ctx context.Context, id string) (domain.Run, error) {
	var (
		run               domain.Run
		repository        string
		started, finished string
	)
	err := s.db.QueryRowContext(
		ctx,
		`SELECT id, repository, mode, started_at, COALESCE(finished_at, ''), halted FROM runs WHERE id = ?`,
		id,
	).Scan(&run.ID, &repository, &run.Mode, &started, &finished, &run.Halted)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Run{}, fmt.Errorf("no run %q", id)
	}
	if err != nil {
		return domain.Run{}, fmt.Errorf("query run: %w", err)
	}
	run.Repository = parseRepository(repository)
	run.StartedAt = parseTime(started)
	run.FinishedAt = parseTime(finished)
	run.Attempts, err = s.Attempts(ctx, id)
	return run, err
}

func parseRepository(value string) domain.RepositoryRef {
	owner, name, _ := strings.Cut(value, "/")
	return domain.RepositoryRef{Owner: owner, Name: name}
}

func parseTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

// encode stores a structured field as JSON. A failure yields an empty column
// rather than failing the write: the history log must never cost a run.
func encode(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	if string(raw) == "null" {
		return ""
	}
	return string(raw)
}

// json.Unmarshal fills the destination up to the element that fails.
func decode[T any](raw string, into *T) error {
	if raw == "" {
		return nil
	}
	var value T
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return err
	}
	*into = value
	return nil
}

type fault struct {
	column string
	err    error
}

func noteFaults(existing string, faults ...fault) string {
	notes := make([]string, 0, len(faults)+1)
	if existing != "" {
		notes = append(notes, existing)
	}
	for _, fault := range faults {
		if fault.err != nil {
			notes = append(notes, fmt.Sprintf("history: unreadable %s column: %v", fault.column, fault.err))
		}
	}
	return strings.Join(notes, "; ")
}

func stamp(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format(time.RFC3339Nano)
}
