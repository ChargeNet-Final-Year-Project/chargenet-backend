// Package auth implements the JWT and password helpers shared by the mobile
// API (aud="mobile") and the CPO portal (aud="portal").
package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Audiences understood by ParseToken.
const (
	AudMobile = "mobile"
	AudPortal = "portal"
)

// MobileTokenTTL is how long a mobile bearer token stays valid.
const MobileTokenTTL = 24 * time.Hour

// ErrInvalidToken is returned when a token is malformed, expired or signed
// with the wrong key/audience.
var ErrInvalidToken = errors.New("auth: invalid token")

// Claims is the payload embedded in every ChargeNet JWT.
type Claims struct {
	jwt.RegisteredClaims
	Role  string `json:"role,omitempty"`
	CPOID string `json:"cpoId,omitempty"`
}

// IssueMobileToken mints a HS256 token for a driver (`aud="mobile"`).
func IssueMobileToken(secret, userID string) (string, error) {
	return issue(secret, userID, AudMobile, "", "")
}

// IssuePortalToken mints a HS256 token for a portal user (`aud="portal"`).
func IssuePortalToken(secret, userID, role, cpoID string, ttl time.Duration) (string, error) {
	return issueWithTTL(secret, userID, AudPortal, role, cpoID, ttl)
}

func issue(secret, subject, audience, role, cpoID string) (string, error) {
	return issueWithTTL(secret, subject, audience, role, cpoID, MobileTokenTTL)
}

func issueWithTTL(secret, subject, audience, role, cpoID string, ttl time.Duration) (string, error) {
	now := time.Now().UTC()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   subject,
			Audience:  jwt.ClaimStrings{audience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
		Role:  role,
		CPOID: cpoID,
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
}

// ParseToken validates a token for the expected audience and returns its claims.
func ParseToken(secret, tokenString, audience string) (*Claims, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("auth: unexpected signing method %v", t.Header["alg"])
		}
		return []byte(secret), nil
	}, jwt.WithAudience(audience), jwt.WithExpirationRequired())
	if err != nil || !token.Valid {
		return nil, ErrInvalidToken
	}
	return claims, nil
}
