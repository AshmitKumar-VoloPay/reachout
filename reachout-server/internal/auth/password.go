// Package auth handles users, passwords and cookie sessions.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

const (
	MinPasswordLen = 10
	MaxPasswordLen = 128 // caps hashing work per request
)

// argonParams are variables so tests can lower the cost.
var argonParams = struct {
	Memory  uint32 // KiB
	Time    uint32
	Threads uint8
	KeyLen  uint32
	SaltLen int
}{Memory: 64 * 1024, Time: 2, Threads: 2, KeyLen: 32, SaltLen: 16}

var errBadHash = errors.New("auth: malformed password hash")

// HashPassword returns an argon2id hash in the standard encoded form:
// $argon2id$v=19$m=65536,t=2,p=2$<salt>$<key>
func HashPassword(password string) (string, error) {
	p := argonParams
	salt := make([]byte, p.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Time, p.Threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword checks password against an encoded hash in constant time.
// It uses the parameters stored in the hash, so old hashes keep verifying after a cost change.
func VerifyPassword(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errBadHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, errBadHash
	}
	var mem, t uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &t, &threads); err != nil {
		return false, errBadHash
	}
	// Refuse absurd parameters from a corrupted row rather than allocating them.
	if mem == 0 || mem > 1<<20 || t == 0 || t > 10 || threads == 0 {
		return false, errBadHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, errBadHash
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false, errBadHash
	}
	got := argon2.IDKey([]byte(password), salt, t, mem, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

var (
	dummyOnce sync.Once
	dummyHash string
)

// burnPassword spends the same time as a real verification. Login calls it when the
// email is unknown so response time does not reveal which emails exist.
func burnPassword(password string) {
	dummyOnce.Do(func() { dummyHash, _ = HashPassword("not-a-real-password") })
	VerifyPassword(password, dummyHash)
}
