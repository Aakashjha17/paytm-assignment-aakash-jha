package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"seat-reservation/internal/store"
)

const (
	maxSeatsPerShow       = 50_000
	defaultSeatsPerRow    = 20
	defaultPerUserLimit   = 4
	defaultHoldTTLSeconds = 300
)

// Pointers distinguish "omitted" (use the default) from an explicit zero
// (reject it).
type createShowRequest struct {
	Name           string `json:"name"`
	TotalSeats     int    `json:"total_seats"`
	SeatsPerRow    *int   `json:"seats_per_row"`
	PricePaise     *int64 `json:"price_paise"`
	PerUserLimit   *int   `json:"per_user_limit"`
	HoldTTLSeconds *int   `json:"hold_ttl_seconds"`
}

type showResponse struct {
	store.Show
	Counts store.SeatCounts `json:"counts"`
	Seats  []store.Seat     `json:"seats,omitempty"`
}

func (a *API) createShow(w http.ResponseWriter, r *http.Request) {
	var req createShowRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	in, msg := req.validate()
	if msg != "" {
		writeError(w, r, http.StatusBadRequest, "invalid_show", msg)
		return
	}
	show, err := a.store.CreateShow(r.Context(), in)
	if err != nil {
		internalError(w, r, err)
		return
	}
	w.Header().Set("Location", fmt.Sprintf("/shows/%d", show.ID))
	writeJSON(w, http.StatusCreated, showResponse{
		Show:   show,
		Counts: store.SeatCounts{Available: show.TotalSeats},
	})
}

func (a *API) getShow(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, r, http.StatusNotFound, "show_not_found", "no such show")
		return
	}
	show, seats, counts, err := a.store.GetShow(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, http.StatusNotFound, "show_not_found", "no such show")
		return
	}
	if err != nil {
		internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, showResponse{Show: show, Counts: counts, Seats: seats})
}

// validate applies defaults and bounds, returning a message for the first
// problem found. Money stays in integer paise end to end.
func (req createShowRequest) validate() (store.NewShow, string) {
	in := store.NewShow{
		Name:           strings.TrimSpace(req.Name),
		TotalSeats:     req.TotalSeats,
		SeatsPerRow:    defaultSeatsPerRow,
		PerUserLimit:   defaultPerUserLimit,
		HoldTTLSeconds: defaultHoldTTLSeconds,
	}
	if req.SeatsPerRow != nil {
		in.SeatsPerRow = *req.SeatsPerRow
	}
	if req.PerUserLimit != nil {
		in.PerUserLimit = *req.PerUserLimit
	}
	if req.HoldTTLSeconds != nil {
		in.HoldTTLSeconds = *req.HoldTTLSeconds
	}
	switch {
	case in.Name == "" || len(in.Name) > 200:
		return in, "name is required (max 200 chars)"
	case in.TotalSeats < 1 || in.TotalSeats > maxSeatsPerShow:
		return in, fmt.Sprintf("total_seats must be between 1 and %d", maxSeatsPerShow)
	case req.PricePaise == nil || *req.PricePaise < 0:
		return in, "price_paise is required and must be a non-negative integer"
	case in.SeatsPerRow < 1 || in.SeatsPerRow > 1000:
		return in, "seats_per_row must be between 1 and 1000"
	case in.PerUserLimit < 1:
		return in, "per_user_limit must be >= 1"
	case in.HoldTTLSeconds < 10 || in.HoldTTLSeconds > 3600:
		return in, "hold_ttl_seconds must be between 10 and 3600"
	}
	in.PricePaise = *req.PricePaise
	return in, ""
}
