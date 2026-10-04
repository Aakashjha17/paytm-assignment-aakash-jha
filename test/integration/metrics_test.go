//go:build integration

package integration

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"seat-reservation/internal/audit"
)

// After a mixed storm, the metrics agree with the API and with what the
// clients saw:
//   - seat gauges == GET /shows counts, and sum to seats_total
//   - every reserve response incremented exactly one outcome counter, with
//     the reason the client actually received.
func TestMetricsReconcileWithAPI(t *testing.T) {
	show := createShow(t, showOpts{Seats: 50, PerRow: 50, Limit: 2}) // one row: A1..A50
	prefix := "met-" + runID(t)
	toks := tokens(t, prefix, 300)
	// User 299 holds A50 up front, so the storm's replays of that request are
	// guaranteed to be replays (not seat_taken from losing a hot seat).
	if r := reserve(toks[299], show, "replay-me", "A50"); r.Status != 201 {
		t.Fatal(r.Body)
	}
	before := scrape(t)

	// Mixed traffic: hot seats, replays, per-user-limit, a missing key and a
	// missing token. Index i decides which.
	var rs []resp
	noRetries(t, func() {
		rs = storm(300, func(i int) resp {
			switch {
			case i < 200: // 200 users storm 4 hot seats
				return reserve(toks[i], show, "k", fmt.Sprintf("A%d", i%4+1))
			case i < 240: // 40 replays of user 299's earlier request
				return reserve(toks[299], show, "replay-me", "A50")
			case i < 270: // user 250 over-fires distinct seats at a limit of 2
				return reserve(toks[250], show, fmt.Sprintf("lim%d", i), fmt.Sprintf("A%d", i-240+11)) // A11..A40
			case i < 285: // no Idempotency-Key
				return do("POST", fmt.Sprintf("/shows/%d/reservations", show), toks[i], nil, map[string]any{"seats": []string{"A45"}})
			default: // no token
				return reserve("", show, "k", "A45")
			}
		})
	})
	tally(t, rs)

	// What the clients saw, translated to metric reasons.
	want := map[string]int{}
	for _, r := range rs {
		switch {
		case r.Status == 201:
			want["confirmed"]++
		case r.Status == 200:
			want["idempotent-replay"]++
		default:
			want[strings.ReplaceAll(r.errCode(), "_", "-")]++
		}
	}

	// Cancel two of the winners.
	cancelled := 0
	for i, r := range rs {
		if i < 200 && r.Status == 201 && cancelled < 2 {
			if c := cancel(toks[i], r.str("id")); c.Status != 200 {
				t.Fatalf("cancel: %v", c.Body)
			}
			cancelled++
		}
	}

	after := scrape(t)
	delta := func(series string) int { return int(after[series] - before[series]) }

	// Counters: exactly one per response, with the right reason.
	total := delta("reservations_confirmed_total")
	if got := total; got != want["confirmed"] {
		t.Errorf("confirmed: metric +%d, clients saw %d", got, want["confirmed"])
	}
	for reason, n := range want {
		if reason == "confirmed" {
			continue
		}
		got := delta(fmt.Sprintf(`reservations_declined_total{reason="%s"}`, reason))
		if got != n {
			t.Errorf("declined{%s}: metric +%d, clients saw %d", reason, got, n)
		}
		total += got
	}
	if total != len(rs) {
		t.Errorf("outcome counters moved by %d for %d responses; want exactly one each", total, len(rs))
	}
	if got := delta(`reservation_cancellations_total{outcome="cancelled"}`); got != cancelled {
		t.Errorf("cancellations: metric +%d, did %d", got, cancelled)
	}
	for _, reason := range []string{"seat-taken", "per-user-limit", "idempotent-replay"} {
		if want[reason] == 0 {
			t.Errorf("storm produced no %s; test isn't exercising it", reason)
		}
	}

	// Gauges: equal to the API, and reconciled.
	c, totalSeats := showCounts(t, show)
	id := fmt.Sprint(show)
	g := func(name string) int { return int(after[fmt.Sprintf(`%s{show_id="%s"}`, name, id)]) }
	if g("seats_available") != c.Available || g("seats_held") != c.Held || g("seats_confirmed") != c.Confirmed {
		t.Errorf("gauges available=%d held=%d confirmed=%d, API %+v",
			g("seats_available"), g("seats_held"), g("seats_confirmed"), c)
	}
	if g("seats_available")+g("seats_held")+g("seats_confirmed") != g("seats_total") || g("seats_total") != totalSeats {
		t.Errorf("gauges don't reconcile to seats_total=%d (show has %d)", g("seats_total"), totalSeats)
	}
	if after["seats_scrape_success"] != 1 {
		t.Error("seats_scrape_success != 1")
	}
	checkConsistency(t, show)
}

// The audit gauge reads 0 on a healthy database and goes nonzero, naming the
// right check, when a row is deliberately corrupted; then back to 0 on repair.
func TestAuditDetectsCorruption(t *testing.T) {
	ctx := context.Background()
	runner := audit.NewRunner(st, mtx, slog.New(slog.NewJSONHandler(io.Discard, nil)), time.Hour)
	gauge := func(check string) float64 {
		return scrape(t)[fmt.Sprintf(`audit_mismatches{check="%s"}`, check)]
	}
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}

	if res := runner.RunOnce(ctx); res.Total() != 0 {
		t.Fatalf("audit not clean before corruption: %v %v", res.Mismatches, res.Samples)
	}

	show := createShow(t, showOpts{Seats: 20})
	owner := "aud-" + runID(t)
	r := reserve(token(t, owner), show, "k", "A1")
	if r.Status != 201 {
		t.Fatal(r.Body)
	}

	// Corruption 1: a held seat's owner silently changed (a double-sell shape).
	mustExec(`UPDATE seats SET user_id = 'mallory' WHERE show_id = $1 AND label = 'A1'`, show)
	runner.RunOnce(ctx)
	if gauge("orphan_occupied_seat") < 1 || gauge("reservation_missing_seats") < 1 {
		t.Fatalf("seat owner corruption not detected: orphan=%v missing=%v",
			gauge("orphan_occupied_seat"), gauge("reservation_missing_seats"))
	}
	mustExec(`UPDATE seats SET user_id = $2 WHERE show_id = $1 AND label = 'A1'`, show, owner)

	// Corruption 2: a seat row vanishes, so the counts can't reach total_seats.
	mustExec(`DELETE FROM seats WHERE show_id = $1 AND label = 'A20'`, show)
	runner.RunOnce(ctx)
	if gauge("seat_count_mismatch") != 1 {
		t.Fatalf("missing seat row not detected: %v", gauge("seat_count_mismatch"))
	}
	mustExec(`INSERT INTO seats (show_id, seat_no, label) VALUES ($1, 20, 'A20')`, show)

	if res := runner.RunOnce(ctx); res.Total() != 0 {
		t.Fatalf("audit not clean after repair: %v", res.Samples)
	}
	for _, check := range []string{"orphan_occupied_seat", "reservation_missing_seats", "seat_count_mismatch"} {
		if v := gauge(check); v != 0 {
			t.Errorf("%s gauge = %v after repair", check, v)
		}
	}
}
