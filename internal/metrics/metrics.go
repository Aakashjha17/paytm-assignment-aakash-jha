package metrics

import (
	"net/http"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics owns a private registry (no global state, so tests can build as
// many as they like) and every application metric.
type Metrics struct {
	reg *prometheus.Registry

	reservationsConfirmed prometheus.Counter
	reservationsDeclined  *prometheus.CounterVec
	cancellations         *prometheus.CounterVec

	httpRequests *prometheus.CounterVec
	httpDuration *prometheus.HistogramVec
	inFlight     prometheus.Gauge
	inFlightN    atomic.Int64

	auditMismatches *prometheus.GaugeVec
	auditRuns       prometheus.Counter
	auditErrors     prometheus.Counter
	auditLastOK     prometheus.Gauge
}

func New() *Metrics {
	m := &Metrics{
		reg: prometheus.NewRegistry(),

		reservationsConfirmed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "reservations_confirmed_total",
			Help: "Reserve requests that created a new hold (HTTP 201).",
		}),
		reservationsDeclined: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "reservations_declined_total",
			Help: "Reserve requests that did not create a hold, by reason " +
				"(seat-taken, per-user-limit, idempotent-replay, ...). " +
				"confirmed + sum(declined) == reserve responses.",
		}, []string{"reason"}),
		cancellations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "reservation_cancellations_total",
			Help: "Cancel requests by outcome.",
		}, []string{"outcome"}),

		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "HTTP responses by route pattern and status code.",
		}, []string{"route", "code"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request latency by route pattern.",
			Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"route"}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "http_requests_in_flight",
			Help: "Requests currently being served.",
		}),

		auditMismatches: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "audit_mismatches",
			Help: "Rows breaking each invariant at the last audit. Anything but 0 is a correctness bug.",
		}, []string{"check"}),
		auditRuns: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "audit_runs_total", Help: "Completed audit runs.",
		}),
		auditErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "audit_errors_total", Help: "Audit runs that failed to execute.",
		}),
		auditLastOK: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "audit_last_success_timestamp_seconds",
			Help: "Unix time of the last audit that ran to completion.",
		}),
	}
	m.reg.MustRegister(
		m.reservationsConfirmed, m.reservationsDeclined, m.cancellations,
		m.httpRequests, m.httpDuration, m.inFlight,
		m.auditMismatches, m.auditRuns, m.auditErrors, m.auditLastOK,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// Register adds extra collectors (see collectors.go).
func (m *Metrics) Register(cs ...prometheus.Collector) { m.reg.MustRegister(cs...) }

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{Registry: m.reg})
}

// ObserveReserve records one reserve response under exactly one counter.
func (m *Metrics) ObserveReserve(reason string) {
	if reason == "confirmed" {
		m.reservationsConfirmed.Inc()
		return
	}
	m.reservationsDeclined.WithLabelValues(reason).Inc()
}

func (m *Metrics) ObserveCancel(outcome string) {
	m.cancellations.WithLabelValues(outcome).Inc()
}

func (m *Metrics) ObserveHTTP(route, code string, seconds float64) {
	m.httpRequests.WithLabelValues(route, code).Inc()
	m.httpDuration.WithLabelValues(route).Observe(seconds)
}

func (m *Metrics) RequestStarted()  { m.inFlight.Inc(); m.inFlightN.Add(1) }
func (m *Metrics) RequestFinished() { m.inFlight.Dec(); m.inFlightN.Add(-1) }
func (m *Metrics) InFlight() int64  { return m.inFlightN.Load() }

// RegisterReady exposes app_ready, evaluated at scrape time: 1 once migrated
// and not draining (the DB ping half of /readyz is seats_scrape_success).
func (m *Metrics) RegisterReady(ready func() bool) {
	m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "app_ready", Help: "1 when migrated and not shutting down.",
	}, func() float64 {
		if ready() {
			return 1
		}
		return 0
	}))
}

// SetAudit publishes one audit run; every known check gets a value, so a
// healthy check reads an explicit 0 rather than being absent.
func (m *Metrics) SetAudit(mismatches map[string]int, unixTime float64) {
	for check, n := range mismatches {
		m.auditMismatches.WithLabelValues(check).Set(float64(n))
	}
	m.auditRuns.Inc()
	m.auditLastOK.Set(unixTime)
}

func (m *Metrics) AuditFailed() { m.auditErrors.Inc() }

// InitAuditChecks makes every check visible at 0 before the first run.
func (m *Metrics) InitAuditChecks(names []string) {
	for _, n := range names {
		m.auditMismatches.WithLabelValues(n).Set(0)
	}
}
