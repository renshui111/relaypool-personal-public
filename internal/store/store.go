// Package store owns the SQLite database: schema migrations, account
// persistence, API keys and request logging.
//
// The driver is modernc.org/sqlite (pure Go) rather than mattn/go-sqlite3 so
// the binary stays CGO-free: that is what allows a static build and a tiny
// Alpine image with no libc shims.
package store

import (
	"context"
	"crypto/cipher"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store is a handle to the relaypool database.
type Store struct {
	db               *sql.DB
	path             string
	credentialCipher cipher.AEAD
}

// ErrNotFound is returned when a lookup matches no row. Callers compare with
// errors.Is so a wrapping fmt.Errorf still classifies correctly.
var ErrNotFound = errors.New("not found")

// Open opens (creating if needed) the SQLite database at path, applies any
// pending migrations, and returns a ready handle.
//
// The parent directory is created with 0700 and the database file with 0600
// because it holds provider credentials.
func Open(ctx context.Context, path string) (*Store, error) {
	return OpenWithCredentialKey(ctx, path, nil)
}

// OpenWithCredentialKey 用独立的 32 字节密钥保护凭证，原 Open 保持测试兼容。
func OpenWithCredentialKey(ctx context.Context, path string, key []byte) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("store: path must not be empty")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("store: create data dir %s: %w", dir, err)
	}

	// _txlock=immediate makes every transaction take the write lock up
	// front, which avoids the "database is locked" upgrade deadlock that
	// SQLite hits when two deferred transactions try to write.
	// _busy_timeout gives the writer a grace period instead of failing fast.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_txlock=immediate",
		filepath.ToSlash(path))

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// SQLite tolerates exactly one writer. Capping the pool keeps
	// contention predictable and avoids surprising lock errors under load.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(time.Hour)

	// A ping here proves the file is usable, but it must TOLERATE a busy
	// database: another process starting at the same moment can hold the write
	// lock while it runs its own migration, and failing the whole Open on
	// SQLITE_BUSY would refuse to start a process that is merely second in line.
	//
	// The busy_timeout pragma covers ordinary statement contention, but the
	// initial connection setup can still observe SQLITE_BUSY (measured with four
	// concurrent Opens). Retrying briefly turns a spurious startup failure into
	// a short wait, which is what "another instance is starting" deserves.
	if err := pingWithRetry(ctx, db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: ping %s: %w", path, err)
	}

	s := &Store{db: db, path: path}
	if err := s.initCredentialCipher(key); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.migrateCredentialEncryption(ctx); err != nil {
		db.Close()
		return nil, err
	}
	// Best-effort: tighten the file mode after SQLite created it.
	_ = os.Chmod(path, 0o600)
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// pingWithRetry pings the database, tolerating a transient lock.
//
// A concurrent startup is not an error condition: it means another process is
// migrating the same file, and waiting a moment is correct. Only a persistent
// failure (or a cancelled context) is reported.
func pingWithRetry(ctx context.Context, db *sql.DB) error {
	const attempts = 10
	var err error
	for i := 0; i < attempts; i++ {
		if err = db.PingContext(ctx); err == nil {
			return nil
		}
		// A cancelled or expired caller cannot be helped by waiting.
		if ctx.Err() != nil {
			return err
		}
		// Back off briefly; the holder only needs to finish its migration.
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Duration(20*(i+1)) * time.Millisecond):
		}
	}
	return err
}

// Path reports the database file location.
func (s *Store) Path() string { return s.path }

// 就绪检查只确认数据库连接，不公开账号数量或读取凭证。
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// migrate applies pending migrations, verifying the checksum of each one that
// was already applied.
//
// A checksum mismatch is a hard failure, not a warning: it means the schema
// history was edited after shipping, and silently continuing would run new
// code against an unknown schema.
//
// # Concurrency
//
// Two processes starting against the same database can both observe a migration
// as pending and both try to apply it. The loser hit a UNIQUE violation on
// schema_migrations and Open returned an error, so the process refused to start
// -- measured at roughly 5.6% of runs with three concurrent starts. The
// database itself was never corrupted, only the startup.
//
// The fix is to tolerate the loss explicitly: if the ledger row already exists
// when we try to record it, another process applied the same migration and we
// must verify its checksum rather than fail. That check is what keeps the
// tolerance from masking a real problem -- a mismatch is still fatal.
func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS schema_migrations (
  filename   TEXT PRIMARY KEY,
  checksum   TEXT NOT NULL,
  applied_at TEXT NOT NULL
)`); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}

	applied, err := s.appliedMigrations(ctx)
	if err != nil {
		return err
	}

	for _, m := range migrations {
		sum := checksumOf(m.sql)
		if previous, ok := applied[m.filename]; ok {
			if previous != sum {
				return fmt.Errorf(
					"store: migration %s checksum mismatch (stored %s, computed %s): "+
						"an already-applied migration was edited; append a new migration instead",
					m.filename, previous, sum)
			}
			continue
		}
		if err := s.applyMigration(ctx, m, sum); err != nil {
			// Another process may have won the race between our ledger read and
			// this apply. Re-read the ledger: if it now records the migration
			// with OUR checksum, the schema is exactly what we intended and the
			// only thing lost was the race to write the row.
			if concurrent, verifyErr := s.confirmConcurrentApply(ctx, m.filename, sum); verifyErr != nil {
				return verifyErr
			} else if concurrent {
				continue
			}
			return err
		}
	}
	return nil
}

// confirmConcurrentApply reports whether the migration is now recorded with the
// expected checksum, which means another process applied it while we raced.
//
// The error return is non-nil only for a genuine problem: a checksum mismatch,
// or a ledger that cannot be read. A merely-absent row returns (false, nil) so
// the caller can surface its original error.
func (s *Store) confirmConcurrentApply(ctx context.Context, filename, want string) (bool, error) {
	var stored string
	err := s.db.QueryRowContext(ctx,
		`SELECT checksum FROM schema_migrations WHERE filename = ?`, filename).Scan(&stored)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Nobody applied it, so the failure was real.
			return false, nil
		}
		return false, fmt.Errorf("store: re-read migration %s after a failed apply: %w", filename, err)
	}
	if stored != want {
		return false, fmt.Errorf(
			"store: migration %s is recorded with checksum %s but this build expects %s: "+
				"the schema was migrated by a different version",
			filename, stored, want)
	}
	return true, nil
}

func (s *Store) appliedMigrations(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT filename, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("store: read schema_migrations: %w", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var name, sum string
		if err := rows.Scan(&name, &sum); err != nil {
			return nil, fmt.Errorf("store: scan schema_migrations: %w", err)
		}
		out[name] = sum
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate schema_migrations: %w", err)
	}
	return out, nil
}

// applyMigration runs one migration and records its checksum in the same
// transaction, so a crash mid-migration cannot leave the ledger claiming a
// migration ran when it did not.
func (s *Store) applyMigration(ctx context.Context, m migration, sum string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin migration %s: %w", m.filename, err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("store: apply migration %s: %w", m.filename, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (filename, checksum, applied_at) VALUES (?, ?, ?)`,
		m.filename, sum, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("store: record migration %s: %w", m.filename, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit migration %s: %w", m.filename, err)
	}
	return nil
}

func checksumOf(sql string) string {
	sum := sha256.Sum256([]byte(sql))
	return hex.EncodeToString(sum[:])
}

// SchemaVersion reports the number of applied migrations, which is enough to
// tell an operator whether a database is current.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		return 0, fmt.Errorf("store: count migrations: %w", err)
	}
	return count, nil
}

// now returns the canonical timestamp format used across every table.
func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// formatTime renders a time in the canonical column format. A zero time
// becomes an empty string, which is how "unset" is stored.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// parseTime reads a canonical timestamp. An empty or unparseable value
// yields the zero time rather than an error, because a malformed timestamp in
// a log row must not break a read path.
func parseTime(raw string) time.Time {
	text := strings.TrimSpace(raw)
	if text == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		if t2, err2 := time.Parse(time.RFC3339, text); err2 == nil {
			return t2.UTC()
		}
		return time.Time{}
	}
	return t.UTC()
}
