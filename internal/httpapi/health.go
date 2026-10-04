package httpapi

import (
	"context"
	"net/http"
	"time"
)

// livez says the process is up and serving HTTP. It deliberately checks no
// dependencies, so a database outage doesn't get the container restarted.
func (a *API) livez(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz says the service should receive traffic right now: not shutting
// down, migrations finished, and the database answers a ping. Anything else
// is 503 (fail closed).
func (a *API) readyz(w http.ResponseWriter, r *http.Request) {
	if a.draining() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "draining"})
		return
	}
	if !a.migrated() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "starting", "database": "not_migrated"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := a.store.Ping(ctx); err != nil {
		if st := stateFrom(r.Context()); st != nil {
			st.err = err
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "database": "unreachable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "database": "ok"})
}
