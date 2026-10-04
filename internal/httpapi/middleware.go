package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"seat-reservation/internal/auth"
	"seat-reservation/internal/booking"
	"seat-reservation/internal/metrics"
)

const (
	requestIDHeader = "X-Request-ID"
	maxBodyBytes    = 64 << 10
)

var incomingIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// requestState is shared by every layer handling one request, so that the
// access log, written once at the end, can carry what inner layers learned
// (who the user was, why it failed, whether it panicked).
type requestState struct {
	id      string
	user    string
	outcome string // booking outcome, for reserve/cancel
	errCode string
	err     error
	panic   any
	stack   string
}

type stateKey struct{}

func stateFrom(ctx context.Context) *requestState {
	st, _ := ctx.Value(stateKey{}).(*requestState)
	return st
}

func requestIDFrom(ctx context.Context) string {
	if st := stateFrom(ctx); st != nil {
		return st.id
	}
	return ""
}

// withRequestState assigns the request ID (reusing a well-formed incoming
// X-Request-ID so IDs correlate across hops) and echoes it on the response.
func withRequestState(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if !incomingIDPattern.MatchString(id) {
			id = newRequestID()
		}
		w.Header().Set(requestIDHeader, id)
		st := &requestState{id: id}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), stateKey{}, st)))
	})
}

func newRequestID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// observe writes exactly one log line and records exactly one set of metrics
// per request, after it completes. Nothing else in the request path logs or
// counts; inner layers only annotate requestState.
func observe(log *slog.Logger, m *metrics.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			m.RequestStarted()
			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			m.RequestFinished()
			elapsed := time.Since(start)

			st := stateFrom(r.Context())
			status := rec.status
			if status == 0 {
				status = http.StatusOK
			}
			recordMetrics(m, r, st, status, elapsed)
			attrs := []slog.Attr{
				slog.String("request_id", st.id),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", status),
				slog.Int("bytes", rec.bytes),
				slog.Float64("duration_ms", float64(elapsed.Microseconds())/1000),
				slog.String("remote", r.RemoteAddr),
			}
			if st.user != "" {
				attrs = append(attrs, slog.String("user_id", st.user))
			}
			if st.outcome != "" {
				attrs = append(attrs, slog.String("outcome", st.outcome))
			}
			if st.errCode != "" {
				attrs = append(attrs, slog.String("error_code", st.errCode))
			}
			if st.err != nil {
				attrs = append(attrs, slog.String("error", st.err.Error()))
			}
			level := slog.LevelInfo
			if st.panic != nil {
				level = slog.LevelError
				attrs = append(attrs, slog.String("panic", fmt.Sprint(st.panic)), slog.String("stack", st.stack))
			} else if status >= 500 && r.URL.Path == "/readyz" {
				level = slog.LevelWarn // an expected answer while starting or draining
			} else if status >= 500 {
				level = slog.LevelError
			}
			log.LogAttrs(r.Context(), level, "request", attrs...)
		})
	}
}

var (
	reservePath = regexp.MustCompile(`^/shows/[^/]+/reservations$`)
	cancelPath  = regexp.MustCompile(`^/reservations/[^/]+$`)
)

// recordMetrics is the only place outcome counters are incremented, so every
// reserve response lands in exactly one of reservations_confirmed_total or
// reservations_declined_total{reason}, including auth failures, bad input,
// not-ready and panics, not just the database's decisions.
func recordMetrics(m *metrics.Metrics, r *http.Request, st *requestState, status int, elapsed time.Duration) {
	// r.Pattern is set by the inner mux on this same request; it is a
	// bounded set of route templates, never a raw path.
	route := r.Pattern
	if route == "" || route == "/" {
		route = "unmatched"
	}
	m.ObserveHTTP(route, strconv.Itoa(status), elapsed.Seconds())

	switch {
	case r.Method == http.MethodPost && reservePath.MatchString(r.URL.Path):
		m.ObserveReserve(reasonFor(st, status))
	case r.Method == http.MethodDelete && cancelPath.MatchString(r.URL.Path):
		m.ObserveCancel(reasonFor(st, status))
	}
}

// reasonFor prefers the booking outcome's reason; failures that happened
// before a decision (auth, validation, not ready) use their error code.
func reasonFor(st *requestState, status int) string {
	if st.panic != nil {
		return "internal-error"
	}
	if st.outcome != "" {
		if spec, ok := booking.Outcome(st.outcome).Spec(); ok {
			return spec.Reason
		}
	}
	if st.errCode != "" {
		return strings.ReplaceAll(st.errCode, "_", "-")
	}
	if status >= 500 {
		return "internal-error"
	}
	return "unknown"
}

// recoverPanics turns a handler panic into a 500 and records it on the access
// log line instead of crashing the process or logging a second line.
func recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			p := recover()
			if p == nil {
				return
			}
			if p == http.ErrAbortHandler {
				panic(p)
			}
			if st := stateFrom(r.Context()); st != nil {
				st.panic = p
				st.stack = string(debug.Stack())
			}
			if rec, ok := w.(*statusRecorder); !ok || rec.status == 0 {
				writeError(w, r, http.StatusInternalServerError, "internal", "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		next.ServeHTTP(w, r)
	})
}

// requireReady rejects API calls with 503 until the database is reachable and
// migrated, so nothing runs against a schema that isn't there yet.
func requireReady(ready func() bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ready() {
			w.Header().Set("Retry-After", "2")
			writeError(w, r, http.StatusServiceUnavailable, "not_ready", "service is starting up")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// setUser records the authenticated user both in the request context (for
// handlers) and on the request state (for the access log line).
func setUser(r *http.Request, userID string) *http.Request {
	if st := stateFrom(r.Context()); st != nil {
		st.user = userID
	}
	return r.WithContext(auth.WithUser(r.Context(), userID))
}
