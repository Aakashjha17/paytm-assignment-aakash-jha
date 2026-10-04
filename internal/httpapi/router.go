package httpapi

import (
	"log/slog"
	"net/http"

	"seat-reservation/internal/auth"
	"seat-reservation/internal/metrics"
	"seat-reservation/internal/store"
)

type Deps struct {
	Store   *store.Store
	Auth    *auth.Authenticator
	Metrics *metrics.Metrics
	// Migrated reports that the schema is in place; until then the API is 503.
	Migrated func() bool
	// Draining reports that shutdown has begun: /readyz turns 503 so traffic
	// moves away, but requests that still arrive are served normally.
	Draining func() bool
}

type API struct {
	store    *store.Store
	auth     *auth.Authenticator
	metrics  *metrics.Metrics
	migrated func() bool
	draining func() bool
}

func New(d Deps) *API {
	if d.Draining == nil {
		d.Draining = func() bool { return false }
	}
	return &API{store: d.Store, auth: d.Auth, metrics: d.Metrics, migrated: d.Migrated, draining: d.Draining}
}

// Handler wires routes and the middleware chain. Outermost first:
// request ID -> log + metrics -> panic recovery -> body limit -> routes.
func (a *API) Handler(log *slog.Logger) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /livez", a.livez)
	mux.HandleFunc("GET /readyz", a.readyz)
	mux.Handle("GET /metrics", a.metrics.Handler())

	api := http.NewServeMux()
	api.HandleFunc("POST /tokens", a.mintToken)
	api.HandleFunc("POST /shows", a.requireAdmin(a.createShow))
	api.HandleFunc("GET /shows/{id}", a.getShow)
	api.HandleFunc("POST /shows/{id}/reservations", a.requireUser(a.reserve))
	api.HandleFunc("DELETE /reservations/{id}", a.requireUser(a.cancel))
	mux.Handle("/", requireReady(a.migrated, api))

	var h http.Handler = mux
	h = limitBody(h)
	h = recoverPanics(h)
	h = observe(log, a.metrics)(h)
	h = withRequestState(h)
	return h
}
