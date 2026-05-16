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

	"github.com/Waddenn/projet-etude-app-demo/internal/auth"
	"github.com/Waddenn/projet-etude-app-demo/internal/dbutil"
	"github.com/Waddenn/projet-etude-app-demo/internal/httpapi"
	"github.com/Waddenn/projet-etude-app-demo/internal/otelinit"
	"github.com/Waddenn/projet-etude-app-demo/internal/store"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("api fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://app:app@localhost:5432/app?sslmode=disable"
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	shutdownTracer, err := otelinit.Setup(ctx, otelinit.Config{ServiceName: "projet-etude-app-demo-api"})
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

	if err := store.Migrate(ctx, pool); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	st := store.New(pool)
	if err := httpapi.RefreshOpenGauge(ctx, st); err != nil {
		slog.Warn("initial open-gauge refresh failed", "err", err)
	}

	authCfg := auth.ConfigFromEnv()
	authenticator, err := auth.New(ctx, authCfg)
	if err != nil {
		return fmt.Errorf("auth init: %w", err)
	}
	login, err := auth.NewLogin(ctx, authCfg)
	if err != nil {
		return fmt.Errorf("login init: %w", err)
	}
	if !authCfg.Enabled {
		slog.Warn("OIDC disabled — every request authenticated as dev/operator")
	}

	srv := &http.Server{
		Addr:              ":8080",
		Handler:           httpapi.NewMux(st, authenticator, login),
		ReadHeaderTimeout: 5 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		slog.Info("api listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		return fmt.Errorf("server: %w", err)
	case <-ctx.Done():
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("shutdown", "err", err)
		}
	}
	return nil
}
