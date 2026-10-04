package booking

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func mustReq(t *testing.T, show int64, key string, seats ...string) ReserveRequest {
	t.Helper()
	r, err := NewReserveRequest(show, "alice", key, seats)
	if err != nil {
		t.Fatalf("NewReserveRequest(%v): %v", seats, err)
	}
	return r
}

func TestFingerprintIgnoresSeatOrderAndCase(t *testing.T) {
	a := mustReq(t, 1, "k", "A1", "B2", "A10")
	b := mustReq(t, 1, "k", "A10", "a1", " B2 ")
	if a.Fingerprint() != b.Fingerprint() {
		t.Fatal("same seats in a different order/case gave different fingerprints")
	}
	if !slices.Equal(a.Seats, b.Seats) {
		t.Fatalf("canonical seats differ: %v vs %v", a.Seats, b.Seats)
	}
}

func TestFingerprintDistinguishesRequests(t *testing.T) {
	base := mustReq(t, 1, "k", "A1", "A2").Fingerprint()
	for name, other := range map[string]ReserveRequest{
		"different seat":  mustReq(t, 1, "k", "A1", "A3"),
		"subset":          mustReq(t, 1, "k", "A1"),
		"superset":        mustReq(t, 1, "k", "A1", "A2", "A3"),
		"different show":  mustReq(t, 2, "k", "A1", "A2"),
		"join ambiguity":  mustReq(t, 1, "k", "A1", "A21"),
		"show id overlap": mustReq(t, 11, "k", "A1", "A2"),
	} {
		if other.Fingerprint() == base {
			t.Errorf("%s: fingerprint collided with base request", name)
		}
	}
	// The key is not part of the fingerprint: it's the lookup, not the content.
	if mustReq(t, 1, "other-key", "A1", "A2").Fingerprint() != base {
		t.Error("fingerprint should not depend on the idempotency key")
	}
}

func TestRejects(t *testing.T) {
	tooMany := make([]string, MaxSeatsPerRequest+1)
	for i := range tooMany {
		tooMany[i] = "A" + string(rune('1'+i%9)) + strings.Repeat("0", i/9)
	}
	cases := []struct {
		name  string
		key   string
		seats []string
		want  error
	}{
		{"missing key", "", []string{"A1"}, ErrMissingKey},
		{"key with space", "has space", []string{"A1"}, ErrInvalidKey},
		{"key too long", strings.Repeat("k", 129), []string{"A1"}, ErrInvalidKey},
		{"nil seats", "k", nil, ErrNoSeats},
		{"empty seats", "k", []string{}, ErrNoSeats},
		{"duplicate seat", "k", []string{"A1", "A2", "A1"}, ErrDuplicateSeat},
		{"duplicate after normalising", "k", []string{"a1", "A1 "}, ErrDuplicateSeat},
		{"too many seats", "k", tooMany, ErrTooManySeats},
		{"empty label", "k", []string{""}, ErrInvalidSeat},
		{"bad label", "k", []string{"1A"}, ErrInvalidSeat},
		{"seat zero", "k", []string{"A0"}, ErrInvalidSeat},
		{"injection", "k", []string{"A1'; DROP TABLE seats;--"}, ErrInvalidSeat},
	}
	for _, c := range cases {
		_, err := NewReserveRequest(1, "alice", c.key, c.seats)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
		if err != nil && ErrorCode(err) == "invalid_request" {
			t.Errorf("%s: no specific error code for %v", c.name, err)
		}
	}
}

func TestOutcomeTableComplete(t *testing.T) {
	for _, o := range AllOutcomes {
		s, ok := o.Spec()
		if !ok {
			t.Errorf("%s: missing from outcome table", o)
			continue
		}
		if s.Status < 200 || s.Status >= 500 {
			t.Errorf("%s: status %d; every outcome must be 2xx or 4xx", o, s.Status)
		}
		if s.OK != (s.Status < 300) {
			t.Errorf("%s: OK=%v disagrees with status %d", o, s.OK, s.Status)
		}
		if !s.OK && (s.Code == "" || s.Message == "") {
			t.Errorf("%s: a decline needs a code and message", o)
		}
	}
	if len(outcomes) != len(AllOutcomes) {
		t.Errorf("table has %d entries, AllOutcomes has %d", len(outcomes), len(AllOutcomes))
	}
	if _, ok := Outcome("bogus").Spec(); ok {
		t.Error("unknown outcome should not resolve")
	}
}
