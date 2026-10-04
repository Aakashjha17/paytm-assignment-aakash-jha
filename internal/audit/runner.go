// Package audit periodically re-checks the booking invariants against the
// database and publishes the result as the audit_mismatches gauge.
package audit

import (
	"context"
	"log/slog"
	"time"

	"seat-reservation/internal/metrics"
	"seat-reservation/internal/store"
)

type Runner struct {
	store    *store.Store
	metrics  *metrics.Metrics
	log      *slog.Logger
	interval time.Duration
}

func NewRunner(st *store.Store, m *metrics.Metrics, log *slog.Logger, interval time.Duration) *Runner {
	m.InitAuditChecks(store.AuditCheckNames())
	return &Runner{store: st, metrics: m, log: log, interval: interval}
}

// Run audits once immediately, then every interval, until ctx is cancelled.
func (r *Runner) Run(ctx context.Context) {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		r.RunOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (r *Runner) RunOnce(parent context.Context) store.AuditResult {
	start := time.Now()
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	res, err := r.store.Audit(ctx)
	if err != nil {
		if parent.Err() != nil {
			return res // shutting down; not a failure
		}
		r.metrics.AuditFailed()
		r.log.Error("audit failed to run", "err", err)
		return res
	}
	r.metrics.SetAudit(res.Mismatches, float64(time.Now().Unix()))
	if total := res.Total(); total > 0 {
		r.log.Error("audit found invariant violations",
			"total", total, "mismatches", res.Mismatches, "samples", res.Samples,
			"duration_ms", time.Since(start).Milliseconds())
	} else {
		r.log.Debug("audit clean", "duration_ms", time.Since(start).Milliseconds())
	}
	return res
}
