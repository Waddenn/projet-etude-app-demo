package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Waddenn/projet-etude-app-demo/internal/metrics"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	Pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store { return &Store{Pool: pool} }

var migrations = []string{
	`CREATE TABLE IF NOT EXISTS tickets (
        id          BIGSERIAL PRIMARY KEY,
        title       TEXT      NOT NULL CHECK (length(title) > 0 AND length(title) <= 200),
        description TEXT      NOT NULL DEFAULT '',
        priority    TEXT      NOT NULL DEFAULT 'medium'
                              CHECK (priority IN ('low','medium','high')),
        status      TEXT      NOT NULL DEFAULT 'open'
                              CHECK (status IN ('open','closed')),
        created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
        updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
    )`,
	`CREATE INDEX IF NOT EXISTS tickets_status_created_at_idx
        ON tickets (status, created_at DESC)`,

	`CREATE TABLE IF NOT EXISTS jobs (
        id           BIGSERIAL PRIMARY KEY,
        kind         TEXT       NOT NULL,
        payload      JSONB      NOT NULL DEFAULT '{}'::jsonb,
        status       TEXT       NOT NULL DEFAULT 'pending'
                                CHECK (status IN ('pending','running','done','failed')),
        attempts     INT        NOT NULL DEFAULT 0,
        max_attempts INT        NOT NULL DEFAULT 5,
        last_error   TEXT,
        run_after    TIMESTAMPTZ NOT NULL DEFAULT now(),
        locked_at    TIMESTAMPTZ,
        locked_by    TEXT,
        created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
        updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
    )`,
	`CREATE INDEX IF NOT EXISTS jobs_ready_idx
        ON jobs (run_after) WHERE status = 'pending'`,
	`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS trace_context JSONB`,

	`CREATE TABLE IF NOT EXISTS audit_log (
        id            BIGSERIAL PRIMARY KEY,
        ts            TIMESTAMPTZ NOT NULL DEFAULT now(),
        user_sub      TEXT        NOT NULL DEFAULT 'anonymous',
        user_email    TEXT,
        action        TEXT        NOT NULL,
        resource_type TEXT,
        resource_id   TEXT,
        ip            INET,
        user_agent    TEXT,
        trace_id      TEXT,
        result        TEXT        NOT NULL DEFAULT 'ok'
                                    CHECK (result IN ('ok','denied','error'))
    )`,
	`CREATE INDEX IF NOT EXISTS audit_log_ts_idx ON audit_log (ts DESC)`,
	`CREATE INDEX IF NOT EXISTS audit_log_user_idx ON audit_log (user_sub, ts DESC)`,

	`CREATE OR REPLACE FUNCTION notify_jobs() RETURNS trigger AS $$
        BEGIN
          PERFORM pg_notify('jobs', NEW.kind);
          RETURN NEW;
        END;
     $$ LANGUAGE plpgsql`,
	`DROP TRIGGER IF EXISTS jobs_notify ON jobs`,
	`CREATE TRIGGER jobs_notify
        AFTER INSERT ON jobs
        FOR EACH ROW EXECUTE FUNCTION notify_jobs()`,
}

func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	for _, stmt := range migrations {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("migration failed (%.40s...): %w", stmt, err)
		}
	}
	return nil
}

func (s *Store) Ping(ctx context.Context) error {
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return s.Pool.Ping(c)
}

func (s *Store) timed(ctx context.Context, op string, start time.Time) {
	metrics.ObserveQueryCtx(ctx, op, start)
}
