package main

import (
	"fmt"
	"math/rand/v2"

	"seat-reservation/internal/store"
)

// The mixed workload, as shares of -requests. Each kind exercises one of the
// correctness bars; the checks in check.go know what each should produce.
const (
	pctHot      = 40 // many users, one request each, all at the few hot seats
	pctRetry    = 10 // exact duplicates (same user, key, seats) of hot requests, fired concurrently
	pctCold     = 37 // 3 requests per user, 1-3 random non-hot seats; some cancel what they win
	pctGreedy   = 5  // users firing 10 parallel single-seat reserves at a limit-4 show
	pctConflict = 4  // same key, two different seat sets, concurrently
	// the rest: spoof, a body user_id claiming to be someone else
)

// buildWorkload returns the jobs, shuffled so the kinds interleave, and how
// many users they need. Users never overlap between kinds except retries
// (which reuse a hot user and key by design).
func buildWorkload(cfg config) ([]*job, int) {
	rng := rand.New(rand.NewPCG(cfg.seed, 0))
	label := func(n int) string { return store.SeatLabel(n, 50) }
	coldSeat := func() string { return label(cfg.hotSeats + 1 + rng.IntN(cfg.seats-cfg.hotSeats)) }
	coldSeats := func(n int) []string {
		set := map[string]bool{}
		for len(set) < n {
			set[coldSeat()] = true
		}
		out := make([]string, 0, n)
		for s := range set {
			out = append(out, s)
		}
		return out
	}

	R := cfg.requests
	var jobs []*job
	user := 0
	newUser := func() int { user++; return user - 1 }

	// Hot-seat storm.
	nHot := R * pctHot / 100
	hot := make([]*job, 0, nHot)
	for i := 0; i < nHot; i++ {
		seat := label(i%cfg.hotSeats + 1)
		hot = append(hot, &job{kind: "hot", user: newUser(), key: "k", seats: []string{seat}, hotSeat: seat})
	}
	jobs = append(jobs, hot...)

	// Retries: the same request again, possibly arriving before the original.
	for i := 0; i < R*pctRetry/100; i++ {
		o := hot[rng.IntN(len(hot))]
		jobs = append(jobs, &job{kind: "hot-retry", user: o.user, key: o.key, seats: o.seats, hotSeat: o.hotSeat})
	}

	// Cold traffic with cancels.
	for i, u := 0, newUser(); i < R*pctCold/100; i++ {
		if i > 0 && i%3 == 0 {
			u = newUser()
		}
		jobs = append(jobs, &job{kind: "cold", user: u, key: fmt.Sprintf("c%d", i%3),
			seats: coldSeats(1 + rng.IntN(3)), cancel: rng.IntN(5) == 0})
	}

	// Per-user-limit abuse: 10 parallel reserves each.
	for g := 0; g < R*pctGreedy/100/10; g++ {
		u := newUser()
		for k := 0; k < 10; k++ {
			jobs = append(jobs, &job{kind: "greedy", user: u, key: fmt.Sprintf("g%d", k), seats: coldSeats(1)})
		}
	}

	// Same key, different body.
	for c := 0; c < R*pctConflict/100/2; c++ {
		u := newUser()
		jobs = append(jobs,
			&job{kind: "conflict", user: u, key: "same", seats: coldSeats(1)},
			&job{kind: "conflict", user: u, key: "same", seats: coldSeats(2)})
	}

	// Spoofed identity: claim to be a hot-storm user in the body.
	for len(jobs) < R {
		victim := hot[rng.IntN(len(hot))].user
		jobs = append(jobs, &job{kind: "spoof", user: newUser(), key: "k", seats: coldSeats(1), spoofAs: fmt.Sprint(victim)})
	}

	rng.Shuffle(len(jobs), func(a, b int) { jobs[a], jobs[b] = jobs[b], jobs[a] })
	return jobs, user
}
