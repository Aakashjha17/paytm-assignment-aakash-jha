//go:build integration

package integration

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

// A body that claims to be someone else is ignored: the reservation belongs to
// the token's user, whatever user_id/userId/owner the body says.
func TestSpoofedBodyActsAsTokenUser(t *testing.T) {
	show := createShow(t, showOpts{Seats: 20})
	id := runID(t)
	mallory, victim := "mallory-"+id, "victim-"+id
	tok := token(t, mallory)

	r := do("POST", "/shows/"+strconv.FormatInt(show, 10)+"/reservations", tok,
		map[string]string{"Idempotency-Key": "k", "X-User-ID": victim},
		map[string]any{"seats": []string{"A1"}, "user_id": victim, "userId": victim, "owner": victim})
	if r.Status != 201 {
		t.Fatalf("%d %v", r.Status, r.Body)
	}
	if got := r.str("user_id"); got != mallory {
		t.Fatalf("reservation user_id = %q, want token user %q", got, mallory)
	}
	var owner string
	if err := pool.QueryRow(context.Background(),
		`SELECT user_id FROM seats WHERE show_id = $1 AND label = 'A1'`, show).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != mallory {
		t.Fatalf("seat owner in DB = %q, want %q", owner, mallory)
	}
	// And it counts against mallory's quota, not the victim's.
	if n := liveSeatsOf(t, show, victim); n != 0 {
		t.Fatalf("victim charged with %d seats", n)
	}
}

// Only the owner can cancel. Anyone else gets 404, the same answer as for a
// reservation that doesn't exist, and the hold is untouched. Many attackers
// trying at once change nothing either.
func TestOnlyOwnerCanCancel(t *testing.T) {
	show := createShow(t, showOpts{Seats: 20})
	id := runID(t)
	owner := token(t, "own-"+id)
	others := tokens(t, "thief-"+id, 50)

	r := reserve(owner, show, "k", "A1")
	if r.Status != 201 {
		t.Fatal(r.Body)
	}
	rs := storm(len(others), func(i int) resp { return cancel(others[i], r.str("id")) })
	if got := tally(t, rs); got["404 reservation_not_found"] != len(others) {
		t.Fatalf("want all 404, got %v", got)
	}
	if c := checkConsistency(t, show); c.Held != 1 {
		t.Fatalf("hold was disturbed: %+v", c)
	}
	if c := cancel(owner, r.str("id")); c.Status != 200 {
		t.Fatalf("owner cancel: %d %v", c.Status, c.Body)
	}
}

// No token, a tampered token, or a token signed with another key: 401, and
// nothing is reserved.
func TestBadTokensRejected(t *testing.T) {
	show := createShow(t, showOpts{Seats: 20})
	good := token(t, "tok-"+runID(t))
	parts := strings.Split(good, ".")
	forgedPayload := strings.Split(token(t, "someone-else"), ".")[1]

	for name, tok := range map[string]string{
		"none":             "",
		"garbage":          "garbage",
		"payload swapped":  parts[0] + "." + forgedPayload + "." + parts[2],
		"signature broken": parts[0] + "." + parts[1] + ".AAAA" + parts[2][4:],
	} {
		r := reserve(tok, show, "k-"+name, "A1")
		if r.Status != 401 {
			t.Errorf("%s: want 401, got %d %v", name, r.Status, r.Body)
		}
	}
	if c := checkConsistency(t, show); c.Held != 0 {
		t.Fatalf("a rejected request reserved something: %+v", c)
	}
}
