package protocol

import (
	"crypto/sha256"
	"crypto/subtle"
)

// TokenEqual reports whether two shared-secret tokens match, in constant
// time. Both inputs are hashed with SHA-256 first and the fixed-size
// digests compared with subtle.ConstantTimeCompare: comparing the raw
// strings would let ConstantTimeCompare short-circuit on unequal lengths
// and leak the token's length. Never log a token; log the first 8 hex
// characters of its SHA-256 for correlation instead.
func TokenEqual(a, b string) bool {
	ha := sha256.Sum256([]byte(a))
	hb := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}
