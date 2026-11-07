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

	"github.com/joho/godotenv"

	"github.com/ashmit/reachout-server/internal/accounts"
	"github.com/ashmit/reachout-server/internal/auth"
	"github.com/ashmit/reachout-server/internal/campaigns"
	"github.com/ashmit/reachout-server/internal/config"
	"github.com/ashmit/reachout-server/internal/httpapi"
	"github.com/ashmit/reachout-server/internal/inbox"
	"github.com/ashmit/reachout-server/internal/leads"
	"github.com/ashmit/reachout-server/internal/linkedin"
	"github.com/ashmit/reachout-server/internal/mdb"
	"github.com/ashmit/reachout-server/internal/webhooks"
	"github.com/ashmit/reachout-server/internal/worker"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	// Dev convenience: read .env if present. Variables already set in the environment win, so production is unaffected.
	_ = godotenv.Load()
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

	database, err := mdb.Open(ctx, cfg.MongoURI, cfg.MongoDB)
	if err != nil {
		return err
	}
	defer database.Client().Disconnect(context.Background())

	// Indexes replace SQL migrations. Several are guarantees the code relies on (one LinkedIn
	// task at a time per account, one connection in flight), so the server will not start without them.
	if err := mdb.EnsureIndexes(ctx, database); err != nil {
		return err
	}
	log.Info("database ready", "db", cfg.MongoDB)

	mux, handler := httpapi.NewRouter(httpapi.Deps{DB: mdb.Pinger{DB: database}, Log: log, AllowedOrigins: cfg.AppOrigins})
	authH := auth.NewHandler(auth.NewService(database), auth.Options{CookieSecure: cfg.CookieSecure, Log: log})
	authH.Register(mux)

	cipher, err := accounts.NewCipher(cfg.TokenKey)
	if err != nil {
		return fmt.Errorf("TOKEN_ENCRYPTION_KEY: %w", err)
	}
	li := linkedin.NewHTTPClient(cfg.LinkedAPIBaseURL, cfg.LinkedAPIToken)
	accountsSvc := accounts.NewService(database, li, cipher, log)
	accounts.NewHandler(accountsSvc, authH, log).Register(mux)
	leads.NewHandler(leads.NewService(database, li, accountsSvc, log), authH, log).Register(mux)
	campaignsSvc := campaigns.NewService(database, accountsSvc, log)
	campaigns.NewHandler(campaignsSvc, authH, log).Register(mux)

	inbox.NewHandler(inbox.NewService(database), authH, log).Register(mux)

	// Linked API calls this endpoint when someone accepts a request or replies. It checks the
	// signature itself, so it sits outside the cookie-based auth.
	if cfg.WebhookSecret == "" {
		log.Warn("LINKED_API_WEBHOOK_SECRET is not set: /webhooks/linkedapi will refuse all events, so accepted requests and replies will not be noticed")
	}
	webhooks.New(database, campaignsSvc, cfg.WebhookSecret, log).Register(mux)
	go webhooks.NewReconciler(database, li, log).Run(ctx, 10*time.Minute)

	// The worker is the only code that acts on LinkedIn accounts. WORKER_ENABLED=false
	// leaves the API running but sends nothing.
	workerCfg := worker.DefaultConfig()
	workerCfg.Enabled = cfg.WorkerEnabled
	go worker.New(database, accountsSvc, campaignsSvc, li, workerCfg, log).Run(ctx)
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
