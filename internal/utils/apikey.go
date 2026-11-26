package utils

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

// APIKeyPrefix marks Trelis API keys so they are recognizable in config
// files and secret scanners.
const APIKeyPrefix = "trelis_"

// GenerateAPIKey returns a new API key: the prefix and 256 random bits.
func GenerateAPIKey() string {
	secret := make([]byte, 32)
	_, _ = rand.Read(secret) // crypto/rand.Read never returns an error
	return APIKeyPrefix + base64.RawURLEncoding.EncodeToString(secret)
}

// HashAPIKey is what is stored for a key. Keys carry 256 random bits, so a
// fast unsalted hash is enough: there is nothing to brute-force.
func HashAPIKey(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}
