package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestMintVerifyRoundTrip(t *testing.T) {
	a := New("0123456789abcdef0123", "admin-key-0123456789", time.Hour)
	tok, _, err := a.Mint("alice")
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.Verify(tok)
	if err != nil || got != "alice" {
		t.Fatalf("Verify = %q, %v; want alice", got, err)
	}
}

func TestVerifyRejects(t *testing.T) {
	a := New("0123456789abcdef0123", "admin-key-0123456789", time.Hour)
	good, _, _ := a.Mint("alice")
	parts := strings.Split(good, ".")

	other := New("a-completely-different-secret", "x", time.Hour)
	foreign, _, _ := other.Mint("alice")

	expired := New("0123456789abcdef0123", "x", time.Hour)
	expired.now = func() time.Time { return time.Now().Add(-2 * time.Hour) }
	old, _, _ := expired.Mint("alice")

	// Payload swapped for one claiming to be bob, signature left as alice's.
	bobPayload := strings.Split(mustMint(t, a, "bob"), ".")[1]

	none := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.RegisteredClaims{
		Issuer: issuer, Subject: "alice", ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	})
	noneTok, _ := none.SignedString(jwt.UnsafeAllowNoneSignatureType)

	cases := map[string]string{
		"tampered payload":   parts[0] + "." + bobPayload + "." + parts[2],
		"tampered signature": parts[0] + "." + parts[1] + "." + strings.Repeat("A", len(parts[2])),
		"wrong secret":       foreign,
		"expired":            old,
		"alg none":           noneTok,
		"garbage":            "not.a.jwt",
		"empty":              "",
	}
	for name, tok := range cases {
		if _, err := a.Verify(tok); err == nil {
			t.Errorf("%s: Verify accepted it", name)
		}
	}
}

func TestAdminKey(t *testing.T) {
	a := New("0123456789abcdef0123", "admin-key-0123456789", time.Hour)
	if !a.IsAdminKey("admin-key-0123456789") {
		t.Error("correct key rejected")
	}
	for _, k := range []string{"", "admin-key-012345678", "admin-key-01234567890"} {
		if a.IsAdminKey(k) {
			t.Errorf("accepted %q", k)
		}
	}
}

func mustMint(t *testing.T, a *Authenticator, user string) string {
	t.Helper()
	tok, _, err := a.Mint(user)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}
