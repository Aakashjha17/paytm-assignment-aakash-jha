//go:build integration

package integration

import (
	"context"
	"fmt"
	"testing"
)

// Reserve, cancel, and the released seat can be re-reserved by someone else.
// Cancelling twice is a harmless 200.
func TestCancelReleasesSeats(t *testing.T) {
	show := createShow(t, showOpts{Seats: 20})
	id := runID(t)
	alice, bob := token(t, "c-alice-"+id), token(t, "c-bob-"+id)

	r := reserve(alice, show, "k", "A1", "A2")
	if r.Status != 201 {
		t.Fatal(r.Body)
	}
	resID := r.str("id")

	c := cancel(alice, resID)
	if c.Status != 200 || c.str("status") != "cancelled" {
		t.Fatalf("cancel: %d %v", c.Status, c.Body)
	}
	if got := checkConsistency(t, show); got.Held != 0 || got.Available != 20 {
		t.Fatalf("after cancel: %+v", got)
	}
	if again := cancel(alice, resID); again.Status != 200 || again.str("status") != "cancelled" {
		t.Errorf("second cancel: want 200 cancelled, got %d %v", again.Status, again.Body)
	}
	// A replay of the original reserve now reports the cancelled reservation;
	// it does not quietly re-hold the seats.
	if rp := reserve(alice, show, "k", "A1", "A2"); rp.Status != 200 || rp.str("status") != "cancelled" {
		t.Errorf("replay after cancel: want 200 cancelled, got %d %v", rp.Status, rp.Body)
	}
	if b := reserve(bob, show, "k", "A2"); b.Status != 201 {
		t.Fatalf("bob re-reserving released seat: %d %v", b.Status, b.Body)
	}
	checkConsistency(t, show)
}

// Cancelling frees per-user quota.
func TestCancelFreesQuota(t *testing.T) {
	show := createShow(t, showOpts{Seats: 20, Limit: 2})
	tok := token(t, "quota-"+runID(t))

	first := reserve(tok, show, "k1", "A1", "A2")
	if first.Status != 201 {
		t.Fatal(first.Body)
	}
	if r := reserve(tok, show, "k2", "A3"); r.errCode() != "per_user_limit" {
		t.Fatalf("want per_user_limit, got %d %v", r.Status, r.Body)
	}
	if c := cancel(tok, first.str("id")); c.Status != 200 {
		t.Fatal(c.Body)
	}
	if r := reserve(tok, show, "k3", "A3", "A4"); r.Status != 201 {
		t.Fatalf("after cancel: want 201, got %d %v", r.Status, r.Body)
	}
	checkConsistency(t, show)
}

// The owner cancels a hot seat while a crowd storms it: at most one of the
// crowd gets it (only if it was released before they looked), never two.
func TestCancelDuringStorm(t *testing.T) {
	const crowd = 200
	show := createShow(t, showOpts{Seats: 20})
	id := runID(t)
	owner := token(t, "owner-"+id)
	toks := tokens(t, "crowd-"+id, crowd)

	held := reserve(owner, show, "k", "A1")
	if held.Status != 201 {
		t.Fatal(held.Body)
	}
	var rs []resp
	noRetries(t, func() {
		rs = storm(crowd+1, func(i int) resp {
			if i == crowd/2 {
				return cancel(owner, held.str("id"))
			}
			return reserve(toks[i%crowd], show, fmt.Sprintf("k%d", i), "A1")
		})
	})
	got := tally(t, rs)
	if rs[crowd/2].Status != 200 {
		t.Fatalf("cancel failed: %v", rs[crowd/2].Body)
	}
	if got["201"] > 1 {
		t.Fatalf("%d winners for one seat", got["201"])
	}
	c := checkConsistency(t, show)
	if c.Held != got["201"] {
		t.Fatalf("held=%d, winners=%d", c.Held, got["201"])
	}
}

// Holds expire lazily: once held_until passes, the seat counts as available,
// someone else can take it, the original owner's cancel says expired and must
// not touch the new holder, and the original owner's quota is freed.
func TestExpiredHold(t *testing.T) {
	ctx := context.Background()
	show := createShow(t, showOpts{Seats: 20, Limit: 1})
	id := runID(t)
	alice, bob := token(t, "exp-alice-"+id), token(t, "exp-bob-"+id)

	r := reserve(alice, show, "k", "A1")
	if r.Status != 201 {
		t.Fatal(r.Body)
	}
	// Fast-forward this hold past its expiry instead of sleeping for the TTL.
	if _, err := pool.Exec(ctx, `
		WITH r AS (UPDATE reservations SET expires_at = now() - interval '1 second'
		           WHERE id = $1 RETURNING id)
		UPDATE seats SET held_until = now() - interval '1 second'
		WHERE reservation_id IN (SELECT id FROM r)`, r.str("id")); err != nil {
		t.Fatal(err)
	}

	if c := checkConsistency(t, show); c.Held != 0 || c.Available != 20 {
		t.Fatalf("expired hold should read as available: %+v", c)
	}
	if b := reserve(bob, show, "k", "A1"); b.Status != 201 {
		t.Fatalf("bob taking expired seat: %d %v", b.Status, b.Body)
	}
	if c := cancel(alice, r.str("id")); c.Status != 409 || c.errCode() != "reservation_expired" {
		t.Fatalf("cancel of expired hold: want 409 reservation_expired, got %d %v", c.Status, c.Body)
	}
	// Bob still holds A1: alice's late cancel didn't release his seat.
	var holder string
	if err := pool.QueryRow(ctx, `SELECT coalesce(user_id, '') FROM seats WHERE show_id = $1 AND label = 'A1'`, show).Scan(&holder); err != nil {
		t.Fatal(err)
	}
	if holder != "exp-bob-"+id {
		t.Fatalf("A1 holder = %q, want bob", holder)
	}
	// Alice's limit-1 quota no longer counts the expired hold.
	if a := reserve(alice, show, "k2", "A2"); a.Status != 201 {
		t.Fatalf("alice after expiry: want 201, got %d %v", a.Status, a.Body)
	}
	checkConsistency(t, show)
}

func TestCancelUnknown(t *testing.T) {
	tok := token(t, "unk-"+runID(t))
	for _, id := range []string{"00000000-0000-0000-0000-000000000000", "not-a-uuid"} {
		if r := cancel(tok, id); r.Status != 404 || r.errCode() != "reservation_not_found" {
			t.Errorf("cancel %q: want 404, got %d %v", id, r.Status, r.Body)
		}
	}
}
