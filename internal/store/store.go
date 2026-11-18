// Package store keeps a kb in one SQLite file: pages and every revision of
// them, the links and categories derived from their text, files as
// content-addressed blobs, full-text indexes, and the tasks, notes and change
// feed agents use to coordinate. All SQL in kb lives in this package.
//
// The schema is a sequence of numbered migrations embedded in the binary.
// Opening a database brings it up to the binary's version; a database newer
// than the binary is refused rather than guessed at.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

//go:embed migrations/*.sql
var migrations embed.FS

// Sentinel errors. Callers match them with errors.Is; the wrapped message
// says what was wrong in words an agent can act on.
var (
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("already exists")
	ErrConflict = errors.New("edit conflict")
	ErrInvalid  = errors.New("invalid request")
)

// Store is an open kb database.
type Store struct {
	path string
	w    *sql.DB // the one writer connection
	r    *sql.DB // readers
	log  *slog.Logger
	now  func() time.Time
}

// Options tunes Open.
type Options struct {
	Logger *slog.Logger
	// ReadOnly opens without the writer: no migration, no writes. The schema
	// must already be at this binary's version.
	ReadOnly bool
	// NoBackup skips the copy taken before migrating a database with data.
	NoBackup bool
}

func dsn(path string, readOnly bool) string {
	q := url.Values{}
	for _, p := range []string{"busy_timeout(10000)", "foreign_keys(1)", "cache_size(-32000)", "temp_store(MEMORY)"} {
		q.Add("_pragma", p)
	}
	if readOnly {
		q.Add("_pragma", "query_only(1)")
	} else {
		// journal_size_limit cuts the WAL back after a checkpoint, so a bulk
		// import does not leave a WAL the size of the import behind.
		for _, p := range []string{"journal_mode(WAL)", "synchronous(NORMAL)", "journal_size_limit(67108864)"} {
			q.Add("_pragma", p)
		}
		q.Set("_txlock", "immediate")
	}
	return "file:" + path + "?" + q.Encode()
}

// SchemaVersion is the migration version this binary carries.
func SchemaVersion() int {
	files, _ := migrationFiles()
	if len(files) == 0 {
		return 0
	}
	return files[len(files)-1].version
}

type migrationFile struct {
	version int
	name    string
}

func migrationFiles() ([]migrationFile, error) {
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return nil, err
	}
	var out []migrationFile
	for _, e := range entries {
		num, _, ok := strings.Cut(e.Name(), "_")
		if !ok || !strings.HasSuffix(e.Name(), ".sql") {
			return nil, fmt.Errorf("migration %s: name must be NNNN_description.sql", e.Name())
		}
		v, err := strconv.Atoi(num)
		if err != nil {
			return nil, fmt.Errorf("migration %s: %w", e.Name(), err)
		}
		out = append(out, migrationFile{v, e.Name()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i, m := range out {
		if m.version != i+1 {
			return nil, fmt.Errorf("migrations must be numbered 1..N without gaps: found %s at position %d", m.name, i+1)
		}
	}
	return out, nil
}

// Open opens or creates the kb at path and migrates it to this binary's
// schema.
func Open(path string, opts Options) (*Store, error) {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	s := &Store{path: path, log: opts.Logger, now: time.Now}
	if opts.ReadOnly {
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("open %s: %w", path, err)
		}
	} else {
		if dir := filepath.Dir(path); dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, err
			}
		}
		w, err := sql.Open("sqlite", dsn(path, false))
		if err != nil {
			return nil, err
		}
		// One writer: SQLite serializes writers anyway, and one connection
		// means a write never waits on busy_timeout behind another of ours.
		w.SetMaxOpenConns(1)
		w.SetConnMaxLifetime(0)
		s.w = w
		if err := s.migrate(context.Background(), !opts.NoBackup); err != nil {
			_ = w.Close()
			return nil, err
		}
	}
	r, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		s.Close()
		return nil, err
	}
	r.SetMaxOpenConns(8)
	s.r = r
	var v int
	if err := r.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		s.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if want := SchemaVersion(); v != want {
		s.Close()
		switch {
		case v == 0:
			return nil, fmt.Errorf("%s: not a kb database (open it once without read-only, or run `kb db migrate`)", path)
		case v < want:
			return nil, fmt.Errorf("%s: schema is version %d, this binary needs %d (run `kb db migrate`)", path, v, want)
		default:
			return nil, fmt.Errorf("%s: schema is version %d, newer than this binary's %d (upgrade kb)", path, v, want)
		}
	}
	return s, nil
}

// Path is the database file.
func (s *Store) Path() string { return s.path }

// ReadOnly reports whether the store was opened without a writer.
func (s *Store) ReadOnly() bool { return s.w == nil }

// Close closes the database, running PRAGMA optimize first.
func (s *Store) Close() error {
	var err error
	if s.r != nil {
		err = s.r.Close()
	}
	if s.w != nil {
		_, _ = s.w.Exec("PRAGMA optimize")
		if e := s.w.Close(); err == nil {
			err = e
		}
	}
	return err
}

func (s *Store) migrate(ctx context.Context, backup bool) error {
	var version int
	if err := s.w.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("%s: %w", s.path, err)
	}
	files, err := migrationFiles()
	if err != nil {
		return err
	}
	want := 0
	if len(files) > 0 {
		want = files[len(files)-1].version
	}
	if version > want {
		return fmt.Errorf("%s: schema is version %d, newer than this binary's %d (upgrade kb)", s.path, version, want)
	}
	if version == want {
		return nil
	}
	if version == 0 {
		// Must precede the first table: auto_vacuum cannot be switched on
		// later without a full VACUUM.
		if _, err := s.w.ExecContext(ctx, "PRAGMA auto_vacuum = INCREMENTAL"); err != nil {
			return err
		}
	} else if backup {
		// An existing kb is about to change shape. Keep a copy of it as it was,
		// so a migration that goes wrong costs a restore, not the kb.
		bak := fmt.Sprintf("%s.pre-v%d.bak", s.path, want)
		if _, err := os.Stat(bak); errors.Is(err, os.ErrNotExist) {
			if _, err := s.w.ExecContext(ctx, "VACUUM INTO ?", bak); err != nil {
				return fmt.Errorf("backup before migration: %w", err)
			}
			s.log.Info("backed up before migrating", "path", bak, "from", version, "to", want)
		}
	}
	for _, m := range files {
		if m.version <= version {
			continue
		}
		body, err := migrations.ReadFile("migrations/" + m.name)
		if err != nil {
			return err
		}
		tx, err := s.w.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
		// PRAGMA does not take parameters; the value is an int we produced.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
		s.log.Info("migrated", "version", m.version, "file", m.name)
	}
	return nil
}

// Version reports the database's schema version.
func (s *Store) Version(ctx context.Context) (int, error) {
	var v int
	err := s.r.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	return v, err
}

// Backup writes a consistent copy of the database to dst with VACUUM INTO
// and checks the copy's integrity.
func (s *Store) Backup(ctx context.Context, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("%s already exists", dst)
	}
	db := s.w
	if db == nil {
		db = s.r
	}
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", dst); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	check, err := sql.Open("sqlite", dsn(dst, true))
	if err != nil {
		return err
	}
	defer check.Close()
	var result string
	if err := check.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		return fmt.Errorf("backup integrity check: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("backup integrity check: %s", result)
	}
	return nil
}

// Check runs SQLite's integrity check and the FTS5 integrity checks.
func (s *Store) Check(ctx context.Context) ([]string, error) {
	var problems []string
	rows, err := s.r.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			rows.Close()
			return nil, err
		}
		if r != "ok" {
			problems = append(problems, r)
		}
	}
	rows.Close()
	if s.w != nil {
		for _, t := range []string{"pages_fts", "titles_fts", "notes_fts"} {
			if _, err := s.w.ExecContext(ctx, fmt.Sprintf("INSERT INTO %s(%s, rank) VALUES('integrity-check', 1)", t, t)); err != nil {
				problems = append(problems, fmt.Sprintf("%s: %v", t, err))
			}
		}
	}
	return problems, nil
}

// Vacuum drops blobs no file revision refers to, then reclaims free pages.
func (s *Store) Vacuum(ctx context.Context) (blobsRemoved int64, err error) {
	if err := s.requireWriter(); err != nil {
		return 0, err
	}
	res, err := s.w.ExecContext(ctx, `DELETE FROM blobs WHERE sha256 NOT IN (SELECT sha256 FROM file_revisions)`)
	if err != nil {
		return 0, err
	}
	blobsRemoved, _ = res.RowsAffected()
	for _, t := range []string{"pages_fts", "titles_fts", "notes_fts"} {
		if _, err := s.w.ExecContext(ctx, fmt.Sprintf("INSERT INTO %s(%s) VALUES('optimize')", t, t)); err != nil {
			return blobsRemoved, err
		}
	}
	_, err = s.w.ExecContext(ctx, "VACUUM")
	return blobsRemoved, err
}

func (s *Store) requireWriter() error {
	if s.w == nil {
		return fmt.Errorf("%w: the kb is open read-only", ErrInvalid)
	}
	return nil
}

// write runs fn in one immediate transaction on the writer.
func (s *Store) write(ctx context.Context, fn func(tx *sql.Tx) error) error {
	if err := s.requireWriter(); err != nil {
		return err
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// ms is the store's clock in unix milliseconds.
func (s *Store) ms() int64 { return s.now().UnixMilli() }

// querier is what both *sql.DB and *sql.Tx offer, so read helpers can run
// inside a write transaction or on the reader pool.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// clampLimit bounds a page size.
func clampLimit(n, def, maxN int) int {
	if n <= 0 {
		return def
	}
	return min(n, maxN)
}

// IsNotFound reports whether err is a not-found error.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }
