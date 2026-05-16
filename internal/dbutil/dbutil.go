package dbutil

import (
	"context"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Open ouvre un pool pgx en attendant jusqu'à 30s que Postgres soit prêt.
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 10
	cfg.MinConns = 1
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.ConnConfig.Tracer = otelpgx.NewTracer(
		otelpgx.WithTrimSQLInSpanName(),
	)

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := pool.Ping(pingCtx); err == nil {
			return pool, nil
		} else if time.Now().After(deadline) {
			pool.Close()
			return nil, err
		}
		time.Sleep(500 * time.Millisecond)
	}
}
