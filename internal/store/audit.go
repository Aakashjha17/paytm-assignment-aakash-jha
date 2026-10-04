package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// An audit check is a query that returns the rows breaking one invariant.
// Healthy means every check returns nothing. These are deliberately written
// independently of the reserve/cancel code, as a second opinion on its work.
type auditCheck struct {
	name  string
	query string
}

// Live means: confirmed, or held and not yet expired.
var auditChecks = []auditCheck{
	{
		// available + held + confirmed == total_seats only if every show has
		// exactly total_seats seat rows (each row has exactly one status).
		name: "seat_count_mismatch",
		query: `
			SELECT 'show ' || sh.id || ': ' || count(s.seat_no) || ' seat rows, total_seats=' || sh.total_seats
			FROM shows sh LEFT JOIN seats s ON s.show_id = sh.id
			GROUP BY sh.id, sh.total_seats
			HAVING count(s.seat_no) <> sh.total_seats`,
	},
	{
		// All-or-nothing: a live reservation owns exactly the seats it lists.
		name: "reservation_missing_seats",
		query: `
			SELECT r.id::text FROM reservations r
			WHERE (r.status = 'confirmed' OR (r.status = 'held' AND r.expires_at > now()))
			  AND cardinality(r.seat_labels) <> (
			      SELECT count(*) FROM seats s
			      WHERE s.show_id = r.show_id AND s.reservation_id = r.id
			        AND s.user_id = r.user_id AND s.label = ANY (r.seat_labels)
			        AND s.status IN ('held', 'confirmed'))`,
	},
	{
		// Every occupied seat is backed by its owner's live reservation that
		// lists it. Catches double-sells, orphans, and cancelled-but-held.
		name: "orphan_occupied_seat",
		query: `
			SELECT 'show ' || s.show_id || ' seat ' || s.label
			FROM seats s LEFT JOIN reservations r ON r.id = s.reservation_id
			WHERE (s.status = 'confirmed' OR (s.status = 'held' AND s.held_until > now()))
			  AND (r.id IS NULL OR r.show_id <> s.show_id OR r.user_id <> s.user_id
			       OR NOT (s.label = ANY (r.seat_labels))
			       OR r.status NOT IN ('held', 'confirmed'))`,
	},
	{
		// A held seat expires exactly when its reservation does; if they
		// drift, the API and the per-user limit would disagree about it.
		name: "hold_expiry_mismatch",
		query: `
			SELECT 'show ' || s.show_id || ' seat ' || s.label
			FROM seats s JOIN reservations r ON r.id = s.reservation_id
			WHERE s.status = 'held' AND r.status = 'held' AND s.held_until <> r.expires_at`,
	},
	{
		name: "over_user_limit",
		query: `
			SELECT 'show ' || s.show_id || ' user ' || s.user_id || ': ' || count(*)
			FROM seats s JOIN shows sh ON sh.id = s.show_id
			WHERE s.status = 'confirmed' OR (s.status = 'held' AND s.held_until > now())
			GROUP BY s.show_id, s.user_id, sh.per_user_limit
			HAVING count(*) > sh.per_user_limit`,
	},
}

// AuditCheckNames lists every check, so a gauge can be reported (as zero) for
// each one even before the first violation.
func AuditCheckNames() []string {
	names := make([]string, len(auditChecks))
	for i, c := range auditChecks {
		names[i] = c.name
	}
	return names
}

type AuditResult struct {
	// Mismatches has an entry for every check: how many rows break it.
	Mismatches map[string]int
	// Samples holds up to 5 offending rows per failing check, for the log.
	Samples map[string][]string
}

func (r AuditResult) Total() int {
	n := 0
	for _, v := range r.Mismatches {
		n += v
	}
	return n
}

// Audit runs every check in one REPEATABLE READ snapshot, so concurrent
// reservations can't produce a false positive by landing between two checks:
// each reservation and its seats commit atomically, and we see all or none.
func (s *Store) Audit(ctx context.Context) (AuditResult, error) {
	res := AuditResult{Mismatches: map[string]int{}, Samples: map[string][]string{}}
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		for _, c := range auditChecks {
			rows, err := tx.Query(ctx, c.query)
			if err != nil {
				return err
			}
			bad, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				return err
			}
			res.Mismatches[c.name] = len(bad)
			if len(bad) > 0 {
				res.Samples[c.name] = bad[:min(5, len(bad))]
			}
		}
		return nil
	})
	return res, err
}
