package booking

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const MaxSeatsPerRequest = 10

var (
	ErrMissingKey    = errors.New("Idempotency-Key header is required")
	ErrInvalidKey    = errors.New("Idempotency-Key must be 1-128 visible ASCII characters")
	ErrNoSeats       = errors.New("seats must be a non-empty list")
	ErrTooManySeats  = fmt.Errorf("at most %d seats per request", MaxSeatsPerRequest)
	ErrDuplicateSeat = errors.New("seat listed more than once")
	ErrInvalidSeat   = errors.New("seat label must look like A1 or AB12")
)

var (
	keyPattern  = regexp.MustCompile(`^[\x21-\x7E]{1,128}$`)
	seatPattern = regexp.MustCompile(`^[A-Z]{1,3}[1-9][0-9]{0,3}$`)
)

// ErrorCode maps a validation error to its stable API error code.
func ErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrMissingKey):
		return "missing_idempotency_key"
	case errors.Is(err, ErrInvalidKey):
		return "invalid_idempotency_key"
	case errors.Is(err, ErrNoSeats), errors.Is(err, ErrTooManySeats),
		errors.Is(err, ErrDuplicateSeat), errors.Is(err, ErrInvalidSeat):
		return "invalid_seats"
	}
	return "invalid_request"
}

// ReserveRequest is a validated, canonical reserve call. UserID always comes
// from the verified token, never from the request body.
type ReserveRequest struct {
	ShowID         int64
	UserID         string
	IdempotencyKey string
	Seats          []string // trimmed, upper-cased, unique, sorted
}

func NewReserveRequest(showID int64, userID, key string, seats []string) (ReserveRequest, error) {
	if key == "" {
		return ReserveRequest{}, ErrMissingKey
	}
	if !keyPattern.MatchString(key) {
		return ReserveRequest{}, ErrInvalidKey
	}
	if len(seats) == 0 {
		return ReserveRequest{}, ErrNoSeats
	}
	if len(seats) > MaxSeatsPerRequest {
		return ReserveRequest{}, ErrTooManySeats
	}
	canon := make([]string, len(seats))
	for i, s := range seats {
		s = strings.ToUpper(strings.TrimSpace(s))
		if !seatPattern.MatchString(s) {
			return ReserveRequest{}, fmt.Errorf("%w: %q", ErrInvalidSeat, seats[i])
		}
		canon[i] = s
	}
	slices.Sort(canon)
	for i := 1; i < len(canon); i++ {
		if canon[i] == canon[i-1] {
			return ReserveRequest{}, fmt.Errorf("%w: %s", ErrDuplicateSeat, canon[i])
		}
	}
	return ReserveRequest{ShowID: showID, UserID: userID, IdempotencyKey: key, Seats: canon}, nil
}

// Fingerprint identifies what was asked for, independent of seat order or
// case, so a retry of the same request matches and a different request under
// the same key doesn't. The key is scoped per user in the database, so the
// user isn't part of it.
func (r ReserveRequest) Fingerprint() string {
	h := sha256.New()
	h.Write([]byte("v1\x00show=" + strconv.FormatInt(r.ShowID, 10) + "\x00seats=" + strings.Join(r.Seats, ",")))
	return hex.EncodeToString(h.Sum(nil))
}
