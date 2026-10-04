package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"seat-reservation/internal/store"
)

type errorBody struct {
	Error     errorDetail `json:"error"`
	RequestID string      `json:"request_id,omitempty"`
}

type errorDetail struct {
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Details []string `json:"details,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError sends the uniform error envelope. code is a stable,
// machine-readable reason; message is for humans.
func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	writeErrorDetails(w, r, status, code, message, nil)
}

// writeErrorDetails is writeError plus specifics, e.g. which seats were taken.
func writeErrorDetails(w http.ResponseWriter, r *http.Request, status int, code, message string, details []string) {
	if st := stateFrom(r.Context()); st != nil {
		st.errCode = code
	}
	writeJSON(w, status, errorBody{
		Error:     errorDetail{Code: code, Message: message, Details: details},
		RequestID: requestIDFrom(r.Context()),
	})
}

// internalError records err on the request's access-log line (the single log
// line for this request) and returns an opaque 500 to the client.
// A database outage is reported as 503 database_unavailable with Retry-After,
// so clients back off and dashboards can tell a dependency outage from a bug.
func internalError(w http.ResponseWriter, r *http.Request, err error) {
	if st := stateFrom(r.Context()); st != nil {
		st.err = err
	}
	if store.IsUnavailable(err) {
		w.Header().Set("Retry-After", "2")
		writeError(w, r, http.StatusServiceUnavailable, "database_unavailable", "database unavailable, retry shortly")
		return
	}
	writeError(w, r, http.StatusInternalServerError, "internal", "internal error")
}

// decodeJSON reads one JSON object into dst. Unknown fields are ignored on
// purpose: a client that sends e.g. a user_id in the body is not rejected, the
// field simply has no effect, since identity only ever comes from the token.
// It returns false after writing a 400/413 if the body is unusable.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	err := json.NewDecoder(r.Body).Decode(dst)
	if err == nil {
		return true
	}
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		writeError(w, r, http.StatusRequestEntityTooLarge, "body_too_large", "request body too large")
	case errors.Is(err, io.EOF):
		writeError(w, r, http.StatusBadRequest, "invalid_body", "request body is empty")
	default:
		writeError(w, r, http.StatusBadRequest, "invalid_body", "request body is not valid JSON for this endpoint: "+err.Error())
	}
	return false
}
