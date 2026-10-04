package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"

	"seat-reservation/internal/auth"
	"seat-reservation/internal/booking"
	"seat-reservation/internal/store"
)

const idempotencyKeyHeader = "Idempotency-Key"

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Only seats is read from the body. Any user_id or similar field a client
// sends is ignored: the caller is whoever the token says.
type reserveBody struct {
	Seats []string `json:"seats"`
}

func (a *API) reserve(w http.ResponseWriter, r *http.Request) {
	showID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || showID < 1 {
		writeError(w, r, http.StatusNotFound, "show_not_found", "no such show")
		return
	}
	var body reserveBody
	if !decodeJSON(w, r, &body) {
		return
	}
	req, err := booking.NewReserveRequest(showID, auth.UserFrom(r.Context()), r.Header.Get(idempotencyKeyHeader), body.Seats)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, booking.ErrorCode(err), err.Error())
		return
	}
	res, err := a.store.Reserve(r.Context(), req)
	a.writeResult(w, r, res, err)
}

func (a *API) cancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !uuidPattern.MatchString(id) {
		writeError(w, r, http.StatusNotFound, "reservation_not_found", "no such reservation")
		return
	}
	res, err := a.store.Cancel(r.Context(), id, auth.UserFrom(r.Context()))
	a.writeResult(w, r, res, err)
}

// writeResult renders a database decision via the booking outcome table.
func (a *API) writeResult(w http.ResponseWriter, r *http.Request, res store.Result, err error) {
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// The client hung up while we waited; nobody is reading this.
			// 499 keeps it out of the 5xx count; the transaction rolled back.
			writeError(w, r, 499, "client_closed_request", "client closed request")
			return
		}
		internalError(w, r, err)
		return
	}
	if st := stateFrom(r.Context()); st != nil {
		st.outcome = string(res.Outcome)
	}
	spec, ok := res.Outcome.Spec()
	if !ok {
		internalError(w, r, fmt.Errorf("unknown outcome %q", res.Outcome))
		return
	}
	if spec.OK {
		if res.Outcome == booking.Replayed {
			w.Header().Set("Idempotent-Replayed", "true")
		}
		writeJSON(w, spec.Status, res.Reservation)
		return
	}
	writeErrorDetails(w, r, spec.Status, spec.Code, spec.Message, res.Detail)
}
