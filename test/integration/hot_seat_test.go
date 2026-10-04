//go:build integration

package integration

import (
	"fmt"
	"testing"
)

// Many users storm one seat: exactly one 201, everyone else 409 seat_taken.
func TestHotSeatSingle(t *testing.T) {
	const users = 400
	show := createShow(t, showOpts{Seats: 50})
	toks := tokens(t, "hot1-"+runID(t), users)

	var rs []resp
	noRetries(t, func() {
		rs = storm(users, func(i int) resp { return reserve(toks[i], show, "k", "A1") })
	})

	got := tally(t, rs)
	if got["201"] != 1 || got["409 seat_taken"] != users-1 || len(got) != 2 {
		t.Fatalf("want exactly 1×201 and %d×409 seat_taken, got %v", users-1, got)
	}
	c := checkConsistency(t, show)
	if c.Held != 1 || c.Available != 49 {
		t.Fatalf("counts after storm: %+v", c)
	}
}

// Several hot seats stormed at once, each by its own crowd, plus background
// traffic on cold seats. Each hot seat gets exactly one winner.
func TestHotSeatMany(t *testing.T) {
	const (
		hot      = 5
		perSeat  = 150
		cold     = 100
		totalReq = hot*perSeat + cold
	)
	show := createShow(t, showOpts{Seats: 200, PerRow: 200, Limit: 1}) // one row: A1..A200
	toks := tokens(t, "hot5-"+runID(t), totalReq)

	seatFor := func(i int) string {
		if i < hot*perSeat {
			return fmt.Sprintf("A%d", i%hot+1) // A1..A5 are hot
		}
		return fmt.Sprintf("A%d", 100+i-hot*perSeat+1) // A101..A200: one cold seat per user
	}

	var rs []resp
	noRetries(t, func() {
		rs = storm(totalReq, func(i int) resp { return reserve(toks[i], show, "k", seatFor(i)) })
	})
	tally(t, rs)

	wins := map[string]int{}
	for i, r := range rs {
		switch {
		case r.Status == 201:
			wins[seatFor(i)]++
		case r.Status == 409 && r.errCode() == "seat_taken":
		default:
			t.Errorf("request %d for %s: unexpected %d %v", i, seatFor(i), r.Status, r.Body)
		}
	}
	for s := 1; s <= hot; s++ {
		if n := wins[fmt.Sprintf("A%d", s)]; n != 1 {
			t.Errorf("hot seat A%d: %d winners, want exactly 1", s, n)
		}
	}
	coldWins := len(wins) - hot
	if coldWins != cold {
		t.Errorf("cold seats: %d won, want all %d (no contention)", coldWins, cold)
	}
	if c := checkConsistency(t, show); c.Held != hot+cold {
		t.Errorf("held = %d, want %d", c.Held, hot+cold)
	}
}
