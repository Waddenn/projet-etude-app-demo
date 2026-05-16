package main

import (
	"context"
	"errors"
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

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://app:app@localhost:5432/app?sslmode=disable"
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	shutdownTracer, err := otelinit.Setup(ctx, otelinit.Config{ServiceName: "projet-etude-app-demo-api"})
	if err != nil {
		slog.Error("otel setup", "err", err)
		os.Exit(1)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTracer(ctx)
	}()

	pool, err := dbutil.Open(ctx, dsn)
	if err != nil {
		slog.Error("db connect", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := store.Migrate(ctx, pool); err != nil {
		slog.Error("migrate", "err", err)
		os.Exit(1)
	}

	st := store.New(pool)
	if err := httpapi.RefreshOpenGauge(ctx, st); err != nil {
		slog.Warn("initial open-gauge refresh failed", "err", err)
	}

	authCfg := auth.ConfigFromEnv()
	authenticator, err := auth.New(ctx, authCfg)
	if err != nil {
		slog.Error("auth init", "err", err)
		os.Exit(1)
	}
	login, err := auth.NewLogin(ctx, authCfg)
	if err != nil {
		slog.Error("login init", "err", err)
		os.Exit(1)
	}
	if !authCfg.Enabled {
		slog.Warn("OIDC disabled — every request authenticated as dev/operator")
	}

	srv := &http.Server{
		Addr:              ":8080",
		Handler:           httpapi.NewMux(st, authenticator, login),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		slog.Info("api listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown", "err", err)
	}
}
