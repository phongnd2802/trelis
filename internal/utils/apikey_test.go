package utils

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestGeneratedAPIKeysAreTrelisKeysAndDistinct(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		key := GenerateAPIKey()
		if !strings.HasPrefix(key, "trelis_") {
			t.Fatalf("key %q lacks the trelis_ prefix", key)
		}
		// 32 random bytes, unpadded base64url: 43 characters after the prefix.
		if n := len(key) - len("trelis_"); n != 43 {
			t.Fatalf("key %q has %d characters after the prefix, want 43", key, n)
		}
		if seen[key] {
			t.Fatalf("key %q generated twice", key)
		}
		seen[key] = true
	}
}

func TestHashAPIKeyIsTheSHA256OfTheKey(t *testing.T) {
	// echo -n trelis_abc | sha256sum
	const want = "95aa168901a3100fbc28ef9cf430611213d4013fc109fa02601a107e9d3f1576"
	if got := hex.EncodeToString(HashAPIKey("trelis_abc")); got != want {
		t.Errorf("HashAPIKey(trelis_abc) = %s, want %s", got, want)
	}
}
