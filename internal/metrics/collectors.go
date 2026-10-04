package metrics

import (
	"context"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"seat-reservation/internal/store"
)

// SeatCollector reads seat gauges from the database at scrape time rather
// than tracking them in memory. That way they are the database's own numbers,
// computed with the same expiry rule as GET /shows, so they reconcile with
// the API by construction and survive restarts and multiple replicas.
type SeatCollector struct {
	store *store.Store

	available, held, confirmed, total *prometheus.Desc
	up                                *prometheus.Desc
}

func NewSeatCollector(st *store.Store) *SeatCollector {
	l := []string{"show_id"}
	return &SeatCollector{
		store:     st,
		available: prometheus.NewDesc("seats_available", "Seats available now (expired holds count as available).", l, nil),
		held:      prometheus.NewDesc("seats_held", "Seats under a live hold.", l, nil),
		confirmed: prometheus.NewDesc("seats_confirmed", "Seats sold.", l, nil),
		total:     prometheus.NewDesc("seats_total", "Seats in the show. available + held + confirmed must equal this.", l, nil),
		up:        prometheus.NewDesc("seats_scrape_success", "1 if the seat gauges were read from the database on this scrape.", nil, nil),
	}
}

func (c *SeatCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.available
	ch <- c.held
	ch <- c.confirmed
	ch <- c.total
	ch <- c.up
}

func (c *SeatCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rows, err := c.store.SeatCountsByShow(ctx)
	if err != nil {
		// Report the failure instead of stale or zero gauges.
		ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, 0)
		return
	}
	ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, 1)
	for _, r := range rows {
		id := strconv.FormatInt(r.ShowID, 10)
		ch <- prometheus.MustNewConstMetric(c.available, prometheus.GaugeValue, float64(r.Available), id)
		ch <- prometheus.MustNewConstMetric(c.held, prometheus.GaugeValue, float64(r.Held), id)
		ch <- prometheus.MustNewConstMetric(c.confirmed, prometheus.GaugeValue, float64(r.Confirmed), id)
		ch <- prometheus.MustNewConstMetric(c.total, prometheus.GaugeValue, float64(r.Total), id)
	}
}

// PoolCollector exposes pgxpool statistics. Under a burst, acquire waits and
// a full pool are the first sign the database is the bottleneck.
type PoolCollector struct {
	store *store.Store

	acquired, idle, total, max   *prometheus.Desc
	acquires, emptyAcquires      *prometheus.Desc
	acquireWait, canceledAcquire *prometheus.Desc
	retries                      *prometheus.Desc
}

func NewPoolCollector(st *store.Store) *PoolCollector {
	return &PoolCollector{
		store:           st,
		acquired:        prometheus.NewDesc("db_pool_acquired_conns", "Connections in use.", nil, nil),
		idle:            prometheus.NewDesc("db_pool_idle_conns", "Idle connections.", nil, nil),
		total:           prometheus.NewDesc("db_pool_total_conns", "Open connections.", nil, nil),
		max:             prometheus.NewDesc("db_pool_max_conns", "Pool size limit.", nil, nil),
		acquires:        prometheus.NewDesc("db_pool_acquires_total", "Successful connection acquires.", nil, nil),
		emptyAcquires:   prometheus.NewDesc("db_pool_empty_acquires_total", "Acquires that had to wait because the pool was empty.", nil, nil),
		acquireWait:     prometheus.NewDesc("db_pool_acquire_wait_seconds_total", "Total time spent waiting for a connection.", nil, nil),
		canceledAcquire: prometheus.NewDesc("db_pool_canceled_acquires_total", "Acquires abandoned because the request was cancelled.", nil, nil),
		retries:         prometheus.NewDesc("db_tx_retries_total", "Reserve/cancel transactions retried after a deadlock or serialization failure. Should stay 0.", nil, nil),
	}
}

func (c *PoolCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.acquired, c.idle, c.total, c.max, c.acquires, c.emptyAcquires, c.acquireWait, c.canceledAcquire, c.retries} {
		ch <- d
	}
}

func (c *PoolCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.store.PoolStat()
	g, k := prometheus.GaugeValue, prometheus.CounterValue
	ch <- prometheus.MustNewConstMetric(c.acquired, g, float64(s.AcquiredConns()))
	ch <- prometheus.MustNewConstMetric(c.idle, g, float64(s.IdleConns()))
	ch <- prometheus.MustNewConstMetric(c.total, g, float64(s.TotalConns()))
	ch <- prometheus.MustNewConstMetric(c.max, g, float64(s.MaxConns()))
	ch <- prometheus.MustNewConstMetric(c.acquires, k, float64(s.AcquireCount()))
	ch <- prometheus.MustNewConstMetric(c.emptyAcquires, k, float64(s.EmptyAcquireCount()))
	ch <- prometheus.MustNewConstMetric(c.acquireWait, k, s.AcquireDuration().Seconds())
	ch <- prometheus.MustNewConstMetric(c.canceledAcquire, k, float64(s.CanceledAcquireCount()))
	ch <- prometheus.MustNewConstMetric(c.retries, k, float64(store.Retries()))
}
