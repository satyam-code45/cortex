package keys_test

import (
	"bytes"
	"strings"
	"testing"

	"cortex/internal/keys"
)

// Encryption at rest for stored keys.
//
// The stored blob must never be the plaintext key, must round-trip back to it
// under the same secret, and must be useless under any other secret: rotation
// means re-adding keys, not silently decrypting garbage.

const (
	testSecretHex  = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	otherSecretHex = "ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100"
)

func TestCipherRoundTripsWithoutStoringPlaintext(t *testing.T) {
	t.Parallel()

	cipher, err := keys.NewCipher(testSecretHex)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}

	tests := []struct {
		name string
		key  string
	}{
		{name: "an OpenAI-shaped key", key: "sk-proj-abcdef0123456789ULTRAsecret"},
		{name: "a short key", key: "k"},
		{name: "a key with unicode", key: "sk-clé-ключ-鍵"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			blob, err := cipher.Encrypt(tt.key)
			if err != nil {
				t.Fatalf("Encrypt: %v", err)
			}
			// The ciphertext is what lands in user_llm_keys.key_ciphertext; a
			// database dump must not contain the key. Contains is only a sound
			// assertion for plaintexts long enough that a chance match in
			// effectively-random bytes is negligible — for a 1-byte key, any
			// correct cipher's output "contains" it ~10% of the time (28
			// non-nonce bytes × 1/256), which made this test flaky as written.
			if len(tt.key) >= 8 && bytes.Contains(blob, []byte(tt.key)) {
				t.Errorf("ciphertext contains the plaintext key")
			}
			if string(blob) == tt.key {
				t.Errorf("ciphertext equals the plaintext key")
			}

			got, err := cipher.Decrypt(blob)
			if err != nil {
				t.Fatalf("Decrypt: %v", err)
			}
			if got != tt.key {
				t.Errorf("Decrypt = %q, want %q", got, tt.key)
			}
		})
	}
}

// Two encryptions of the same key must not produce the same blob — a repeated
// nonce under GCM breaks the whole scheme, and identical blobs would also let
// a database reader spot two users sharing one key.
func TestCipherUsesAFreshNoncePerCall(t *testing.T) {
	t.Parallel()

	cipher, err := keys.NewCipher(testSecretHex)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	first, err := cipher.Encrypt("sk-same-key")
	if err != nil {
		t.Fatalf("Encrypt #1: %v", err)
	}
	second, err := cipher.Encrypt("sk-same-key")
	if err != nil {
		t.Fatalf("Encrypt #2: %v", err)
	}
	if bytes.Equal(first, second) {
		t.Error("two encryptions of the same key produced identical ciphertexts")
	}
}

// A rotated secret must fail loudly (the stored key is unusable and must be
// re-added), never return wrong plaintext.
func TestCipherRejectsForeignAndCorruptBlobs(t *testing.T) {
	t.Parallel()

	cipher, err := keys.NewCipher(testSecretHex)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	other, err := keys.NewCipher(otherSecretHex)
	if err != nil {
		t.Fatalf("NewCipher(other): %v", err)
	}

	blob, err := cipher.Encrypt("sk-rotate-me")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	if _, err := other.Decrypt(blob); err == nil {
		t.Error("Decrypt under a different secret succeeded, want an error")
	}

	corrupted := append([]byte(nil), blob...)
	corrupted[len(corrupted)-1] ^= 0xff
	if _, err := cipher.Decrypt(corrupted); err == nil {
		t.Error("Decrypt of a corrupted blob succeeded, want an error")
	}

	if _, err := cipher.Decrypt([]byte{0x01}); err == nil {
		t.Error("Decrypt of a blob shorter than a nonce succeeded, want an error")
	}
}

// LLM_KEY_ENCRYPTION_SECRET's format (exactly 64 hex chars = 32 bytes) is
// enforced here as well as in config.Load, so the package is safe on its own.
func TestNewCipherRejectsMalformedSecrets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		secret string
	}{
		{name: "empty", secret: ""},
		{name: "not hex", secret: strings.Repeat("zz", 32)},
		{name: "too short", secret: "00112233445566778899aabbccddeeff"},
		{name: "too long", secret: testSecretHex + "00"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := keys.NewCipher(tt.secret); err == nil {
				t.Errorf("NewCipher(%q) succeeded, want an error", tt.secret)
			}
		})
	}
}
