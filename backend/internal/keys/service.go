package keys

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"cortex/internal/store"
)

// ErrNoKey is returned when a user has no stored LLM key.
var ErrNoKey = errors.New("keys: no llm key on file")

// ErrUnusableKey is returned when a stored key exists but cannot be decrypted
// (the encryption secret rotated, or the blob is corrupt). Like ErrNoKey it
// does not heal on retry — the user must re-add their key — which is what
// separates both from a transient database failure.
var ErrUnusableKey = errors.New("keys: stored llm key is unusable")

// Service is the one path to the user_llm_keys table. Everything above it
// handles either ciphertext or short-lived plaintext; nothing else touches
// the cipher.
type Service struct {
	db     store.DBTX
	cipher *Cipher
}

// NewService builds a Service.
func NewService(db store.DBTX, cipher *Cipher) *Service {
	return &Service{db: db, cipher: cipher}
}

// Save encrypts and stores a user's key, returning the last4 for display.
// The caller has already validated the key against the provider.
func (s *Service) Save(ctx context.Context, userID uuid.UUID, provider, apiKey string) (last4 string, err error) {
	ciphertext, err := s.cipher.Encrypt(apiKey)
	if err != nil {
		return "", err
	}
	last4 = apiKey
	if len(last4) > 4 {
		last4 = last4[len(last4)-4:]
	}
	_, err = store.New(s.db).UpsertUserLLMKey(ctx, store.UpsertUserLLMKeyParams{
		UserID:        userID,
		Provider:      provider,
		KeyCiphertext: ciphertext,
		KeyLast4:      last4,
	})
	if err != nil {
		return "", fmt.Errorf("keys: store key: %w", err)
	}
	return last4, nil
}

// Get decrypts a user's stored key. The plaintext exists only in the caller's
// memory for the duration of one request or run: decrypt → use → discard.
func (s *Service) Get(ctx context.Context, userID uuid.UUID) (provider, apiKey string, err error) {
	row, err := store.New(s.db).GetUserLLMKey(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNoKey
	}
	if err != nil {
		return "", "", fmt.Errorf("keys: load key: %w", err)
	}
	apiKey, err = s.cipher.Decrypt(row.KeyCiphertext)
	if err != nil {
		return "", "", fmt.Errorf("%w: %w", ErrUnusableKey, err)
	}
	return row.Provider, apiKey, nil
}

// Info reports what the API may show about a stored key: provider and last4,
// never the key. ErrNoKey when none is stored.
func (s *Service) Info(ctx context.Context, userID uuid.UUID) (provider, last4 string, err error) {
	row, err := store.New(s.db).GetUserLLMKey(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNoKey
	}
	if err != nil {
		return "", "", fmt.Errorf("keys: load key info: %w", err)
	}
	return row.Provider, row.KeyLast4, nil
}

// Delete removes a user's stored key. ErrNoKey when none was stored.
func (s *Service) Delete(ctx context.Context, userID uuid.UUID) error {
	rows, err := store.New(s.db).DeleteUserLLMKey(ctx, userID)
	if err != nil {
		return fmt.Errorf("keys: delete key: %w", err)
	}
	if rows == 0 {
		return ErrNoKey
	}
	return nil
}
