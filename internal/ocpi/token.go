package ocpi

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// tokenBytes is the entropy of Token C: 32 random bytes rendered as hex, i.e. a
// 64-character string.
const tokenBytes = 32

// NewToken mints the Token C ChargeNet hands a CPO after a successful
// `POST /credentials`.
func NewToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("ocpi: generate token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// tokenCandidates lists the strings a presented token could be, best guess
// first.
//
// OCPI leaves the token encoding to the implementation: most send it verbatim,
// some base64-encode it. The
// verbatim form always comes first, so a raw token that happens to be valid
// The decoded form is considered only when the raw token matches no CPO and the
// decoded bytes are printable ASCII.
func tokenCandidates(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	out := []string{raw}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawURLEncoding} {
		decoded, err := enc.DecodeString(raw)
		if err != nil || len(decoded) == 0 || !printable(decoded) {
			continue
		}
		s := string(decoded)
		if s != raw {
			out = append(out, s)
		}
	}
	return out
}

// printable reports whether every byte is visible ASCII, which is what all
// ChargeNet tokens are made of.
func printable(b []byte) bool {
	for _, c := range b {
		if c < 0x20 || c > 0x7E {
			return false
		}
	}
	return true
}
