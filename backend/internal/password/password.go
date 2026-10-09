// Package password implements PBKDF2-HMAC-SHA256 password hashing, using the
// same stored format as the previous Cloudflare implementation so an existing
// hash keeps working:
//
//	pbkdf2$<iterations>$<base64url salt>$<base64url derivedKey>
package password

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

const (
	// DefaultIterations follows current OWASP guidance for PBKDF2-HMAC-SHA256.
	//
	// The previous Cloudflare deployment was capped at 100 000 because the
	// Workers runtime rejects anything higher. That constraint does not exist
	// on our own server, so the default is raised; Verify still accepts the
	// lower iteration count of existing hashes.
	DefaultIterations = 600_000
	keyLength         = 32
	saltLength        = 16
)

// Hash derives a new password hash with a random salt.
func Hash(plaintext string, iterations int) (string, error) {
	if iterations <= 0 {
		iterations = DefaultIterations
	}
	salt := make([]byte, saltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	key, err := pbkdf2.Key(sha256.New, plaintext, salt, iterations, keyLength)
	if err != nil {
		return "", fmt.Errorf("derive key: %w", err)
	}
	return strings.Join([]string{
		"pbkdf2",
		strconv.Itoa(iterations),
		base64.RawURLEncoding.EncodeToString(salt),
		base64.RawURLEncoding.EncodeToString(key),
	}, "$"), nil
}

// Verify reports whether plaintext matches the stored hash.
//
// It returns false (never an error) for a malformed hash, so a corrupt
// configuration cannot be distinguished from a wrong password by an attacker.
func Verify(plaintext, stored string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2" {
		return false
	}
	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations <= 0 {
		return false
	}
	salt, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	expected, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	actual, err := pbkdf2.Key(sha256.New, plaintext, salt, iterations, len(expected))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

// IterationsOf reports the iteration count encoded in a stored hash (0 when
// the hash is malformed). Used by the startup self-check.
func IterationsOf(stored string) int {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2" {
		return 0
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0
	}
	return n
}
