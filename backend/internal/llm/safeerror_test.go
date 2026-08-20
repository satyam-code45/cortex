package llm_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"

	"cortex/internal/llm"
)

// SafeErrorMessage is what stands between the provider's raw error and
// agent_runs.error / the run_events transcript, so its classification of a real
// *openai.Error is asserted directly — the handler tests only ever reach the
// default branch.
func TestSafeErrorMessage(t *testing.T) {
	t.Parallel()

	// A 401 as the API actually returns it: the body echoes a partially-masked
	// key, and the URL of a proxy-style base URL can carry a credential.
	apiErr := &openai.Error{
		Code:       "invalid_api_key",
		Type:       "invalid_request_error",
		StatusCode: http.StatusUnauthorized,
		Request: &http.Request{
			Method: http.MethodPost,
			URL:    mustParseURL(t, "https://proxy.example.com/v1/chat/completions?api-key=super-secret"),
		},
		Response: &http.Response{StatusCode: http.StatusUnauthorized},
	}

	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "nil", err: nil, want: ""},
		{
			name: "api error is reduced to status, type and code",
			err:  apiErr,
			want: "llm provider returned HTTP 401, type=invalid_request_error, code=invalid_api_key",
		},
		{
			name: "wrapped api error is still recognised",
			err:  fmt.Errorf("openai: chat completion: %w", apiErr),
			want: "llm provider returned HTTP 401, type=invalid_request_error, code=invalid_api_key",
		},
		{
			name: "deadline exceeded",
			err:  fmt.Errorf("openai: chat completion: %w", context.DeadlineExceeded),
			want: "llm request timed out",
		},
		{
			name: "canceled",
			err:  fmt.Errorf("openai: chat completion: %w", context.Canceled),
			want: "llm request canceled",
		},
		{
			name: "transport errors reveal nothing but the category",
			err:  &url.Error{Op: "Post", URL: "https://proxy.example.com/v1?api-key=super-secret", Err: errors.New("dial tcp: connection refused")},
			want: "llm request failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := llm.SafeErrorMessage(tt.err)
			if got != tt.want {
				t.Errorf("SafeErrorMessage() = %q, want %q", got, tt.want)
			}
		})
	}
}

// The whole point of the helper: nothing it returns may echo the endpoint or
// the upstream response body.
func TestSafeErrorMessageLeaksNothing(t *testing.T) {
	t.Parallel()

	const secret = "super-secret"
	rawBody := `{"error":{"message":"Incorrect API key provided: sk-proj-ABCD...WXYZ."}}`

	apiErr := &openai.Error{
		Code:       "invalid_api_key",
		Type:       "invalid_request_error",
		StatusCode: http.StatusUnauthorized,
		Request: &http.Request{
			Method: http.MethodPost,
			URL:    mustParseURL(t, "https://proxy.example.com/v1/chat/completions?api-key="+secret),
		},
		Response: &http.Response{StatusCode: http.StatusUnauthorized},
	}

	got := llm.SafeErrorMessage(fmt.Errorf("openai: chat completion: %w", apiErr))
	for _, forbidden := range []string{secret, "sk-proj", "proxy.example.com", "https://", rawBody} {
		if strings.Contains(got, forbidden) {
			t.Errorf("SafeErrorMessage() = %q, must not contain %q", got, forbidden)
		}
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}
