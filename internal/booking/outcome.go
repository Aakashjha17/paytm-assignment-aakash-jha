package booking

import "net/http"

// Outcome is the result of a reserve or cancel decision. The database function
// returns one of these strings; this table is the single place that turns it
// into an HTTP status and error code. Every outcome is a 2xx or 4xx: a decline
// is a normal business answer, never a server error.
type Outcome string

const (
	// Reserve outcomes.
	Reserved            Outcome = "reserved"
	Replayed            Outcome = "replayed"
	SeatTaken           Outcome = "seat_taken"
	PerUserLimit        Outcome = "per_user_limit"
	IdempotencyConflict Outcome = "idempotency_conflict"
	SeatNotFound        Outcome = "seat_not_found"
	ShowNotFound        Outcome = "show_not_found"

	// Cancel outcomes.
	Cancelled           Outcome = "cancelled"
	AlreadyCancelled    Outcome = "already_cancelled"
	ReservationNotFound Outcome = "not_found"
	ReservationExpired  Outcome = "expired"
	NotCancellable      Outcome = "not_cancellable"
)

// Spec says how an outcome is presented. When OK is true the response body is
// the reservation; otherwise it is an error envelope with Code and Message.
// Reason is the metric label: each response increments exactly one counter
// with it (reservations_confirmed_total for "confirmed", otherwise
// reservations_declined_total{reason} or reservation_cancellations_total{outcome}).
type Spec struct {
	Status  int
	OK      bool
	Code    string
	Message string
	Reason  string
}

var outcomes = map[Outcome]Spec{
	Reserved: {Status: http.StatusCreated, OK: true, Reason: "confirmed"},
	Replayed: {Status: http.StatusOK, OK: true, Reason: "idempotent-replay"},
	SeatTaken: {Status: http.StatusConflict, Code: "seat_taken", Reason: "seat-taken",
		Message: "one or more seats are already held or sold"},
	PerUserLimit: {Status: http.StatusConflict, Code: "per_user_limit", Reason: "per-user-limit",
		Message: "this would exceed the per-user seat limit for the show"},
	IdempotencyConflict: {Status: http.StatusConflict, Code: "idempotency_key_reused", Reason: "idempotency-conflict",
		Message: "this Idempotency-Key was already used with a different request"},
	SeatNotFound: {Status: http.StatusUnprocessableEntity, Code: "seat_not_found", Reason: "seat-not-found",
		Message: "one or more seats do not exist in this show"},
	ShowNotFound: {Status: http.StatusNotFound, Code: "show_not_found", Reason: "show-not-found",
		Message: "no such show"},

	Cancelled:        {Status: http.StatusOK, OK: true, Reason: "cancelled"},
	AlreadyCancelled: {Status: http.StatusOK, OK: true, Reason: "already-cancelled"},
	// Someone else's reservation is reported exactly like a missing one, so
	// IDs can't be probed for existence.
	ReservationNotFound: {Status: http.StatusNotFound, Code: "reservation_not_found", Reason: "not-found",
		Message: "no such reservation"},
	ReservationExpired: {Status: http.StatusConflict, Code: "reservation_expired", Reason: "expired",
		Message: "the hold has already expired and its seats were released"},
	NotCancellable: {Status: http.StatusConflict, Code: "not_cancellable", Reason: "not-cancellable",
		Message: "a confirmed reservation cannot be cancelled"},
}

// AllOutcomes lists every outcome, so tests can check the table is complete.
var AllOutcomes = []Outcome{
	Reserved, Replayed, SeatTaken, PerUserLimit, IdempotencyConflict, SeatNotFound, ShowNotFound,
	Cancelled, AlreadyCancelled, ReservationNotFound, ReservationExpired, NotCancellable,
}

// Spec looks up the presentation for o. ok is false for an outcome the
// database returned that this table doesn't know, which is a bug.
func (o Outcome) Spec() (Spec, bool) {
	s, ok := outcomes[o]
	return s, ok
}
