//go:build integration

package integration

import (
	"fmt"
	"math/rand/v2"
	"testing"
	"time"
)

// A ring of overlapping two-seat requests, each listing its seats in the
// opposite order to its neighbour: user i wants {S(i+1), S(i)}. Without a
// global lock order this is the textbook deadlock (each holds one seat the
// next one wants). With seats locked in seat_no order it must finish with no
// deadlock, no retries, no 5xx, and every success holding both its seats.
func TestMultiSeatRingNoDeadlock(t *testing.T) {
	const (
		seats  = 20
		rounds = 15
		users  = seats * rounds
	)
	show := createShow(t, showOpts{Seats: seats, PerRow: seats, Limit: 2})
	toks := tokens(t, "ring-"+runID(t), users)

	label := func(i int) string { return fmt.Sprintf("A%d", i%seats+1) }
	var rs []resp
	start := time.Now()
	noRetries(t, func() {
		rs = storm(users, func(i int) resp {
			return reserve(toks[i], show, "k", label(i+1), label(i)) // reversed on purpose
		})
	})
	if d := time.Since(start); d > 20*time.Second {
		t.Errorf("ring took %v; something is waiting on lock timeouts", d)
	}

	got := tally(t, rs)
	if got["201"]+got["409 seat_taken"] != users {
		t.Fatalf("only 201/seat_taken expected, got %v", got)
	}
	c := checkConsistency(t, show)
	if c.Held != 2*got["201"] {
		t.Fatalf("held=%d but %d two-seat reservations won: not all-or-nothing", c.Held, got["201"])
	}
	if got["201"] == 0 {
		t.Fatal("nobody won anything")
	}
}

// Larger random multi-seat requests in shuffled order, mixed with cancels of
// whatever was won, all racing on a small show.
func TestMultiSeatRandomWithCancels(t *testing.T) {
	const (
		seats = 30
		users = 300
	)
	show := createShow(t, showOpts{Seats: seats, PerRow: seats, Limit: 6})
	toks := tokens(t, "rand-"+runID(t), users)

	picks := make([][]string, users)
	for i := range picks {
		perm := rand.Perm(seats)[:2+rand.IntN(4)] // 2..5 seats, shuffled
		for _, p := range perm {
			picks[i] = append(picks[i], fmt.Sprintf("A%d", p+1))
		}
	}

	var rs []resp
	noRetries(t, func() {
		rs = storm(users, func(i int) resp {
			r := reserve(toks[i], show, "k", picks[i]...)
			// Half the winners cancel straight away, freeing seats while
			// other reserves are still storming them.
			if r.Status == 201 && i%2 == 0 {
				if c := cancel(toks[i], r.str("id")); c.Status != 200 {
					return c
				}
			}
			return r
		})
	})
	got := tally(t, rs)
	for k, n := range got {
		if k != "201" && k != "409 seat_taken" {
			t.Errorf("unexpected %q ×%d", k, n)
		}
	}

	held := 0
	for i, r := range rs {
		if r.Status == 201 && i%2 != 0 {
			held += len(picks[i])
		}
	}
	if c := checkConsistency(t, show); c.Held != held {
		t.Fatalf("held=%d, want %d from surviving reservations", c.Held, held)
	}
}
