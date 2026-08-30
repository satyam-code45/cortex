// Package keys stores users' LLM API keys: AES-256-GCM at rest, decrypted
// per request, plaintext never logged and never returned by any endpoint.
package keys

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
)

// Cipher seals and opens key material under one AES-256-GCM key.
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher builds a Cipher from LLM_KEY_ENCRYPTION_SECRET's format: exactly
// 64 hex characters (32 bytes). config.Load validates the format at startup;
// this re-check keeps the package safe to use on its own.
func NewCipher(hexSecret string) (*Cipher, error) {
	raw, err := hex.DecodeString(hexSecret)
	if err != nil {
		return nil, fmt.Errorf("keys: encryption secret is not hex: %w", err)
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("keys: encryption secret is %d bytes, want 32", len(raw))
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, fmt.Errorf("keys: build cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("keys: build GCM: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// Encrypt seals plaintext as nonce‖ciphertext. A fresh random nonce per call
// is what makes storing many keys under one secret safe.
func (c *Cipher) Encrypt(plaintext string) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("keys: generate nonce: %w", err)
	}
	return c.aead.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

// Decrypt opens a blob produced by Encrypt.
func (c *Cipher) Decrypt(blob []byte) (string, error) {
	if len(blob) < c.aead.NonceSize() {
		return "", errors.New("keys: ciphertext shorter than a nonce")
	}
	nonce, ciphertext := blob[:c.aead.NonceSize()], blob[c.aead.NonceSize():]
	plaintext, err := c.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		// GCM authentication failure: the blob was encrypted under a different
		// secret (rotation) or corrupted. The distinction does not matter to
		// the caller — the stored key is unusable and must be re-added.
		return "", fmt.Errorf("keys: decrypt stored key: %w", err)
	}
	return string(plaintext), nil
}
