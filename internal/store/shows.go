package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type Show struct {
	ID             int64     `json:"id"`
	Name           string    `json:"name"`
	TotalSeats     int       `json:"total_seats"`
	PricePaise     int64     `json:"price_paise"`
	PerUserLimit   int       `json:"per_user_limit"`
	HoldTTLSeconds int       `json:"hold_ttl_seconds"`
	CreatedAt      time.Time `json:"created_at"`
}

type NewShow struct {
	Name           string
	TotalSeats     int
	SeatsPerRow    int
	PricePaise     int64
	PerUserLimit   int
	HoldTTLSeconds int
}

type Seat struct {
	Label  string `json:"label"`
	Status string `json:"status"`
}

type SeatCounts struct {
	Available int `json:"available"`
	Held      int `json:"held"`
	Confirmed int `json:"confirmed"`
}

// CreateShow inserts the show and all of its seats in one transaction, so a
// show is never visible with a partial seat map.
func (s *Store) CreateShow(ctx context.Context, in NewShow) (Show, error) {
	var show Show
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO shows (name, total_seats, price_paise, per_user_limit, hold_ttl_seconds)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING id, name, total_seats, price_paise, per_user_limit, hold_ttl_seconds, created_at`,
			in.Name, in.TotalSeats, in.PricePaise, in.PerUserLimit, in.HoldTTLSeconds,
		).Scan(&show.ID, &show.Name, &show.TotalSeats, &show.PricePaise, &show.PerUserLimit, &show.HoldTTLSeconds, &show.CreatedAt)
		if err != nil {
			return fmt.Errorf("insert show: %w", err)
		}

		rows := make([][]any, in.TotalSeats)
		for i := range rows {
			n := i + 1
			rows[i] = []any{show.ID, n, SeatLabel(n, in.SeatsPerRow)}
		}
		copied, err := tx.CopyFrom(ctx, pgx.Identifier{"seats"}, []string{"show_id", "seat_no", "label"}, pgx.CopyFromRows(rows))
		if err != nil {
			return fmt.Errorf("copy seats: %w", err)
		}
		if int(copied) != in.TotalSeats {
			return fmt.Errorf("copied %d seats, want %d", copied, in.TotalSeats)
		}
		return nil
	})
	return show, err
}

// GetShow returns the show, every seat's effective status, and counts derived
// from that same list. Both reads run in one REPEATABLE READ snapshot, and
// counts are tallied from the rows rather than a separate COUNT query, so the
// invariant available + held + confirmed == total_seats holds in every
// response even while reservations are landing.
//
// A hold whose held_until has passed is reported as available: expiry is a
// read-time fact, not something that waits on a sweeper.
func (s *Store) GetShow(ctx context.Context, id int64) (Show, []Seat, SeatCounts, error) {
	var (
		show   Show
		seats  []Seat
		counts SeatCounts
	)
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			SELECT id, name, total_seats, price_paise, per_user_limit, hold_ttl_seconds, created_at
			FROM shows WHERE id = $1`, id,
		).Scan(&show.ID, &show.Name, &show.TotalSeats, &show.PricePaise, &show.PerUserLimit, &show.HoldTTLSeconds, &show.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}

		rows, err := tx.Query(ctx, `
			SELECT label, `+effectiveSeatStatus+`
			FROM seats WHERE show_id = $1 ORDER BY seat_no`, id)
		if err != nil {
			return err
		}
		seats = make([]Seat, 0, show.TotalSeats)
		for rows.Next() {
			var st Seat
			if err := rows.Scan(&st.Label, &st.Status); err != nil {
				return err
			}
			switch st.Status {
			case "available":
				counts.Available++
			case "held":
				counts.Held++
			case "confirmed":
				counts.Confirmed++
			}
			seats = append(seats, st)
		}
		return rows.Err()
	})
	return show, seats, counts, err
}

// effectiveSeatStatus is the one definition of a seat's status as the API and
// metrics report it: a hold past held_until counts as available. GetShow and
// SeatCountsByShow both use it, which is why the gauges can't disagree with
// GET /shows.
const effectiveSeatStatus = `CASE WHEN status = 'held' AND held_until <= now() THEN 'available' ELSE status END`

type ShowSeatCounts struct {
	ShowID int64
	Total  int
	SeatCounts
}

// SeatCountsByShow feeds the seat gauges. One statement, so one snapshot:
// a scrape never sees half of a reservation.
func (s *Store) SeatCountsByShow(ctx context.Context) ([]ShowSeatCounts, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT sh.id, sh.total_seats,
		       count(*) FILTER (WHERE st.status = 'available'),
		       count(*) FILTER (WHERE st.status = 'held'),
		       count(*) FILTER (WHERE st.status = 'confirmed')
		FROM shows sh
		LEFT JOIN LATERAL (
		    SELECT `+effectiveSeatStatus+` AS status FROM seats WHERE seats.show_id = sh.id
		) st ON true
		GROUP BY sh.id, sh.total_seats
		ORDER BY sh.id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (ShowSeatCounts, error) {
		var c ShowSeatCounts
		err := row.Scan(&c.ShowID, &c.Total, &c.Available, &c.Held, &c.Confirmed)
		return c, err
	})
}

// SeatLabel turns a 1-based seat number into a row-letter + column label:
// with 20 per row, 1 -> A1, 20 -> A20, 21 -> B1, 521 -> AA1.
func SeatLabel(n, perRow int) string {
	row := (n - 1) / perRow
	col := (n-1)%perRow + 1
	return rowName(row) + fmt.Sprint(col)
}

// rowName is bijective base-26: 0 -> A, 25 -> Z, 26 -> AA, 27 -> AB.
func rowName(i int) string {
	var b []byte
	for i++; i > 0; i = (i - 1) / 26 {
		b = append([]byte{byte('A' + (i-1)%26)}, b...)
	}
	return string(b)
}
