package store

import (
	"crypto/rand"
	"encoding/hex"
)

// NewID returns a prefixed, random identifier such as "usr_9f3a…".
// 12 random bytes (24 hex chars) mirror the convention in the API contract
func NewID(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand never fails in practice; fall back to a panic so we
		// never silently emit a predictable id.
		panic("store: crypto/rand failed: " + err.Error())
	}
	return prefix + "_" + hex.EncodeToString(b)
}
