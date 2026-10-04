//go:build integration

// Package integration drives the real HTTP stack (router, middleware, auth,
// handlers, store, SQL functions) against a real Postgres, concurrently.
//
//	docker compose up -d --wait db
//	go test -race -tags integration -count=5 ./test/integration/
//
// TEST_DATABASE_URL points at any database on the server (default: the compose
// one); a fresh seats_it database is created from scratch for each run.
package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"seat-reservation/internal/auth"
	"seat-reservation/internal/httpapi"
	"seat-reservation/internal/metrics"
	"seat-reservation/internal/store"
	"seat-reservation/migrations"
)

const (
	testDB    = "seats_it"
	adminKey  = "integration-admin-key-0123456789"
	jwtSecret = "integration-jwt-secret-0123456789"
)

var (
	baseURL string
	client  *http.Client
	pool    *pgxpool.Pool
	authn   *auth.Authenticator
	st      *store.Store
	mtx     *metrics.Metrics
)

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	ctx := context.Background()
	adminURL := os.Getenv("TEST_DATABASE_URL")
	if adminURL == "" {
		adminURL = "postgres://postgres:postgres@localhost:55432/postgres?sslmode=disable"
	}

	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration: cannot reach Postgres at %s (run `docker compose up -d --wait db`): %v\n", adminURL, err)
		return 1
	}
	for _, q := range []string{
		"DROP DATABASE IF EXISTS " + testDB + " WITH (FORCE)",
		"CREATE DATABASE " + testDB,
	} {
		if _, err := admin.Exec(ctx, q); err != nil {
			fmt.Fprintf(os.Stderr, "integration: %s: %v\n", q, err)
			return 1
		}
	}
	admin.Close(ctx)

	cfg, err := pgxpool.ParseConfig(adminURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cfg.ConnConfig.Database = testDB
	cfg.MaxConns = 20
	if pool, err = pgxpool.NewWithConfig(ctx, cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer pool.Close()

	quiet := slog.New(slog.NewJSONHandler(io.Discard, nil))
	if err := store.Migrate(ctx, pool, migrations.FS, quiet); err != nil {
		fmt.Fprintf(os.Stderr, "integration: migrate: %v\n", err)
		return 1
	}

	authn = auth.New(jwtSecret, adminKey, time.Hour)
	st = store.New(pool)
	mtx = metrics.New()
	mtx.Register(metrics.NewSeatCollector(st), metrics.NewPoolCollector(st))
	api := httpapi.New(httpapi.Deps{
		Store: st, Auth: authn, Metrics: mtx,
		Migrated: func() bool { return true },
	})
	srv := httptest.NewServer(api.Handler(quiet))
	defer srv.Close()
	baseURL = srv.URL
	client = &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        2000,
			MaxIdleConnsPerHost: 2000,
			IdleConnTimeout:     30 * time.Second,
		},
	}
	code := m.Run()

	// After everything (every storm, cancel and expiry), audit the whole
	// database. Any mismatch fails the run even if each test passed.
	res, err := st.Audit(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration: final audit: %v\n", err)
		return 1
	}
	if res.Total() != 0 {
		fmt.Fprintf(os.Stderr, "integration: final audit found violations: %v %v\n", res.Mismatches, res.Samples)
		return 1
	}
	fmt.Fprintf(os.Stderr, "integration: final audit clean (%d checks)\n", len(res.Mismatches))
	return code
}

// ---- HTTP helpers -------------------------------------------------------
// These never call t.Fatal so they are safe from storm goroutines; a transport
// failure comes back as Status 0 with Err set.

type resp struct {
	Status int
	Header http.Header
	Body   map[string]any
	Err    error
}

func (r resp) str(key string) string { s, _ := r.Body[key].(string); return s }

// errCode is the error.code of a decline, or "".
func (r resp) errCode() string {
	e, _ := r.Body["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func do(method, path, token string, headers map[string]string, body any) resp {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return resp{Err: err}
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, baseURL+path, rd)
	if err != nil {
		return resp{Err: err}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := client.Do(req)
	if err != nil {
		return resp{Err: err}
	}
	defer res.Body.Close()
	out := resp{Status: res.StatusCode, Header: res.Header}
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return resp{Status: res.StatusCode, Err: err}
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out.Body); err != nil {
			out.Err = fmt.Errorf("decode %q: %w", raw, err)
		}
	}
	return out
}

func reserve(token string, showID int64, key string, seats ...string) resp {
	return do("POST", fmt.Sprintf("/shows/%d/reservations", showID), token,
		map[string]string{"Idempotency-Key": key}, map[string]any{"seats": seats})
}

func cancel(token, reservationID string) resp {
	return do("DELETE", "/reservations/"+reservationID, token, nil, nil)
}

// ---- fixtures -----------------------------------------------------------

type showOpts struct {
	Seats, PerRow, Limit int
}

func createShow(t *testing.T, o showOpts) int64 {
	t.Helper()
	if o.PerRow == 0 {
		o.PerRow = 20
	}
	if o.Limit == 0 {
		o.Limit = 4
	}
	r := do("POST", "/shows", "", map[string]string{"X-Admin-Key": adminKey}, map[string]any{
		"name": t.Name(), "total_seats": o.Seats, "seats_per_row": o.PerRow,
		"price_paise": 15000, "per_user_limit": o.Limit,
	})
	if r.Err != nil || r.Status != http.StatusCreated {
		t.Fatalf("create show: %d %v %v", r.Status, r.Body, r.Err)
	}
	return int64(r.Body["id"].(float64))
}

// runID makes user names unique per test invocation, so -count=N reruns
// never collide on (user, idempotency key).
func runID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func token(t *testing.T, user string) string {
	t.Helper()
	tok, _, err := authn.Mint(user)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func tokens(t *testing.T, prefix string, n int) []string {
	t.Helper()
	out := make([]string, n)
	for i := range out {
		out[i] = token(t, fmt.Sprintf("%s-%d", prefix, i))
	}
	return out
}

// storm runs fn(0..n-1) concurrently, releasing them all at once so they
// genuinely race rather than trickle in as goroutines start.
func storm(n int, fn func(i int) resp) []resp {
	out := make([]resp, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			out[i] = fn(i)
		}(i)
	}
	close(start)
	wg.Wait()
	return out
}

// tally counts responses by "status code_or_outcome" and fails on any
// transport error or 5xx: declines must be 4xx, never server errors.
func tally(t *testing.T, rs []resp) map[string]int {
	t.Helper()
	m := map[string]int{}
	for i, r := range rs {
		if r.Err != nil {
			t.Errorf("request %d: transport error: %v", i, r.Err)
			continue
		}
		if r.Status >= 500 {
			t.Errorf("request %d: got %d %v", i, r.Status, r.Body)
		}
		k := fmt.Sprint(r.Status)
		if c := r.errCode(); c != "" {
			k += " " + c
		}
		m[k]++
	}
	return m
}

// ---- invariants ---------------------------------------------------------

type counts struct{ Available, Held, Confirmed int }

func showCounts(t *testing.T, showID int64) (counts, int) {
	t.Helper()
	r := do("GET", fmt.Sprintf("/shows/%d", showID), "", nil, nil)
	if r.Err != nil || r.Status != 200 {
		t.Fatalf("get show: %d %v", r.Status, r.Err)
	}
	c := r.Body["counts"].(map[string]any)
	return counts{
		Available: int(c["available"].(float64)),
		Held:      int(c["held"].(float64)),
		Confirmed: int(c["confirmed"].(float64)),
	}, int(r.Body["total_seats"].(float64))
}

// checkConsistency asserts every invariant we promise, from both the API and
// the database's point of view. It is the oracle for every concurrency test.
func checkConsistency(t *testing.T, showID int64) counts {
	t.Helper()
	ctx := context.Background()

	c, total := showCounts(t, showID)
	if c.Available+c.Held+c.Confirmed != total {
		t.Errorf("reconciliation: %d + %d + %d != %d", c.Available, c.Held, c.Confirmed, total)
	}

	// The API's counts agree with the table.
	var dbHeld, dbConfirmed int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status = 'held' AND held_until > now()),
		       count(*) FILTER (WHERE status = 'confirmed')
		FROM seats WHERE show_id = $1`, showID).Scan(&dbHeld, &dbConfirmed); err != nil {
		t.Fatal(err)
	}
	if dbHeld != c.Held || dbConfirmed != c.Confirmed {
		t.Errorf("API counts %+v disagree with DB held=%d confirmed=%d", c, dbHeld, dbConfirmed)
	}

	violations := map[string]string{
		// All-or-nothing: a live reservation owns exactly the seats it lists.
		"live reservation without all its seats": `
			SELECT r.id::text FROM reservations r
			WHERE r.show_id = $1
			  AND (r.status = 'confirmed' OR (r.status = 'held' AND r.expires_at > now()))
			  AND cardinality(r.seat_labels) <> (
			      SELECT count(*) FROM seats s
			      WHERE s.show_id = $1 AND s.reservation_id = r.id
			        AND s.label = ANY (r.seat_labels) AND s.user_id = r.user_id)`,
		// No orphan or mismatched occupied seat.
		"occupied seat not backed by its owner's live reservation": `
			SELECT s.label FROM seats s LEFT JOIN reservations r ON r.id = s.reservation_id
			WHERE s.show_id = $1
			  AND (s.status = 'confirmed' OR (s.status = 'held' AND s.held_until > now()))
			  AND (r.id IS NULL OR r.user_id <> s.user_id OR NOT (s.label = ANY (r.seat_labels))
			       OR r.status NOT IN ('held', 'confirmed'))`,
		// Cancelled reservations hold nothing.
		"cancelled reservation still holds seats": `
			SELECT s.label FROM seats s JOIN reservations r ON r.id = s.reservation_id
			WHERE s.show_id = $1 AND r.status = 'cancelled'`,
		// Per-user limit.
		"user over per-user limit": `
			SELECT s.user_id FROM seats s JOIN shows sh ON sh.id = s.show_id
			WHERE s.show_id = $1
			  AND (s.status = 'confirmed' OR (s.status = 'held' AND s.held_until > now()))
			GROUP BY s.user_id, sh.per_user_limit HAVING count(*) > sh.per_user_limit`,
	}
	for name, q := range violations {
		rows, err := pool.Query(ctx, q, showID)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		bad, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(bad) > 0 {
			t.Errorf("%s: %v", name, bad)
		}
	}
	return c
}

// noRetries fails if any transaction had to be retried for a deadlock or
// serialization failure while fn ran. The lock order should make that zero.
func noRetries(t *testing.T, fn func()) {
	t.Helper()
	before := store.Retries()
	fn()
	if d := store.Retries() - before; d != 0 {
		t.Errorf("%d transaction(s) retried after deadlock/serialization failure; lock order is broken", d)
	}
}

// ---- metrics ------------------------------------------------------------

// scrape fetches /metrics and returns every sample keyed by its full series,
// e.g. `seats_held{show_id="3"}` or `reservations_confirmed_total`.
func scrape(t *testing.T) map[string]float64 {
	t.Helper()
	res, err := client.Get(baseURL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		v, err := strconv.ParseFloat(line[i+1:], 64)
		if err != nil {
			t.Fatalf("bad metrics line %q", line)
		}
		out[line[:i]] = v
	}
	return out
}
