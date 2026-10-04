package httpapi

import (
	"log/slog"
	"net/http"

	"seat-reservation/internal/auth"
	"seat-reservation/internal/store"
)

type API struct {
	store *store.Store
	auth  *auth.Authenticator
	ready func() bool
}

func New(st *store.Store, a *auth.Authenticator, ready func() bool) *API {
	return &API{store: st, auth: a, ready: ready}
}

// Handler wires routes and the middleware chain. Outermost first:
// request ID -> access log -> panic recovery -> body limit -> routes.
func (a *API) Handler(log *slog.Logger) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /livez", a.livez)
	mux.HandleFunc("GET /readyz", a.readyz)

	api := http.NewServeMux()
	api.HandleFunc("POST /tokens", a.mintToken)
	api.HandleFunc("POST /shows", a.requireAdmin(a.createShow))
	api.HandleFunc("GET /shows/{id}", a.getShow)
	mux.Handle("/", requireReady(a.ready, api))

	var h http.Handler = mux
	h = limitBody(h)
	h = recoverPanics(h)
	h = accessLog(log)(h)
	h = withRequestState(h)
	return h
}
