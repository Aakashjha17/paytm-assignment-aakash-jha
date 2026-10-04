package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// job is one reserve request (and optionally a cancel of whatever it wins).
type job struct {
	kind     string // hot, hot-retry, cold, greedy, conflict, spoof
	user     int    // index into users/tokens
	key      string
	seats    []string
	spoofAs  string // body user_id claiming to be someone else
	cancel   bool   // cancel the reservation if this request wins it
	hotSeat  string
	reserve  response
	canceled response
}

type env struct {
	cfg    config
	c      *client
	run    string
	show   int64
	users  []string
	tokens []string
}

func run(ctx context.Context, cfg config) (bool, error) {
	e := &env{cfg: cfg, c: newClient(cfg.base, cfg.concurrency, cfg.timeout), run: runID()}

	logf(cfg, "waiting for %s/readyz ...", cfg.base)
	if err := e.c.waitReady(ctx, 90*time.Second); err != nil {
		return false, err
	}
	if err := e.createShow(ctx); err != nil {
		return false, err
	}

	jobs, nUsers := buildWorkload(cfg)
	logf(cfg, "minting %d user tokens ...", nUsers)
	if err := e.mintTokens(ctx, nUsers); err != nil {
		return false, err
	}

	before, err := e.c.metrics(ctx)
	if err != nil {
		logf(cfg, "warning: /metrics not reachable before burst: %v", err)
	}

	logf(cfg, "firing %d reserve requests at concurrency %d ...", len(jobs), cfg.concurrency)
	pollCtx, stopPoll := context.WithCancel(ctx)
	var p poller
	var pollDone sync.WaitGroup
	pollDone.Add(1)
	go func() { defer pollDone.Done(); e.pollInvariants(pollCtx, &p) }()

	start := time.Now()
	e.execute(ctx, jobs)
	elapsed := time.Since(start)
	burstEnd := time.Now()
	stopPoll()
	pollDone.Wait()

	after, err := e.c.metrics(ctx)
	if err != nil {
		logf(cfg, "warning: /metrics not reachable after burst: %v", err)
	}

	printOutcomes(e, jobs, elapsed)

	var rep report
	e.checkClientSide(&rep, jobs)
	rep.add("during burst: invariant held on every poll", len(p.violations) == 0 && p.showPolls > 0,
		"%d /shows polls, %d /metrics polls, %d poll errors %s", p.showPolls, p.metricPoll, p.errors, orNone(p.violations))
	e.checkReconciliation(ctx, &rep, jobs, before, after)
	logf(cfg, "waiting for a post-burst audit run (up to %v) ...", cfg.auditWait)
	e.checkAudit(ctx, &rep, burstEnd)
	rep.print()
	return rep.passed(), nil
}

func (e *env) createShow(ctx context.Context) error {
	r := e.c.do(ctx, "POST", "/shows", map[string]string{"X-Admin-Key": e.cfg.adminKey}, map[string]any{
		"name":             "burst " + e.run,
		"total_seats":      e.cfg.seats,
		"seats_per_row":    50,
		"price_paise":      49900,
		"per_user_limit":   e.cfg.limit,
		"hold_ttl_seconds": 3600, // nothing expires mid-run, so held counts are exact
	})
	if r.Err != nil || r.Status != 201 {
		return fmt.Errorf("create show: %d %v %s", r.Status, r.Err, r.Raw)
	}
	e.show = int64(r.Body["id"].(float64))
	return nil
}

func (e *env) mintTokens(ctx context.Context, n int) error {
	e.users = make([]string, n)
	e.tokens = make([]string, n)
	var firstErr atomic.Value
	parallel(n, e.cfg.concurrency, func(i int) {
		e.users[i] = fmt.Sprintf("burst-%s-%d", e.run, i)
		// Setup, not measurement: minting is side-effect free, so a request
		// lost in transit (seen once at a live edge) is simply retried. The
		// measured reserve/cancel requests are never retried.
		var r response
		for attempt := 0; attempt < 3; attempt++ {
			r = e.c.do(ctx, "POST", "/tokens", nil, map[string]any{"user_id": e.users[i]})
			if r.Err == nil && r.Status < 500 {
				break
			}
		}
		if r.Err != nil || r.Status != 201 {
			firstErr.CompareAndSwap(nil, fmt.Errorf("mint token: %d %v %s", r.Status, r.Err, r.Raw))
			return
		}
		e.tokens[i] = r.str("token")
	})
	if err, _ := firstErr.Load().(error); err != nil {
		return err
	}
	return nil
}

func (e *env) execute(ctx context.Context, jobs []*job) {
	var done, fivexx atomic.Int64
	stop := make(chan struct{})
	if !e.cfg.quiet {
		go func() {
			t := time.NewTicker(time.Second)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					fmt.Fprintf(os.Stderr, "  %6d/%d done, 5xx so far: %d\n", done.Load(), len(jobs), fivexx.Load())
				}
			}
		}()
	}
	parallel(len(jobs), e.cfg.concurrency, func(i int) {
		j := jobs[i]
		var extra map[string]any
		if j.spoofAs != "" {
			var victim int
			fmt.Sscan(j.spoofAs, &victim)
			extra = map[string]any{"user_id": e.users[victim], "userId": e.users[victim]}
		}
		j.reserve = e.c.reserve(ctx, e.tokens[j.user], e.show, j.key, j.seats, extra)
		if j.cancel && j.reserve.Status == 201 {
			j.canceled = e.c.cancel(ctx, e.tokens[j.user], j.reserve.str("id"))
		}
		if j.reserve.Status >= 500 || j.canceled.Status >= 500 {
			fivexx.Add(1)
		}
		done.Add(1)
	})
	close(stop)
}

// parallel runs fn(0..n-1) with at most width in flight.
func parallel(n, width int, fn func(i int)) {
	next := atomic.Int64{}
	var wg sync.WaitGroup
	for w := 0; w < min(width, n); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= n {
					return
				}
				fn(i)
			}
		}()
	}
	wg.Wait()
}

// reason maps a response to the metric reason the server should have counted.
func reason(r response) string {
	switch {
	case r.Err != nil:
		return "transport-error"
	case r.Status == 201:
		return "confirmed"
	case r.Status == 200:
		return "idempotent-replay"
	case r.errCode() != "":
		return strings.ReplaceAll(r.errCode(), "_", "-")
	default:
		return fmt.Sprintf("http-%d", r.Status)
	}
}

func printOutcomes(e *env, jobs []*job, elapsed time.Duration) {
	fmt.Printf("\n== burst against %s — show %d (%d seats, limit %d), run %s, seed %d\n",
		e.cfg.base, e.show, e.cfg.seats, e.cfg.limit, e.run, e.cfg.seed)

	cancels := 0
	var durs []time.Duration
	byReason := map[string]int{}
	byStatus := map[string]int{}
	for _, j := range jobs {
		byReason[reason(j.reserve)]++
		byStatus[statusClass(j.reserve)]++
		durs = append(durs, j.reserve.Dur)
		if j.canceled.Status != 0 || j.canceled.Err != nil {
			cancels++
		}
	}
	fmt.Printf("%d reserve + %d cancel requests in %s (%.0f req/s), concurrency %d\n",
		len(jobs), cancels, elapsed.Round(time.Millisecond), float64(len(jobs)+cancels)/elapsed.Seconds(), e.cfg.concurrency)
	sort.Slice(durs, func(a, b int) bool { return durs[a] < durs[b] })
	pct := func(p float64) time.Duration {
		return durs[min(len(durs)-1, int(p*float64(len(durs))))].Round(time.Millisecond)
	}
	fmt.Printf("latency: p50 %v  p95 %v  p99 %v  max %v\n", pct(.50), pct(.95), pct(.99), durs[len(durs)-1].Round(time.Millisecond))

	fmt.Println("\noutcomes (reserve), by what the client received:")
	for _, k := range sortedKeys(byReason) {
		fmt.Printf("  %-28s %7d\n", k, byReason[k])
	}
	fmt.Printf("  %-28s %7d\n", "5xx", byStatus["5xx"])

	byKind := map[string]map[string]int{}
	for _, j := range jobs {
		if byKind[j.kind] == nil {
			byKind[j.kind] = map[string]int{}
		}
		byKind[j.kind][reason(j.reserve)]++
	}
	fmt.Println("\nby workload kind:")
	for _, k := range sortedKeys(byKind) {
		var parts []string
		for _, r := range sortedKeys(byKind[k]) {
			parts = append(parts, fmt.Sprintf("%s=%d", r, byKind[k][r]))
		}
		fmt.Printf("  %-10s %s\n", k, strings.Join(parts, "  "))
	}

	// Hot seats: winners per seat.
	hot := map[string][2]int{} // seat -> [201s, requests]
	for _, j := range jobs {
		if j.hotSeat == "" {
			continue
		}
		v := hot[j.hotSeat]
		v[1]++
		if j.reserve.Status == 201 {
			v[0]++
		}
		hot[j.hotSeat] = v
	}
	if len(hot) > 0 {
		fmt.Println("\nhot seats (201s / requests):")
		for _, s := range sortedKeys(hot) {
			fmt.Printf("  %-5s %d / %d\n", s, hot[s][0], hot[s][1])
		}
	}
}

func statusClass(r response) string {
	switch {
	case r.Err != nil:
		return "error"
	case r.Status >= 500:
		return "5xx"
	case r.Status >= 400:
		return "4xx"
	default:
		return "2xx"
	}
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func logf(cfg config, f string, a ...any) {
	if !cfg.quiet {
		fmt.Fprintf(os.Stderr, f+"\n", a...)
	}
}
