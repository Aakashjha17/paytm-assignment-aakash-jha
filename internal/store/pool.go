package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("not found")

// Store wraps the connection pool; all SQL lives in this package.
type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Open builds a pool without dialing. pgxpool connects lazily, so this
// succeeds even if the database is not up yet; use WaitForDB to block on it.
func Open(ctx context.Context, url string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	cfg.MaxConns = maxConns
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 15 * time.Second
	return pgxpool.NewWithConfig(ctx, cfg)
}

// WaitForDB pings until the database answers or ctx is cancelled, backing
// off exponentially up to 5s. Covers cold starts where the app boots before
// Postgres (or its private network) is reachable.
func WaitForDB(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) error {
	backoff := 250 * time.Millisecond
	for attempt := 1; ; attempt++ {
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := pool.Ping(pingCtx)
		cancel()
		if err == nil {
			log.Info("database reachable", "attempts", attempt)
			return nil
		}
		log.Warn("database not reachable yet", "attempt", attempt, "err", err, "retry_in", backoff.String())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Second)
	}
}

// PoolStat exposes connection-pool counters for metrics.
func (s *Store) PoolStat() *pgxpool.Stat { return s.pool.Stat() }

// Ping is the readiness probe's dependency check.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }
