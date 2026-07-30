package storage

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
)

// canonicalAdvisoryLockIdentity returns a deterministic, collision-safe and
// NUL-free identity for PostgreSQL advisory locks. Length-prefixing each
// component keeps distinct part boundaries distinct before hashing, while the
// explicit domain prevents unrelated lock namespaces from sharing identities.
func canonicalAdvisoryLockIdentity(domain string, parts ...string) string {
	h := sha256.New()
	var length [8]byte
	writePart := func(part string) {
		value := []byte(part)
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		_, _ = h.Write(length[:])
		_, _ = h.Write(value)
	}
	writePart(domain)
	for _, part := range parts {
		writePart(part)
	}
	return hex.EncodeToString(h.Sum(nil))
}
