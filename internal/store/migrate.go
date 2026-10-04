package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationLockID is an arbitrary constant shared by every replica so only one
// of them runs migrations at a time; the others block until it finishes and
// then find nothing left to apply.
const migrationLockID int64 = 0x5EA7_0001

// Migrate applies every *.sql file in fsys, in lexical order, that is not yet
// recorded in schema_migrations. Each file runs in its own transaction together
// with its bookkeeping row, so a failure leaves no half-applied migration.
// Files that are empty (only whitespace) are skipped and NOT recorded, so a
// placeholder can be filled in later and will still be applied.
func Migrate(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS, log *slog.Logger) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire: %w", err)
	}
	defer conn.Release()

	// Session-level lock on a dedicated connection; released explicitly below,
	// and implicitly by Postgres if this process dies mid-migration.
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockID); err != nil {
		return fmt.Errorf("advisory lock: %w", err)
	}
	defer conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLockID) //nolint:errcheck

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			checksum   TEXT NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied := map[string]string{}
	rows, err := conn.Query(ctx, "SELECT version, checksum FROM schema_migrations")
	if err != nil {
		return fmt.Errorf("read schema_migrations: %w", err)
	}
	for rows.Next() {
		var v, sum string
		if err := rows.Scan(&v, &sum); err != nil {
			return err
		}
		applied[v] = sum
	}
	if err := rows.Err(); err != nil {
		return err
	}

	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)

	ran := 0
	for _, name := range names {
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(body)) == "" {
			log.Debug("migration empty, skipping", "version", name)
			continue
		}
		sum := checksum(body)
		if prev, ok := applied[name]; ok {
			if prev != sum {
				// Editing an applied migration never takes effect; say so loudly
				// instead of letting the schema silently drift from the repo.
				log.Warn("applied migration has changed on disk; not re-running", "version", name)
			}
			continue
		}
		if err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(body)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version, checksum) VALUES ($1, $2)", name, sum)
			return err
		}); err != nil {
			return fmt.Errorf("apply %s: %w", name, err)
		}
		log.Info("migration applied", "version", name)
		ran++
	}
	log.Info("migrations complete", "applied", ran, "total_recorded", len(applied)+ran)
	return nil
}

func checksum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
