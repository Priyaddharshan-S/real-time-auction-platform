package store

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"time"
)

// Arbitrary constant key for the migration advisory lock.
const migrateLockKey = 727274

// Migrate applies every embedded *.sql file (sorted by name) that is not yet
// recorded in schema_migrations. Each file runs in its own transaction with a
// transaction-level advisory lock, so two Render instances booting at once
// cannot apply the same migration twice (xact locks are pooler-safe).
func (s *Store) Migrate(ctx context.Context, fsys fs.FS) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	if _, err := s.DB.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	files, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)

	for _, name := range files {
		sqlBytes, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		tx, err := s.DB.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrateLockKey); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("lock: %w", err)
		}
		var done bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, name).Scan(&done); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if done {
			_ = tx.Rollback(ctx)
			continue
		}
		if _, err := tx.Exec(ctx, string(sqlBytes)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		slog.Info("migration applied", "version", name)
	}
	return nil
}
