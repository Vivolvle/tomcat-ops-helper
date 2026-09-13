package actions

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
)

// newHash returns a fresh SHA-256 hasher. Indirected so future tests can swap
// the algorithm without touching probe code.
func newHash() hash.Hash { return sha256.New() }

// hexOf is a tiny helper used in places where we want hex output.
func hexOf(h hash.Hash) string { return hex.EncodeToString(h.Sum(nil)) }
