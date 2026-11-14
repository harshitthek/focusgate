// Package auth provides cookie session signing and token validation.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// DefaultIterations is the number of PBKDF2-HMAC-SHA256 iterations.
	DefaultIterations = 100000
	// KeyLen is the derived key length in bytes.
	KeyLen = 32
	// SaltLen is the cryptographic salt length in bytes.
	SaltLen = 32
	// SessionTokenLen is the byte length of random session tokens.
	SessionTokenLen = 32
	// DefaultSessionTTL defines standard session persistence (30 days).
	DefaultSessionTTL = 30 * 24 * time.Hour
)

// DeriveKey derives a cryptographic key using PBKDF2-HMAC-SHA256 (RFC 2898 / RFC 8018).
func DeriveKey(passphrase string, salt []byte, iterations, keyLen int) []byte {
	if iterations <= 0 {
		iterations = DefaultIterations
	}
	if keyLen <= 0 {
		keyLen = KeyLen
	}

	prf := func(key, data []byte) []byte {
		h := hmac.New(sha256.New, key)
		h.Write(data)
		return h.Sum(nil)
	}

	passBytes := []byte(passphrase)
	hashLen := sha256.Size
	numBlocks := (keyLen + hashLen - 1) / hashLen
	out := make([]byte, 0, numBlocks*hashLen)

	for block := 1; block <= numBlocks; block++ {
		var blockBuf [4]byte
		binary.BigEndian.PutUint32(blockBuf[:], uint32(block))

		saltBlock := append(append([]byte(nil), salt...), blockBuf[:]...)
		u := prf(passBytes, saltBlock)
		t := append([]byte(nil), u...)

		for iter := 1; iter < iterations; iter++ {
			u = prf(passBytes, u)
			for i := range t {
				t[i] ^= u[i]
			}
		}
		out = append(out, t...)
	}

	return out[:keyLen]
}

// HashPassphrase generates a cryptographically secure random salt and derives the PBKDF2-SHA256 hash.
// Both are returned as base64-encoded strings.
func HashPassphrase(passphrase string) (hashB64, saltB64 string, err error) {
	if strings.TrimSpace(passphrase) == "" {
		return "", "", errors.New("passphrase cannot be empty")
	}

	salt := make([]byte, SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", "", fmt.Errorf("generate salt: %w", err)
	}

	key := DeriveKey(passphrase, salt, DefaultIterations, KeyLen)
	return base64.StdEncoding.EncodeToString(key), base64.StdEncoding.EncodeToString(salt), nil
}

// VerifyPassphrase compares a plaintext passphrase against an expected base64 hash and salt
// using constant-time comparison to prevent timing side-channel attacks.
func VerifyPassphrase(passphrase, expectedHashB64, saltB64 string) bool {
	if strings.TrimSpace(passphrase) == "" || expectedHashB64 == "" || saltB64 == "" {
		return false
	}

	salt, err := base64.StdEncoding.DecodeString(saltB64)
	if err != nil || len(salt) == 0 {
		return false
	}

	expectedHash, err := base64.StdEncoding.DecodeString(expectedHashB64)
	if err != nil || len(expectedHash) == 0 {
		return false
	}

	computed := DeriveKey(passphrase, salt, DefaultIterations, len(expectedHash))
	return subtle.ConstantTimeCompare(computed, expectedHash) == 1
}

// GenerateSessionToken returns a cryptographically secure hex-encoded random session token.
func GenerateSessionToken() (string, error) {
	b := make([]byte, SessionTokenLen)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	return hex.EncodeToString(b), nil
}
