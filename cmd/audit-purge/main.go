// Petit binaire utilitaire : exécute `DELETE FROM audit_log WHERE ts < now()-X`.
// Lancé par un CronJob Kubernetes (rétention RGPD).
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/Waddenn/projet-etude-app-demo/internal/dbutil"
	"github.com/Waddenn/projet-etude-app-demo/internal/store"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("audit-purge failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	days := 90
	if v := os.Getenv("AUDIT_RETENTION_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			days = n
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := dbutil.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	st := store.New(pool)
	n, err := st.PurgeAuditOlderThan(ctx, time.Duration(days)*24*time.Hour)
	if err != nil {
		return err
	}
	slog.Info("audit purge done", "retention_days", days, "rows_deleted", n)
	return nil
}
