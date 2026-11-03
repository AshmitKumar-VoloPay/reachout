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

	"github.com/ashmit/reachout-server/internal/accounts"
	"github.com/ashmit/reachout-server/internal/auth"
	"github.com/ashmit/reachout-server/internal/config"
	"github.com/ashmit/reachout-server/internal/db"
	"github.com/ashmit/reachout-server/internal/httpapi"
	"github.com/ashmit/reachout-server/internal/leads"
	"github.com/ashmit/reachout-server/internal/linkedin"
	"github.com/ashmit/reachout-server/migrations"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	if cfg.LinkedAPIToken == "" {
		return errors.New("LINKED_API_TOKEN is required")
	}
	if cfg.TokenKey == nil {
		return errors.New("TOKEN_ENCRYPTION_KEY is required (openssl rand -base64 32)")
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	applied, err := db.Migrate(ctx, pool, migrations.FS)
	if err != nil {
		return err
	}
	log.Info("migrations", "applied", applied)

	mux, handler := httpapi.NewRouter(httpapi.Deps{DB: pool, Log: log, AllowedOrigins: cfg.AppOrigins})
	authH := auth.NewHandler(auth.NewService(pool), auth.Options{CookieSecure: cfg.CookieSecure, Log: log})
	authH.Register(mux)

	cipher, err := accounts.NewCipher(cfg.TokenKey)
	if err != nil {
		return fmt.Errorf("TOKEN_ENCRYPTION_KEY: %w", err)
	}
	li := linkedin.NewHTTPClient(cfg.LinkedAPIBaseURL, cfg.LinkedAPIToken)
	accountsSvc := accounts.NewService(pool, li, cipher, log)
	accounts.NewHandler(accountsSvc, authH, log).Register(mux)
	leads.NewHandler(leads.NewService(pool, li, accountsSvc, log), authH, log).Register(mux)
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(sc)
	}()

	log.Info("listening", "addr", cfg.Addr, "worker_enabled", cfg.WorkerEnabled)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
