package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"seat-reservation/internal/audit"
	"seat-reservation/internal/auth"
	"seat-reservation/internal/config"
	"seat-reservation/internal/httpapi"
	"seat-reservation/internal/logging"
	"seat-reservation/internal/metrics"
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

	// ctx is cancelled on SIGTERM/SIGINT; background work watches it. Request
	// contexts are separate, so cancelling it does not abort in-flight requests.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := store.Open(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		log.Error("database config", "err", err)
		os.Exit(1)
	}
	st := store.New(pool)

	var migrated, draining atomic.Bool
	m := metrics.New()
	m.Register(metrics.NewSeatCollector(st), metrics.NewPoolCollector(st))
	m.RegisterReady(func() bool { return migrated.Load() && !draining.Load() })
	auditor := audit.NewRunner(st, m, log, cfg.AuditInterval)

	// HTTP comes up immediately so /livez answers during a cold start; /readyz
	// and the API stay 503 until the database is reachable and migrated.
	var background sync.WaitGroup
	background.Add(1)
	go func() {
		defer background.Done()
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
		migrated.Store(true)
		log.Info("ready")
		auditor.Run(ctx) // until shutdown
	}()

	api := httpapi.New(httpapi.Deps{
		Store:    st,
		Auth:     auth.New(cfg.JWTSecret, cfg.AdminAPIKey, cfg.TokenTTL),
		Metrics:  m,
		Migrated: migrated.Load,
		Draining: draining.Load,
	})
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           api.Handler(log),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
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
	stop() // a second signal now kills the process immediately

	// Shutdown, in order: stop advertising readiness, keep serving while
	// traffic moves away, finish in-flight requests, then close the database.
	draining.Store(true)
	log.Info("shutdown 1/4: signal received, /readyz now 503", "drain_delay", cfg.DrainDelay.String(), "in_flight", m.InFlight())
	time.Sleep(cfg.DrainDelay)

	log.Info("shutdown 2/4: closing listener, draining in-flight requests", "in_flight", m.InFlight(), "timeout", cfg.ShutdownTimeout.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	exitCode := 0
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown: in-flight requests did not finish in time", "err", err, "in_flight", m.InFlight())
		exitCode = 1
	} else {
		log.Info("shutdown 3/4: http drained", "in_flight", m.InFlight())
	}

	background.Wait() // migration/audit goroutine exits on ctx
	pool.Close()
	log.Info("shutdown 4/4: database pool closed; exiting")
	os.Exit(exitCode)
}
