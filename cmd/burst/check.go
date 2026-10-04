package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---- mid-burst invariant poller ------------------------------------------

type poller struct {
	mu         sync.Mutex
	showPolls  int
	metricPoll int
	violations []string
	errors     int
}

func (p *poller) fail(format string, a ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.violations) < 10 {
		p.violations = append(p.violations, fmt.Sprintf(format, a...))
	} else if len(p.violations) == 10 {
		p.violations = append(p.violations, "...")
	}
}

// pollInvariants checks, while the burst is running, that GET /shows always
// reconciles (available + held + confirmed == total, every seat listed) and
// that /metrics agrees: seat gauges reconcile and audit_mismatches stays 0.
func (e *env) pollInvariants(ctx context.Context, p *poller) {
	tick := time.NewTicker(e.cfg.pollEvery)
	defer tick.Stop()
	lastMetrics := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		c, err := e.c.showCounts(ctx, e.show)
		if ctx.Err() != nil {
			return
		}
		p.mu.Lock()
		p.showPolls++
		if err != nil {
			p.errors++
		}
		p.mu.Unlock()
		if err == nil {
			if c.Available+c.Held+c.Confirmed != c.Total || c.Listed != c.Total {
				p.fail("GET /shows: %d + %d + %d != %d (listed %d)", c.Available, c.Held, c.Confirmed, c.Total, c.Listed)
			}
		}

		if time.Since(lastMetrics) < time.Second {
			continue
		}
		lastMetrics = time.Now()
		m, err := e.c.metrics(ctx)
		if ctx.Err() != nil {
			return
		}
		p.mu.Lock()
		p.metricPoll++
		if err != nil {
			p.errors++
		}
		p.mu.Unlock()
		if err != nil {
			continue
		}
		g := e.gauges(m)
		if g.Available+g.Held+g.Confirmed != g.Total {
			p.fail("/metrics: seats %d + %d + %d != %d", g.Available, g.Held, g.Confirmed, g.Total)
		}
		if n := auditMismatches(m); n != 0 {
			p.fail("/metrics: audit_mismatches = %v", n)
		}
	}
}

func (e *env) gauges(m map[string]float64) counts {
	g := func(name string) int { return int(m[fmt.Sprintf(`%s{show_id="%d"}`, name, e.show)]) }
	return counts{Available: g("seats_available"), Held: g("seats_held"), Confirmed: g("seats_confirmed"), Total: g("seats_total")}
}

func auditMismatches(m map[string]float64) float64 {
	var n float64
	for k, v := range m {
		if strings.HasPrefix(k, "audit_mismatches{") {
			n += v
		}
	}
	return n
}

// ---- final checks ----------------------------------------------------------

type check struct {
	name   string
	ok     bool
	detail string
}

type report struct{ checks []check }

func (r *report) add(name string, ok bool, format string, a ...any) {
	r.checks = append(r.checks, check{name, ok, fmt.Sprintf(format, a...)})
}

func (r *report) passed() bool {
	for _, c := range r.checks {
		if !c.ok {
			return false
		}
	}
	return true
}

func (r *report) print() {
	fmt.Println("\nchecks:")
	for _, c := range r.checks {
		mark := "PASS"
		if !c.ok {
			mark = "FAIL"
		}
		fmt.Printf("  %s  %-44s %s\n", mark, c.name, c.detail)
	}
	if r.passed() {
		fmt.Println("\nRESULT: PASS")
	} else {
		fmt.Println("\nRESULT: FAIL")
	}
}

// checkClientSide verifies the correctness bars from what the clients saw.
func (e *env) checkClientSide(r *report, jobs []*job) {
	// 1. Zero 5xx, zero transport errors (a timeout hides the outcome).
	fivexx, transport := 0, 0
	for _, j := range jobs {
		for _, res := range []response{j.reserve, j.canceled} {
			if res.Err != nil {
				transport++
			} else if res.Status >= 500 {
				fivexx++
			}
		}
	}
	r.add("zero 5xx", fivexx == 0, "%d", fivexx)
	r.add("zero transport errors / timeouts", transport == 0, "%d", transport)

	// 2. Every hot seat: exactly one 201, everyone else 409 seat_taken (or a
	// 200 replay for the winner's own retry).
	type hs struct{ wins, taken, replay, other int }
	hot := map[string]*hs{}
	for _, j := range jobs {
		if j.hotSeat == "" {
			continue
		}
		h := hot[j.hotSeat]
		if h == nil {
			h = &hs{}
			hot[j.hotSeat] = h
		}
		switch {
		case j.reserve.Status == 201:
			h.wins++
		case j.reserve.Status == 409 && j.reserve.errCode() == "seat_taken":
			h.taken++
		case j.reserve.Status == 200:
			h.replay++
		default:
			h.other++
		}
	}
	var bad []string
	for s, h := range hot {
		if h.wins != 1 || h.other != 0 {
			bad = append(bad, fmt.Sprintf("%s: %d winners, %d unexpected", s, h.wins, h.other))
		}
	}
	sort.Strings(bad)
	r.add("each hot seat: exactly one 201, rest 409", len(bad) == 0, "%d seats %s", len(hot), strings.Join(bad, "; "))

	// 3. Idempotency: per (user, key) at most one 201, and every 2xx names
	// the same reservation.
	type key struct {
		user int
		k    string
	}
	ids := map[key]map[string]bool{}
	wins := map[key]int{}
	for _, j := range jobs {
		if j.reserve.Status/100 != 2 {
			continue
		}
		k := key{j.user, j.key}
		if ids[k] == nil {
			ids[k] = map[string]bool{}
		}
		ids[k][j.reserve.str("id")] = true
		if j.reserve.Status == 201 {
			wins[k]++
		}
	}
	idemBad := 0
	for k := range ids {
		if len(ids[k]) != 1 || wins[k] > 1 {
			idemBad++
		}
	}
	r.add("same key → one reservation", idemBad == 0, "%d keys with 2xx, %d violating", len(ids), idemBad)

	// 4. Per-user limit: seats a user still holds never exceed the limit.
	held := map[int]int{}
	for _, j := range jobs {
		if j.reserve.Status == 201 {
			held[j.user] += len(j.seats)
			if j.canceled.Status == 200 {
				held[j.user] -= len(j.seats)
			}
		}
	}
	over := 0
	for _, n := range held {
		if n > e.cfg.limit {
			over++
		}
	}
	r.add("per-user limit holds", over == 0, "%d users over %d", over, e.cfg.limit)

	// 5. Identity is the token's: every reservation returned belongs to the
	// caller, including the ones whose body claimed to be someone else.
	spoofed := 0
	for _, j := range jobs {
		if j.reserve.Status/100 == 2 && j.reserve.str("user_id") != e.users[j.user] {
			spoofed++
		}
	}
	r.add("identity comes from the token", spoofed == 0, "%d responses for the wrong user", spoofed)
}

// expectedHeld is what the clients collectively hold now: seats in 201s minus
// seats successfully cancelled. Holds last an hour, so none expired.
func expectedHeld(jobs []*job) int {
	n := 0
	for _, j := range jobs {
		if j.reserve.Status == 201 {
			n += len(j.seats)
			if j.canceled.Status == 200 {
				n -= len(j.seats)
			}
		}
	}
	return n
}

// checkReconciliation compares the final API state, the metrics, and the
// client-side tally.
func (e *env) checkReconciliation(ctx context.Context, r *report, jobs []*job, before, after map[string]float64) {
	c, err := e.c.showCounts(ctx, e.show)
	if err != nil {
		r.add("final GET /shows", false, "%v", err)
		return
	}
	r.add("final: available + held + confirmed == total", c.Available+c.Held+c.Confirmed == c.Total && c.Listed == c.Total,
		"%d + %d + %d = %d of %d", c.Available, c.Held, c.Confirmed, c.Available+c.Held+c.Confirmed, c.Total)
	want := expectedHeld(jobs)
	r.add("final: held == client wins − cancels", c.Held == want, "API held %d, clients hold %d", c.Held, want)

	if before == nil || after == nil {
		r.add("metrics reachable", false, "could not scrape /metrics")
		return
	}
	g := e.gauges(after)
	r.add("metrics: seat gauges == API", g.Available == c.Available && g.Held == c.Held && g.Confirmed == c.Confirmed && g.Total == c.Total,
		"gauges %d/%d/%d of %d, API %d/%d/%d", g.Available, g.Held, g.Confirmed, g.Total, c.Available, c.Held, c.Confirmed)

	// Outcome counters moved by exactly what the clients received. Assumes
	// nobody else is hitting this instance during the burst, and one replica.
	clientReasons := map[string]int{}
	cancelOK := 0
	for _, j := range jobs {
		clientReasons[reason(j.reserve)]++
		if j.canceled.Status == 200 {
			cancelOK++
		}
	}
	delta := func(series string) int { return int(after[series] - before[series]) }
	var diffs []string
	for _, rs := range sortedKeys(clientReasons) {
		var got int
		if rs == "confirmed" {
			got = delta("reservations_confirmed_total")
		} else {
			got = delta(fmt.Sprintf(`reservations_declined_total{reason="%s"}`, rs))
		}
		if got != clientReasons[rs] {
			diffs = append(diffs, fmt.Sprintf("%s metric +%d vs clients %d", rs, got, clientReasons[rs]))
		}
	}
	if got := delta(`reservation_cancellations_total{outcome="cancelled"}`); got != cancelOK {
		diffs = append(diffs, fmt.Sprintf("cancelled metric +%d vs clients %d", got, cancelOK))
	}
	r.add("metrics: outcome counters == client tally", len(diffs) == 0, "%s", orNone(diffs))
	r.add("metrics: no deadlock/serialization retries", delta("db_tx_retries_total") == 0, "+%d", delta("db_tx_retries_total"))
}

// checkAudit waits for the auditor to run once after the burst ended, then
// requires every audit_mismatches gauge to be 0.
func (e *env) checkAudit(ctx context.Context, r *report, burstEnd time.Time) {
	deadline := time.Now().Add(e.cfg.auditWait)
	for {
		m, err := e.c.metrics(ctx)
		if err == nil && m["audit_last_success_timestamp_seconds"] >= float64(burstEnd.Unix()+1) {
			n := auditMismatches(m)
			r.add("audit after burst: zero mismatches", n == 0, "audit_mismatches total %v", n)
			return
		}
		if time.Now().After(deadline) {
			r.add("audit after burst: zero mismatches", false, "no audit completed within %v of the burst", e.cfg.auditWait)
			return
		}
		time.Sleep(2 * time.Second)
	}
}

func orNone(xs []string) string {
	if len(xs) == 0 {
		return "all match"
	}
	return strings.Join(xs, "; ")
}
