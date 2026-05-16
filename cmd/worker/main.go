package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Waddenn/projet-etude-app-demo/internal/dbutil"
	"github.com/Waddenn/projet-etude-app-demo/internal/jobworker"
	"github.com/Waddenn/projet-etude-app-demo/internal/otelinit"
	"github.com/Waddenn/projet-etude-app-demo/internal/store"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("worker fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://app:app@localhost:5432/app?sslmode=disable"
	}
	metricsAddr := os.Getenv("METRICS_ADDR")
	if metricsAddr == "" {
		metricsAddr = ":8081"
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	shutdownTracer, err := otelinit.Setup(ctx, otelinit.Config{ServiceName: "projet-etude-app-demo-worker"})
	if err != nil {
		return fmt.Errorf("otel setup: %w", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTracer(ctx)
	}()

	pool, err := dbutil.Open(ctx, dsn)
	if err != nil {
		return fmt.Errorf("db connect: %w", err)
	}
	defer pool.Close()

	// Migrations idempotentes : un worker isolé doit aussi pouvoir démarrer le schéma
	// (utile pour les tests / déploiement où l'api n'a pas encore tourné).
	if err := store.Migrate(ctx, pool); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	st := store.New(pool)
	w := jobworker.New(st, pool)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(rw http.ResponseWriter, _ *http.Request) {
		_, _ = rw.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(rw http.ResponseWriter, r *http.Request) {
		c, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := st.Ping(c); err != nil {
			http.Error(rw, err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = rw.Write([]byte("ready\n"))
	})
	mux.Handle("GET /metrics", promhttp.Handler())

	srv := &http.Server{
		Addr:              metricsAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		slog.Info("worker metrics listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	workerErr := make(chan error, 1)
	go func() { workerErr <- w.Run(ctx) }()

	select {
	case <-ctx.Done():
	case err := <-serverErr:
		return fmt.Errorf("metrics server: %w", err)
	case err := <-workerErr:
		if err != nil {
			slog.Error("worker stopped", "err", err)
		}
	}

	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	return nil
}
