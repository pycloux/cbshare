// Package crypto provides key derivation, key encoding, password hashing and
// random PIN generation for cpsync.
//
// Every random value comes from crypto/rand, and every secret comparison
// is constant-time, so that response timing reveals nothing about how close
// a guess was.
package crypto

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

// KeySize is the size in bytes of the database encryption key.
const KeySize = 32

// Argon2id parameters (RFC 9106 second recommended option).
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 4
)

var (
	// ErrInvalidKey is returned when an encoded key is malformed or mistyped.
	ErrInvalidKey = errors.New("invalid key: check that it was copied correctly")

	keyEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)
)

// DeriveKey derives a new database key from the admin password, mixed with a
// random salt and the current time, so that every first start produces a
// different key even with the same password.
//
// The salt is not stored anywhere, so the key cannot be derived again: it
// is printed once and the operator keeps it. Knowing the password alone is
// therefore not enough to decrypt a stolen database file.
func DeriveKey(password string) ([]byte, error) {
	if password == "" {
		return nil, errors.New("password must not be empty")
	}
	salt := make([]byte, 16, 24)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("read random salt: %w", err)
	}
	salt = binary.BigEndian.AppendUint64(salt, uint64(time.Now().UnixNano()))
	return argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, KeySize), nil
}

// EncodeKey formats a key for display: base32 with a 2-byte checksum,
// grouped by 5 characters, e.g. "ABCDE-FGHIJ-...". Base32 avoids
// characters that are easy to confuse or that shells interpret, and the
// checksum lets DecodeKey tell a typo apart from a wrong key.
func EncodeKey(key []byte) string {
	sum := sha256.Sum256(key)
	s := keyEncoding.EncodeToString(append(append([]byte{}, key...), sum[0], sum[1]))
	var b strings.Builder
	for i := 0; i < len(s); i += 5 {
		if i > 0 {
			b.WriteByte('-')
		}
		b.WriteString(s[i:min(i+5, len(s))])
	}
	return b.String()
}

// DecodeKey parses a key produced by EncodeKey. Dashes, spaces and letter
// case are ignored; the checksum catches typos.
func DecodeKey(s string) ([]byte, error) {
	s = strings.ToUpper(strings.NewReplacer("-", "", " ", "", "\t", "").Replace(strings.TrimSpace(s)))
	raw, err := keyEncoding.DecodeString(s)
	if err != nil || len(raw) != KeySize+2 {
		return nil, ErrInvalidKey
	}
	key := raw[:KeySize]
	sum := sha256.Sum256(key)
	if raw[KeySize] != sum[0] || raw[KeySize+1] != sum[1] {
		return nil, ErrInvalidKey
	}
	return key, nil
}

// HashPassword returns an Argon2id hash in PHC string format.
func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("read random salt: %w", err)
	}
	hash := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, 32)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		b64.EncodeToString(salt), b64.EncodeToString(hash)), nil
}

// VerifyPassword reports whether password matches a hash from HashPassword.
func VerifyPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var version int
	var memory, iterations uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &threads); err != nil {
		return false
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, iterations, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// pinAlphabet avoids look-alike characters (0/O, 1/I/L).
const pinAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// GeneratePIN returns a random alphanumeric PIN of length n.
func GeneratePIN(n int) (string, error) {
	return randomString(n, pinAlphabet)
}

// RandomToken returns a URL-safe random token with 256 bits of entropy.
func RandomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// randomString picks n characters uniformly from alphabet (rand.Int avoids
// the bias of a simple modulo).
func randomString(n int, alphabet string) (string, error) {
	max := big.NewInt(int64(len(alphabet)))
	b := make([]byte, n)
	for i := range b {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b[i] = alphabet[v.Int64()]
	}
	return string(b), nil
}

// EqualFold compares two secrets in constant time, ignoring ASCII case.
func EqualFold(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(strings.ToUpper(a)), []byte(strings.ToUpper(b))) == 1
}

// Equal compares two secrets in constant time.
func Equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
