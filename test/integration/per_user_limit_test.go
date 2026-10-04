//go:build integration

package integration

import (
	"context"
	"fmt"
	"testing"
)

func liveSeatsOf(t *testing.T, show int64, user string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM seats
		WHERE show_id = $1 AND user_id = $2
		  AND (status = 'confirmed' OR (status = 'held' AND held_until > now()))`,
		show, user).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// One user fires 10 parallel single-seat reserves (distinct seats, distinct
// keys) at a limit-4 show: exactly 4 succeed, 6 are per_user_limit.
func TestPerUserLimitParallel(t *testing.T) {
	show := createShow(t, showOpts{Seats: 50, Limit: 4})
	user := "lim-" + runID(t)
	tok := token(t, user)

	var rs []resp
	noRetries(t, func() {
		rs = storm(10, func(i int) resp {
			return reserve(tok, show, fmt.Sprintf("k%d", i), fmt.Sprintf("A%d", i+1))
		})
	})
	got := tally(t, rs)
	if got["201"] != 4 || got["409 per_user_limit"] != 6 {
		t.Fatalf("want 4×201 + 6×409 per_user_limit, got %v", got)
	}
	if n := liveSeatsOf(t, show, user); n != 4 {
		t.Fatalf("user holds %d seats, want 4", n)
	}
	checkConsistency(t, show)
}

// Multi-seat requests: 3 seats each against a limit of 4, so only one fits.
func TestPerUserLimitMultiSeat(t *testing.T) {
	show := createShow(t, showOpts{Seats: 100, PerRow: 100, Limit: 4})
	user := "limm-" + runID(t)
	tok := token(t, user)

	var rs []resp
	noRetries(t, func() {
		rs = storm(10, func(i int) resp {
			b := i*3 + 1
			return reserve(tok, show, fmt.Sprintf("k%d", i),
				fmt.Sprintf("A%d", b), fmt.Sprintf("A%d", b+1), fmt.Sprintf("A%d", b+2))
		})
	})
	got := tally(t, rs)
	if got["201"] != 1 || got["409 per_user_limit"] != 9 {
		t.Fatalf("want 1×201 + 9×409 per_user_limit, got %v", got)
	}
	if n := liveSeatsOf(t, show, user); n != 3 {
		t.Fatalf("user holds %d seats, want 3", n)
	}
	checkConsistency(t, show)
}

// Many users each over-firing at once, all on the same show and contending
// for overlapping seats: nobody ends above the limit, and everything adds up.
func TestPerUserLimitManyUsers(t *testing.T) {
	const (
		users   = 30
		perUser = 10
		limit   = 4
	)
	show := createShow(t, showOpts{Seats: 200, PerRow: 200, Limit: limit})
	prefix := "limmany-" + runID(t)
	toks := tokens(t, prefix, users)

	var rs []resp
	noRetries(t, func() {
		rs = storm(users*perUser, func(i int) resp {
			u, j := i/perUser, i%perUser
			// Seats overlap between neighbouring users, so limit and seat
			// contention interact.
			return reserve(toks[u], show, fmt.Sprintf("k%d", j), fmt.Sprintf("A%d", (u*5+j)%200+1))
		})
	})
	tally(t, rs)

	for u := 0; u < users; u++ {
		if n := liveSeatsOf(t, show, fmt.Sprintf("%s-%d", prefix, u)); n > limit {
			t.Errorf("user %d holds %d seats, limit %d", u, n, limit)
		}
	}
	wins := 0
	for _, r := range rs {
		if r.Status == 201 {
			wins++
		}
	}
	if c := checkConsistency(t, show); c.Held != wins {
		t.Errorf("held=%d but %d reservations succeeded (all single-seat)", c.Held, wins)
	}
}
