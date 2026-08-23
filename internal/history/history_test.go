package history_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lkshrk/ops-pilot/internal/domain"
	"github.com/lkshrk/ops-pilot/internal/history"
	_ "modernc.org/sqlite"
)

func open(t *testing.T) *history.Store {
	t.Helper()
	store, err := history.Open(filepath.Join(t.TempDir(), "nested", "history.db"))
	if err != nil {
		t.Fatalf("open history: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestRunsIsEmptyOnAFreshDatabase(t *testing.T) {
	runs, err := open(t).Runs(context.Background(), 10)
	if err != nil {
		t.Fatalf("query runs: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("want no runs, got %d", len(runs))
	}
}

func TestAttemptRoundTripsThroughTheStore(t *testing.T) {
	ctx, store := context.Background(), open(t)
	started := time.Date(2026, 8, 2, 14, 32, 0, 0, time.UTC)
	run := domain.Run{
		ID:         "run-1",
		Repository: domain.RepositoryRef{Owner: "lkshrk", Name: "h-cloud"},
		StartedAt:  started,
		Mode:       "interactive",
	}
	if err := store.StartRun(ctx, run); err != nil {
		t.Fatalf("start run: %v", err)
	}
	want := domain.Attempt{
		RunID:       "run-1",
		PullRequest: 1209,
		Dependency: domain.Dependency{
			Name:        "ghcr.io/home-operations/gatus-sidecar",
			Kind:        domain.DependencyKindDocker,
			FromVersion: "0.3.6",
			ToVersion:   "0.4.0",
			Bump:        domain.BumpMinor,
		},
		Decision:        domain.DecideMerge,
		ChangelogSource: domain.ChangelogFromPullRequest,
		MergeSHA:        "8f3a21c",
		Watch:           domain.WatchFail,
		Verdict:         domain.VerdictReverted,
		Reason:          "envs.TZ renamed",
		FixAttempts:     1,
		RevertSHA:       "1c2d3e4",
		Duration:        134 * time.Second,
		HeadSHA:         "head123",
		PreMergeSHA:     "base999",
		DiagnosisCause:  "values.env was renamed",
		ChangelogURL:    "https://github.com/TwiN/gatus/releases/tag/v0.4.0",
		Fixes:           []string{"--- a\n+++ b\n"},
		Waited:          true,
		Evidence:        []string{"the HelmRelease sets values.env"},
		Broken: []domain.ObjectHealth{{
			Ref:    domain.ObjectRef{Kind: "HelmRelease", Namespace: "media", Name: "gatus"},
			Reason: "upgrade failed",
		}},
		StartedAt:  started.UTC(),
		FinishedAt: started.Add(134 * time.Second).UTC(),
	}
	if err := store.RecordAttempt(ctx, want); err != nil {
		t.Fatalf("record attempt: %v", err)
	}
	if err := store.FinishRun(ctx, "run-1", started.Add(time.Minute), ""); err != nil {
		t.Fatalf("finish run: %v", err)
	}

	runs, err := store.Runs(ctx, 10)
	if err != nil {
		t.Fatalf("query runs: %v", err)
	}
	if len(runs) != 1 || len(runs[0].Attempts) != 1 {
		t.Fatalf("want one run with one attempt, got %+v", runs)
	}
	if runs[0].Repository.Owner != "lkshrk" || runs[0].Repository.Name != "h-cloud" {
		t.Fatalf("repository did not round-trip: %+v", runs[0].Repository)
	}
	if !runs[0].StartedAt.Equal(started) || runs[0].FinishedAt.IsZero() {
		t.Fatalf("timestamps did not round-trip: %+v", runs[0])
	}
	if got := runs[0].Attempts[0]; !reflect.DeepEqual(got, want) {
		t.Fatalf("attempt did not round-trip:\n got %+v\nwant %+v", got, want)
	}
}

func TestRunReportsAnUnknownIdentifier(t *testing.T) {
	if _, err := open(t).Run(context.Background(), "missing"); err == nil {
		t.Fatal("want an error for an unknown run")
	}
}

// A halt is why the queue was abandoned; it has to outlive the terminal.
func TestHaltReasonSurvivesTheRun(t *testing.T) {
	ctx, store := context.Background(), open(t)
	started := time.Date(2026, 8, 2, 14, 32, 0, 0, time.UTC)
	run := domain.Run{ID: "run-2", Repository: domain.RepositoryRef{Owner: "a", Name: "b"}, StartedAt: started}
	if err := store.StartRun(ctx, run); err != nil {
		t.Fatalf("start run: %v", err)
	}
	const reason = "#918 was reverted but the cluster did not recover"
	if err := store.FinishRun(ctx, "run-2", started.Add(time.Minute), reason); err != nil {
		t.Fatalf("finish run: %v", err)
	}

	stored, err := store.Run(ctx, "run-2")
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if stored.Halted != reason {
		t.Fatalf("halt reason did not survive: %q", stored.Halted)
	}
}

// The terminal outcome is the row a later diagnosis reads to learn the run ended
// and why; writing it nowhere must not read as writing it.
func TestFinishingARunThatWasNeverRecordedIsAnError(t *testing.T) {
	ctx, store := context.Background(), open(t)
	err := store.FinishRun(ctx, "run-never-started", time.Now(), "#918 was reverted but the cluster did not recover")
	if err == nil {
		t.Fatal("want an error when no run row was updated")
	}
	if !strings.Contains(err.Error(), "run-never-started") {
		t.Fatalf("error should name the run: %v", err)
	}
	if strings.Contains(err.Error(), "#918") {
		t.Fatalf("error must not echo the halt reason, which is unredacted: %v", err)
	}
}

// A run finished twice with the same values still matched a row, so re-finishing
// must not be mistaken for a lost outcome.
func TestFinishingARunTwiceIsNotAnError(t *testing.T) {
	ctx, store := context.Background(), open(t)
	started := time.Date(2026, 8, 2, 14, 32, 0, 0, time.UTC)
	run := domain.Run{ID: "run-3", Repository: domain.RepositoryRef{Owner: "a", Name: "b"}, StartedAt: started}
	if err := store.StartRun(ctx, run); err != nil {
		t.Fatalf("start run: %v", err)
	}
	at := started.Add(time.Minute)
	if err := store.FinishRun(ctx, "run-3", at, "halted"); err != nil {
		t.Fatalf("first finish: %v", err)
	}
	if err := store.FinishRun(ctx, "run-3", at, "halted"); err != nil {
		t.Fatalf("second finish with identical values: %v", err)
	}
}

// An empty Broken reads as "the cluster was fine", an empty Evidence as "the
// agent offered none". A column that failed to parse must claim neither.
func TestACorruptStructuredColumnIsReportedRatherThanReadAsEmpty(t *testing.T) {
	tests := []struct {
		name   string
		column string
		raw    string
	}{
		{"truncated broken", "broken", `[{"ref":{"kind":"HelmRelease","name":"gatus"},"reason":"upgrade fai`},
		{"broken is not an array", "broken", `{"ref":{"kind":"HelmRelease"}}`},
		// encoding/json fills the destination up to the type error, so this
		// otherwise decodes to a fabricated "gatus was unhealthy" record.
		{"broken carries a wrongly typed field", "broken", `[{"ref":{"kind":"HelmRelease","name":"gatus"},"healthy":"yes"}]`},
		{"truncated evidence", "evidence", `["the HelmRelease sets values.env"`},
		{"fixes is not a string list", "fixes", `[{"diff":"--- a"}]`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "history.db")
			seedCorruptAttempt(t, path, test.column, test.raw)

			store, err := history.Open(path)
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })

			attempts, err := store.Attempts(ctx, "run-1")
			if err != nil {
				t.Fatalf("read attempts: %v", err)
			}
			if len(attempts) != 1 {
				t.Fatalf("the readable part of the row must survive, got %d attempts", len(attempts))
			}
			got := attempts[0]
			if !strings.Contains(got.Error, test.column) {
				t.Fatalf("corruption of %s was concealed, error is %q", test.column, got.Error)
			}
			if !strings.Contains(got.Error, "revert failed") {
				t.Fatalf("the attempt's own error was lost: %q", got.Error)
			}
			if got.PullRequest != 1209 {
				t.Fatalf("the rest of the row did not survive: %+v", got)
			}
			switch test.column {
			case "broken":
				if len(got.Broken) != 0 {
					t.Fatalf("a partially parsed column must not pass as health evidence: %+v", got.Broken)
				}
			case "evidence":
				if len(got.Evidence) != 0 {
					t.Fatalf("a partially parsed column must not pass as evidence: %+v", got.Evidence)
				}
			case "fixes":
				if len(got.Fixes) != 0 {
					t.Fatalf("a partially parsed column must not pass as applied diffs: %+v", got.Fixes)
				}
			}
		})
	}
}

// The history database can hold controller-authored text, which may embed a
// credential-shaped spec.url, so reporting a corrupt column must not quote it.
func TestReportingACorruptColumnDoesNotEchoItsContents(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "history.db")
	const secret = "https://git:ghp_0123456789abcdef@example.com/o/r"
	seedCorruptAttempt(t, path, "evidence", `["`+secret+`", }`)

	store, err := history.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	attempts, err := store.Attempts(ctx, "run-1")
	if err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("want one attempt, got %d", len(attempts))
	}
	if strings.Contains(attempts[0].Error, "ghp_") {
		t.Fatalf("the corrupt column was quoted back: %q", attempts[0].Error)
	}
}

// A corrupt row must not take the rest of the listing down with it.
func TestOneCorruptColumnDoesNotHideTheOtherRuns(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "history.db")
	seedCorruptAttempt(t, path, "broken", `[{"reason":`)

	store, err := history.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	healthy := domain.Run{ID: "run-2", Repository: domain.RepositoryRef{Owner: "a", Name: "b"}, StartedAt: time.Now()}
	if err := store.StartRun(ctx, healthy); err != nil {
		t.Fatalf("start the second run: %v", err)
	}

	runs, err := store.Runs(ctx, 10)
	if err != nil {
		t.Fatalf("a corrupt column must not fail the listing: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("want both runs, got %d", len(runs))
	}
}

func seedCorruptAttempt(t *testing.T, path, column, raw string) {
	t.Helper()
	ctx := context.Background()
	store, err := history.Open(path)
	if err != nil {
		t.Fatalf("open history: %v", err)
	}
	started := time.Date(2026, 8, 2, 14, 32, 0, 0, time.UTC)
	run := domain.Run{ID: "run-1", Repository: domain.RepositoryRef{Owner: "a", Name: "b"}, StartedAt: started}
	if err := store.StartRun(ctx, run); err != nil {
		t.Fatalf("start run: %v", err)
	}
	attempt := domain.Attempt{
		RunID:       "run-1",
		PullRequest: 1209,
		Verdict:     domain.VerdictError,
		Error:       "revert failed",
		Broken:      []domain.ObjectHealth{{Ref: domain.ObjectRef{Kind: "HelmRelease", Name: "gatus"}}},
		Evidence:    []string{"the HelmRelease sets values.env"},
		Fixes:       []string{"--- a\n+++ b\n"},
	}
	if err := store.RecordAttempt(ctx, attempt); err != nil {
		t.Fatalf("record attempt: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	direct, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := direct.Exec("UPDATE attempts SET "+column+" = ?", raw); err != nil {
		t.Fatalf("corrupt %s: %v", column, err)
	}
	if err := direct.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}
}

// A database written by an earlier version must keep working.
// A database created before fix_diff was dropped still has the column, and
// SQLite rejects an INSERT that omits a NOT NULL column with no default.
func TestAnAttemptStillRecordsIntoADatabaseThatHasTheRetiredFixDiffColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_, err = old.Exec(`CREATE TABLE runs (
		id TEXT PRIMARY KEY, repository TEXT NOT NULL, mode TEXT NOT NULL,
		started_at TEXT NOT NULL, finished_at TEXT);
	CREATE TABLE attempts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		run_id TEXT NOT NULL REFERENCES runs(id),
		pull_request INTEGER NOT NULL, dependency TEXT NOT NULL, kind TEXT NOT NULL,
		from_version TEXT NOT NULL, to_version TEXT NOT NULL, bump TEXT NOT NULL,
		decision TEXT NOT NULL, reason TEXT NOT NULL DEFAULT '',
		changelog_source TEXT NOT NULL DEFAULT '', merge_sha TEXT NOT NULL DEFAULT '',
		watch TEXT NOT NULL DEFAULT '', verdict TEXT NOT NULL,
		fix_diff TEXT NOT NULL DEFAULT '',
		fix_attempts INTEGER NOT NULL DEFAULT 0, revert_sha TEXT NOT NULL DEFAULT '',
		duration_ms INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '')`)
	if err != nil {
		t.Fatalf("create the old schema: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	store, err := history.Open(path)
	if err != nil {
		t.Fatalf("open an older database: %v", err)
	}
	defer func() { _ = store.Close() }()

	ctx := context.Background()
	started := time.Date(2026, 8, 2, 14, 32, 0, 0, time.UTC)
	run := domain.Run{
		ID:         "run-legacy",
		Repository: domain.RepositoryRef{Owner: "acme", Name: "cluster"},
		Mode:       "unattended",
		StartedAt:  started,
	}
	if err := store.StartRun(ctx, run); err != nil {
		t.Fatalf("start run: %v", err)
	}
	attempt := domain.Attempt{
		RunID:       run.ID,
		PullRequest: 7,
		Dependency:  domain.Dependency{Name: "gatus", Kind: domain.DependencyKindHelm},
		Decision:    domain.DecideMerge,
		Verdict:     domain.VerdictFixed,
		Fixes:       []string{"--- a/x\n+++ b/x\n"},
		StartedAt:   started,
		FinishedAt:  started.Add(time.Minute),
	}
	if err := store.RecordAttempt(ctx, attempt); err != nil {
		t.Fatalf("record an attempt into a legacy database: %v", err)
	}
	attempts, err := store.Attempts(ctx, run.ID)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("read back: %+v err=%v", attempts, err)
	}
	if got := attempts[0].Fixes; len(got) != 1 || got[0] != "--- a/x\n+++ b/x\n" {
		t.Fatalf("fixes = %q, want the diff read back from the fixes column", got)
	}

	direct, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer func() { _ = direct.Close() }()
	var legacy string
	if err := direct.QueryRow("SELECT fix_diff FROM attempts").Scan(&legacy); err != nil {
		t.Fatalf("read the retired column: %v", err)
	}
	if legacy != "" {
		t.Fatalf("fix_diff = %q, want the retired column left at its default", legacy)
	}
}

func TestOpenMigratesAnOlderDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_, err = old.Exec(`CREATE TABLE runs (
		id TEXT PRIMARY KEY, repository TEXT NOT NULL, mode TEXT NOT NULL,
		started_at TEXT NOT NULL, finished_at TEXT)`)
	if err != nil {
		t.Fatalf("create the old schema: %v", err)
	}
	if _, err := old.Exec(
		`INSERT INTO runs VALUES ('run-0','a/b','interactive','2026-08-01T00:00:00Z',NULL)`,
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	store, err := history.Open(path)
	if err != nil {
		t.Fatalf("open an older database: %v", err)
	}
	defer func() { _ = store.Close() }()

	if err := store.FinishRun(context.Background(), "run-0", time.Now(), "halted"); err != nil {
		t.Fatalf("write to the migrated database: %v", err)
	}
	runs, err := store.Runs(context.Background(), 10)
	if err != nil || len(runs) != 1 || runs[0].Halted != "halted" {
		t.Fatalf("migrated row: %+v err=%v", runs, err)
	}
}
