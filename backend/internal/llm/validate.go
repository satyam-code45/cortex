package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// KeyValidationError means the provider rejected the key itself. Reason is
// provider text that is safe to show the user (it describes their key, never
// ours) — the settings endpoint surfaces it in the 422 body.
type KeyValidationError struct {
	Reason string
}

func (e *KeyValidationError) Error() string {
	return fmt.Sprintf("llm: key rejected by provider: %s", e.Reason)
}

// ValidateOpenAIKey checks a user-supplied key with the cheapest possible live
// call — listing models costs no tokens. A rejected key returns a
// *KeyValidationError; any other error means the check itself failed (network,
// outage) and says nothing about the key.
//
// baseURL empty means the real API; tests point it at an httptest server.
func ValidateOpenAIKey(ctx context.Context, apiKey, baseURL string) error {
	if baseURL == "" {
		baseURL = DefaultOpenAIBaseURL
	}
	client := openai.NewClient(
		option.WithAPIKey(apiKey),
		option.WithBaseURL(baseURL),
		option.WithHTTPClient(&http.Client{Timeout: requestTimeout}),
		option.WithMaxRetries(1),
	)
	_, err := client.Models.List(ctx)
	if err == nil {
		return nil
	}
	var apiErr *openai.Error
	if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden) {
		reason := apiErr.Message
		if reason == "" {
			reason = fmt.Sprintf("provider returned HTTP %d", apiErr.StatusCode)
		}
		return &KeyValidationError{Reason: reason}
	}
	return fmt.Errorf("llm: key validation call failed: %w", err)
}
