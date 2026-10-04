package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"seat-reservation/internal/auth"
	"seat-reservation/internal/config"
	"seat-reservation/internal/httpapi"
	"seat-reservation/internal/logging"
	"seat-reservation/internal/store"
	"seat-reservation/migrations"
)

func main() {
	cfg, err := config.Load()
	log := logging.New(cfg.LogLevel)
	if err != nil {
		log.Error("invalid config", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := store.Open(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		log.Error("database config", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	// HTTP comes up immediately so /livez answers during a cold start; /readyz
	// and the API stay 503 until the database is reachable and migrated.
	var ready atomic.Bool
	go func() {
		if err := store.WaitForDB(ctx, pool, log); err != nil {
			return // shutting down
		}
		backoff := time.Second
		for {
			err := store.Migrate(ctx, pool, migrations.FS, log)
			if err == nil {
				break
			}
			log.Error("migration failed; retrying", "err", err, "retry_in", backoff.String())
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
		}
		ready.Store(true)
		log.Info("ready")
	}()

	api := httpapi.New(store.New(pool), auth.New(cfg.JWTSecret, cfg.AdminAPIKey, cfg.TokenTTL), ready.Load)
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           api.Handler(log),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		log.Info("listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("listen", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown", "err", err)
		os.Exit(1)
	}
}
