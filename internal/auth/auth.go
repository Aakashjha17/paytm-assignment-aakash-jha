package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const issuer = "seat-reservation"

var (
	ErrInvalidToken  = errors.New("invalid token")
	ErrInvalidUserID = errors.New("user_id must be 1-64 chars of [A-Za-z0-9_.:-]")
)

var userIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)

// Authenticator mints and verifies HS256 user tokens and checks the admin key.
// The user identity for every request comes only from a verified token's sub
// claim; nothing in a request body can change who the caller is.
type Authenticator struct {
	secret   []byte
	adminKey []byte
	ttl      time.Duration
	now      func() time.Time
}

func New(secret, adminKey string, ttl time.Duration) *Authenticator {
	return &Authenticator{secret: []byte(secret), adminKey: []byte(adminKey), ttl: ttl, now: time.Now}
}

func ValidUserID(id string) bool { return userIDPattern.MatchString(id) }

func (a *Authenticator) Mint(userID string) (token string, expiresAt time.Time, err error) {
	if !ValidUserID(userID) {
		return "", time.Time{}, ErrInvalidUserID
	}
	now := a.now()
	expiresAt = now.Add(a.ttl)
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Issuer:    issuer,
		Subject:   userID,
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(expiresAt),
	})
	token, err = t.SignedString(a.secret)
	return token, expiresAt, err
}

// Verify returns the user ID from a valid token. Only HS256 is accepted, which
// rules out alg=none and RS/HS key-confusion tricks; exp and iss are required.
func (a *Authenticator) Verify(token string) (string, error) {
	var claims jwt.RegisteredClaims
	_, err := jwt.ParseWithClaims(token, &claims,
		func(*jwt.Token) (any, error) { return a.secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(issuer),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(a.now),
	)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	if !ValidUserID(claims.Subject) {
		return "", fmt.Errorf("%w: bad subject", ErrInvalidToken)
	}
	return claims.Subject, nil
}

// IsAdminKey compares in constant time so the key can't be recovered by timing.
func (a *Authenticator) IsAdminKey(key string) bool {
	return key != "" && subtle.ConstantTimeCompare([]byte(key), a.adminKey) == 1
}

type ctxKey struct{}

func WithUser(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, ctxKey{}, userID)
}

// UserFrom returns the authenticated user, or "" if the request had none.
func UserFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}
