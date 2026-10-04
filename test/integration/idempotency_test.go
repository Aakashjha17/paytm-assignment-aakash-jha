//go:build integration

package integration

import (
	"context"
	"testing"
)

func reservationsFor(t *testing.T, user, key string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM reservations WHERE user_id = $1 AND idempotency_key = $2`,
		user, key).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The same request fired 50 times in parallel with one key creates exactly one
// reservation: one 201, the rest 200 replays of that same reservation.
func TestIdempotentParallelRetries(t *testing.T) {
	show := createShow(t, showOpts{Seats: 20})
	user := "idem-" + runID(t)
	tok := token(t, user)

	var rs []resp
	noRetries(t, func() {
		rs = storm(50, func(int) resp { return reserve(tok, show, "same-key", "A1", "A2") })
	})
	got := tally(t, rs)
	if got["201"] != 1 || got["200"] != 49 {
		t.Fatalf("want 1×201 + 49×200, got %v", got)
	}
	id := ""
	for _, r := range rs {
		if id == "" {
			id = r.str("id")
		}
		if r.str("id") != id {
			t.Fatalf("responses name different reservations: %s vs %s", id, r.str("id"))
		}
		if r.Status == 200 && r.Header.Get("Idempotent-Replayed") != "true" {
			t.Error("replay missing Idempotent-Replayed header")
		}
	}
	if n := reservationsFor(t, user, "same-key"); n != 1 {
		t.Fatalf("%d reservation rows for the key, want 1", n)
	}
	if c := checkConsistency(t, show); c.Held != 2 {
		t.Fatalf("held=%d, want 2: retries must move nothing extra", c.Held)
	}
}

// After a success: reordered seats replay; different seats on the same key
// are a 409 and change nothing.
func TestIdempotencyKeyReuse(t *testing.T) {
	show := createShow(t, showOpts{Seats: 20})
	user := "reuse-" + runID(t)
	tok := token(t, user)

	first := reserve(tok, show, "k", "A1", "A2")
	if first.Status != 201 {
		t.Fatalf("first: %d %v", first.Status, first.Body)
	}
	if r := reserve(tok, show, "k", "a2", "A1"); r.Status != 200 || r.str("id") != first.str("id") {
		t.Errorf("reordered retry: want 200 same id, got %d %v", r.Status, r.Body)
	}
	for _, seats := range [][]string{{"A3"}, {"A1"}, {"A1", "A2", "A3"}} {
		if r := reserve(tok, show, "k", seats...); r.Status != 409 || r.errCode() != "idempotency_key_reused" {
			t.Errorf("same key, seats %v: want 409 idempotency_key_reused, got %d %v", seats, r.Status, r.Body)
		}
	}
	// Same key on a different show is a different request too.
	other := createShow(t, showOpts{Seats: 20})
	if r := reserve(tok, other, "k", "A1", "A2"); r.Status != 409 || r.errCode() != "idempotency_key_reused" {
		t.Errorf("same key, other show: want 409, got %d %v", r.Status, r.Body)
	}
	if n := reservationsFor(t, user, "k"); n != 1 {
		t.Fatalf("%d reservation rows for the key, want 1", n)
	}
	if c := checkConsistency(t, show); c.Held != 2 {
		t.Fatalf("held=%d, want 2", c.Held)
	}
}

// One key, two different bodies racing: exactly one wins. The losers either
// replay the winner (same body) or get 409 (different body). Never two holds.
func TestIdempotencyRacingDifferentBodies(t *testing.T) {
	show := createShow(t, showOpts{Seats: 20})
	user := "race-" + runID(t)
	tok := token(t, user)

	seats := func(i int) string {
		if i%2 == 0 {
			return "A1"
		}
		return "A2"
	}
	var rs []resp
	noRetries(t, func() {
		rs = storm(40, func(i int) resp { return reserve(tok, show, "contested", seats(i)) })
	})
	got := tally(t, rs)
	if got["201"] != 1 || got["200"]+got["409 idempotency_key_reused"] != 39 {
		t.Fatalf("want 1×201 and 39 replays/conflicts, got %v", got)
	}
	var winner string
	for i, r := range rs {
		if r.Status == 201 {
			winner = seats(i)
		}
	}
	for i, r := range rs {
		sameBody := seats(i) == winner
		if r.Status == 200 && !sameBody {
			t.Errorf("request %d (%s) replayed winner %s's reservation", i, seats(i), winner)
		}
		if r.Status == 409 && sameBody {
			t.Errorf("request %d had the winner's body but got 409", i)
		}
	}
	if c := checkConsistency(t, show); c.Held != 1 {
		t.Fatalf("held=%d, want 1", c.Held)
	}
}

// Keys are per user: two users can use the same key independently.
func TestIdempotencyKeyScopedToUser(t *testing.T) {
	show := createShow(t, showOpts{Seats: 20})
	id := runID(t)
	a, b := token(t, "scope-a-"+id), token(t, "scope-b-"+id)
	if r := reserve(a, show, "shared", "A1"); r.Status != 201 {
		t.Fatalf("a: %d %v", r.Status, r.Body)
	}
	if r := reserve(b, show, "shared", "A2"); r.Status != 201 {
		t.Fatalf("b, same key different user: want 201, got %d %v", r.Status, r.Body)
	}
	checkConsistency(t, show)
}

// A declined request stores nothing under its key, so retrying it later,
// after the seat frees up, can succeed.
func TestDeclinedKeyIsNotBurned(t *testing.T) {
	show := createShow(t, showOpts{Seats: 20})
	id := runID(t)
	holder, other := token(t, "holder-"+id), token(t, "other-"+id)

	held := reserve(holder, show, "h", "A1")
	if held.Status != 201 {
		t.Fatal(held.Body)
	}
	if r := reserve(other, show, "retry-me", "A1"); r.Status != 409 || r.errCode() != "seat_taken" {
		t.Fatalf("want seat_taken, got %d %v", r.Status, r.Body)
	}
	if r := cancel(holder, held.str("id")); r.Status != 200 {
		t.Fatal(r.Body)
	}
	if r := reserve(other, show, "retry-me", "A1"); r.Status != 201 {
		t.Fatalf("retry after release: want 201, got %d %v", r.Status, r.Body)
	}
	checkConsistency(t, show)
}
