package httpapi

import (
	"net/http"
	"strings"
	"time"
)

const adminKeyHeader = "X-Admin-Key"

type mintRequest struct {
	UserID string `json:"user_id"`
}

type mintResponse struct {
	Token     string    `json:"token"`
	UserID    string    `json:"user_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// mintToken stands in for an identity provider: it issues a signed token for
// the requested user ID. It is open so load tests can create many users; in
// production this would sit behind real login. What matters for correctness is
// that every other endpoint trusts only the verified token, never the body.
func (a *API) mintToken(w http.ResponseWriter, r *http.Request) {
	var req mintRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	token, exp, err := a.auth.Mint(req.UserID)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_user_id", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, mintResponse{Token: token, UserID: req.UserID, ExpiresAt: exp})
}

// requireUser admits only requests with a valid bearer token and puts the
// token's subject in the context as the caller's identity.
func (a *API) requireUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, ok := bearerToken(r)
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "missing bearer token")
			return
		}
		userID, err := a.auth.Verify(raw)
		if err != nil {
			writeError(w, r, http.StatusUnauthorized, "invalid_token", "token is invalid or expired")
			return
		}
		next(w, setUser(r, userID))
	}
}

// requireAdmin admits only requests carrying the admin key. A user token is
// never enough: presenting one without the key is 403, nothing at all is 401.
func (a *API) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.auth.IsAdminKey(r.Header.Get(adminKeyHeader)) {
			next(w, r)
			return
		}
		if raw, ok := bearerToken(r); ok {
			if _, err := a.auth.Verify(raw); err == nil {
				writeError(w, r, http.StatusForbidden, "forbidden", "admin key required; user tokens cannot call admin routes")
				return
			}
		}
		writeError(w, r, http.StatusUnauthorized, "admin_key_required", "missing or invalid "+adminKeyHeader)
	}
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	scheme, token, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", false
	}
	return strings.TrimSpace(token), true
}
