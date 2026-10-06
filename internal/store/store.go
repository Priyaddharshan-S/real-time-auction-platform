// Package store owns the Postgres pool and Redis client.
package store

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

type Store struct {
	DB    *pgxpool.Pool
	Redis *redis.Client

	mu       sync.Mutex // guards the cached Redis health below
	redisAt  time.Time
	redisErr error
}

// Open connects to Postgres and Redis and verifies both with a ping.
func Open(ctx context.Context, databaseURL, redisURL string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	// Supabase pooler (port 6543, transaction mode) does not support prepared
	// statements, so use the simple protocol (no statement cache).
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	cfg.MaxConns = 10
	cfg.MinConns = 1
	cfg.MaxConnIdleTime = 5 * time.Minute

	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(cctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if err := pool.Ping(cctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	ropts, err := redis.ParseURL(redisURL)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("parse REDIS_URL: %w", err)
	}
	rdb := redis.NewClient(ropts)
	if err := rdb.Ping(cctx).Err(); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return &Store{DB: pool, Redis: rdb}, nil
}

func (s *Store) Close() {
	s.DB.Close()
	_ = s.Redis.Close()
}

// Health pings both backends with a short timeout.
func (s *Store) Health(ctx context.Context) (dbErr, redisErr error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return s.DB.Ping(ctx), s.Redis.Ping(ctx).Err()
}

// HealthCached is for /healthz, which Render and Docker call every few seconds.
// The DB is pinged every time; Redis at most once a minute, so health checks
// do not eat into the Upstash command quota.
func (s *Store) HealthCached(ctx context.Context) (dbErr, redisErr error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	dbErr = s.DB.Ping(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.redisAt) > time.Minute {
		s.redisErr = s.Redis.Ping(ctx).Err()
		s.redisAt = time.Now()
	}
	return dbErr, s.redisErr
}
