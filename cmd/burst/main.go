// Command burst reproduces an on-sale stampede against a running service
// (local or live) and checks the outcome from the outside: only the public
// API and /metrics, never the database.
//
//	ADMIN_API_KEY=... go run ./cmd/burst -base-url https://example.up.railway.app
//
// It creates a fresh show, mints tokens for thousands of users, fires a mixed
// workload (hot-seat storm, retries with the same key, per-user-limit abuse,
// same-key-different-body, spoofed identity, cancels) with high concurrency,
// polls the reconciliation invariant during the burst, and finally reconciles
// API counts, client-observed outcomes, /metrics and the auditor. Exit status
// is non-zero if any check fails.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"time"
)

type config struct {
	base        string
	adminKey    string
	requests    int
	concurrency int
	hotSeats    int
	seats       int
	limit       int
	timeout     time.Duration
	pollEvery   time.Duration
	auditWait   time.Duration
	seed        uint64
	quiet       bool
}

func main() {
	var c config
	flag.StringVar(&c.base, "base-url", "http://localhost:8080", "service base URL")
	flag.StringVar(&c.adminKey, "admin-key", os.Getenv("ADMIN_API_KEY"), "admin key for creating the show (default $ADMIN_API_KEY)")
	flag.IntVar(&c.requests, "requests", 20000, "reserve requests to fire")
	flag.IntVar(&c.concurrency, "concurrency", 1000, "requests in flight at once")
	flag.IntVar(&c.hotSeats, "hot-seats", 5, "seats everyone in the hot-seat storm fights over")
	flag.IntVar(&c.seats, "seats", 5000, "seats in the show")
	flag.IntVar(&c.limit, "limit", 4, "per-user seat limit for the show")
	flag.DurationVar(&c.timeout, "timeout", 30*time.Second, "per-request timeout")
	flag.DurationVar(&c.pollEvery, "poll", 250*time.Millisecond, "how often to check the invariant during the burst")
	flag.DurationVar(&c.auditWait, "audit-wait", 75*time.Second, "how long to wait for a post-burst audit run")
	flag.Uint64Var(&c.seed, "seed", 0, "workload seed (0 = random)")
	flag.BoolVar(&c.quiet, "quiet", false, "no progress output")
	flag.Parse()

	if c.adminKey == "" {
		fmt.Fprintln(os.Stderr, "burst: an admin key is required (-admin-key or $ADMIN_API_KEY) to create the test show")
		os.Exit(2)
	}
	if c.seed == 0 {
		var b [8]byte
		_, _ = rand.Read(b[:])
		for _, x := range b {
			c.seed = c.seed<<8 | uint64(x)
		}
	}

	ok, err := run(context.Background(), c)
	if err != nil {
		fmt.Fprintln(os.Stderr, "burst: "+err.Error())
		os.Exit(2)
	}
	if !ok {
		os.Exit(1)
	}
}

func runID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
