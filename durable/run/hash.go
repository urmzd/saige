package run

import (
	"crypto/sha256"
	"encoding/hex"
)

// HashInput returns the sha256 hex digest of a marshaled input payload.
// Recorded once at first execution and verified on every replay hit; a
// mismatch is ErrNonDeterministic. Steps and events pass "" instead — their
// payload is the non-determinism being captured, not an input to verify.
func HashInput(input []byte) string {
	sum := sha256.Sum256(input)
	return hex.EncodeToString(sum[:])
}
