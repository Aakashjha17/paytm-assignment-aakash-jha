package store

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"seat-reservation/internal/booking"
)

type Reservation struct {
	ID          string    `json:"id"`
	ShowID      int64     `json:"show_id"`
	UserID      string    `json:"user_id"`
	Status      string    `json:"status"`
	Seats       []string  `json:"seats"`
	AmountPaise int64     `json:"amount_paise"`
	ExpiresAt   time.Time `json:"expires_at"`
	CreatedAt   time.Time `json:"created_at"`
}

// Result is what the database decided. Reservation is set when the outcome
// refers to an existing reservation; Detail carries decline specifics such as
// which seats were taken.
type Result struct {
	Outcome     booking.Outcome
	Reservation *Reservation
	Detail      []string
}

const resultColumns = `r_outcome, r_id::text, r_show_id, r_user_id, r_status, r_seats,
	r_amount_paise, r_expires_at, r_created_at, r_detail`

// Reserve runs the whole reserve decision as one call to reserve_seats, i.e.
// one statement and one transaction. See migrations/0002 for the mechanism.
func (s *Store) Reserve(ctx context.Context, req booking.ReserveRequest) (Result, error) {
	return s.callDecision(ctx, `SELECT `+resultColumns+` FROM reserve_seats($1, $2, $3, $4, $5)`,
		req.ShowID, req.UserID, req.IdempotencyKey, req.Fingerprint(), req.Seats)
}

// Cancel releases userID's reservation id. Anyone else's reservation is
// reported as not found.
func (s *Store) Cancel(ctx context.Context, id, userID string) (Result, error) {
	return s.callDecision(ctx, `SELECT `+resultColumns+` FROM cancel_reservation($1, $2)`, id, userID)
}

// retries counts transactions re-run after a deadlock or serialization
// failure. The lock order should make this stay at zero; tests assert it and
// it will be exported as a metric.
var retries atomic.Int64

func Retries() int64 { return retries.Load() }

const maxAttempts = 4

func (s *Store) callDecision(ctx context.Context, sql string, args ...any) (Result, error) {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		res, err := s.scanDecision(ctx, sql, args...)
		if err == nil || !retryable(err) {
			return res, err
		}
		lastErr = err
		retries.Add(1)
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-time.After(time.Duration(attempt*attempt) * 5 * time.Millisecond):
		}
	}
	return Result{}, fmt.Errorf("gave up after %d attempts: %w", maxAttempts, lastErr)
}

func (s *Store) scanDecision(ctx context.Context, sql string, args ...any) (Result, error) {
	var (
		outcome   string
		id        *string
		showID    *int64
		userID    *string
		status    *string
		seats     []string
		amount    *int64
		expiresAt *time.Time
		createdAt *time.Time
		detail    []string
	)
	err := s.pool.QueryRow(ctx, sql, args...).Scan(
		&outcome, &id, &showID, &userID, &status, &seats, &amount, &expiresAt, &createdAt, &detail)
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, errors.New("decision function returned no row")
	}
	if err != nil {
		return Result{}, err
	}
	res := Result{Outcome: booking.Outcome(outcome), Detail: detail}
	if id != nil {
		res.Reservation = &Reservation{
			ID: *id, ShowID: *showID, UserID: *userID, Status: *status, Seats: seats,
			AmountPaise: *amount, ExpiresAt: *expiresAt, CreatedAt: *createdAt,
		}
	}
	return res, nil
}

func retryable(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case "40P01", // deadlock_detected
		"40001": // serialization_failure
		return true
	}
	return false
}
