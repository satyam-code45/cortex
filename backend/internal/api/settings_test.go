package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"cortex/internal/api"
	"cortex/internal/keys"
)

// TEST-7.2 — bring-your-own-key endpoints (REQ-7.2).
//
// The invariants under test are the spec's, not the handlers': the key is
// validated with one live call before it is stored, the stored bytes are
// ciphertext (never the key), no endpoint ever returns the key, a bad key is a
// 422 carrying the provider's reason, and chat without a key is a 409 the
// frontend can route on.

// fakeOpenAI is an httptest stand-in for the models-list validation call: 200
// for exactly one key, an OpenAI-shaped 401 for everything else.
type fakeOpenAI struct {
	server   *httptest.Server
	validKey string
	calls    atomic.Int64
}

const fakeOpenAIRejection = "Incorrect API key provided: sk-bad."

func newFakeOpenAI(t *testing.T, validKey string) *fakeOpenAI {
	t.Helper()
	f := &fakeOpenAI{validKey: validKey}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") == "Bearer "+f.validKey {
			_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"` + fakeOpenAIRejection + `","type":"invalid_request_error","code":"invalid_api_key"}}`))
	}))
	t.Cleanup(f.server.Close)
	return f
}

// newSettingsRouter wires a router whose key validation hits the fake.
func newSettingsRouter(t *testing.T, db api.DB, openAIBaseURL string) http.Handler {
	t.Helper()
	return api.NewRouter(withTestAuth(api.Deps{
		DB:            db,
		Enqueuer:      &stubEnqueuer{},
		Model:         testModel,
		Logger:        discardLogger(),
		Keys:          newTestKeysService(t, db),
		OpenAIBaseURL: openAIBaseURL,
	}))
}

func putKey(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := localRequest(http.MethodPut, "/api/settings/llm-key", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPutLLMKeyValidatesEncryptsAndNeverEchoesTheKey(t *testing.T) {
	const apiKey = "sk-test-users-own-key-0042"

	pool := testPool(t)
	fake := newFakeOpenAI(t, apiKey)
	h := newSettingsRouter(t, pool, fake.server.URL)

	rec := putKey(t, h, `{"provider":"openai","key":"`+apiKey+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if fake.calls.Load() != 1 {
		t.Errorf("validation calls = %d, want exactly 1 live check", fake.calls.Load())
	}

	var saved struct {
		Provider string `json:"provider"`
		Last4    string `json:"last4"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatalf("decode PUT response %q: %v", rec.Body.String(), err)
	}
	if saved.Provider != "openai" {
		t.Errorf("provider = %q, want openai", saved.Provider)
	}
	if want := apiKey[len(apiKey)-4:]; saved.Last4 != want {
		t.Errorf("last4 = %q, want %q", saved.Last4, want)
	}
	if strings.Contains(rec.Body.String(), apiKey) {
		t.Errorf("PUT response echoes the key: %q", rec.Body.String())
	}

	// Stored ciphertext differs from the plaintext and decrypts back to it
	// under the configured secret.
	var ciphertext []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT key_ciphertext FROM user_llm_keys`).Scan(&ciphertext); err != nil {
		t.Fatalf("read stored ciphertext: %v", err)
	}
	if string(ciphertext) == apiKey || strings.Contains(string(ciphertext), apiKey) {
		t.Fatal("the database holds the plaintext key")
	}
	cipher, err := keys.NewCipher(testKeyEncryptionSecret)
	if err != nil {
		t.Fatalf("build cipher: %v", err)
	}
	plain, err := cipher.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("decrypt stored ciphertext: %v", err)
	}
	if plain != apiKey {
		t.Errorf("decrypted key = %q, want the stored key", plain)
	}

	// GET reports provider + last4 only — the key itself comes back from no
	// endpoint, ever.
	get := httptest.NewRecorder()
	h.ServeHTTP(get, localRequest(http.MethodGet, "/api/settings/llm-key", nil))
	if get.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200 (body %q)", get.Code, get.Body.String())
	}
	if strings.Contains(get.Body.String(), apiKey) {
		t.Errorf("GET response echoes the key: %q", get.Body.String())
	}
	var info struct {
		Provider string `json:"provider"`
		Last4    string `json:"last4"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode GET response %q: %v", get.Body.String(), err)
	}
	if info.Provider != "openai" || info.Last4 != apiKey[len(apiKey)-4:] {
		t.Errorf("GET = %+v, want provider openai and the last4", info)
	}

	// /api/auth/me flips has_llm_key, which gates the chat input.
	me := httptest.NewRecorder()
	h.ServeHTTP(me, localRequest(http.MethodGet, "/api/auth/me", nil))
	if me.Code != http.StatusOK {
		t.Fatalf("GET /api/auth/me = %d (body %q)", me.Code, me.Body.String())
	}
	var meBody struct {
		HasLLMKey bool `json:"has_llm_key"`
	}
	if err := json.Unmarshal(me.Body.Bytes(), &meBody); err != nil {
		t.Fatalf("decode me: %v", err)
	}
	if !meBody.HasLLMKey {
		t.Error("has_llm_key = false after storing a key, want true")
	}
}

// A key the provider rejects is a 422 carrying the provider's reason, and
// nothing is stored.
func TestPutLLMKeyRejectsAnInvalidKeyWith422(t *testing.T) {
	pool := testPool(t)
	fake := newFakeOpenAI(t, "sk-the-only-valid-key")
	h := newSettingsRouter(t, pool, fake.server.URL)

	rec := putKey(t, h, `{"provider":"openai","key":"sk-bad"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), fakeOpenAIRejection) {
		t.Errorf("422 body %q does not carry the provider's reason %q", rec.Body.String(), fakeOpenAIRejection)
	}
	if n := queryInt(t, pool, `SELECT count(*) FROM user_llm_keys`); n != 0 {
		t.Errorf("stored keys = %d after a rejected key, want 0", n)
	}
}

// gemini is schema-legal but API-rejected until an implementation lands; junk
// providers and empty keys are rejected before any live call.
func TestPutLLMKeyRejectsUnusableRequests(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantSubstr string
	}{
		{
			name:       "gemini is coming soon",
			body:       `{"provider":"gemini","key":"any-key"}`,
			wantStatus: http.StatusUnprocessableEntity,
			wantSubstr: "coming soon",
		},
		{
			name:       "unknown provider",
			body:       `{"provider":"anthropic","key":"any-key"}`,
			wantStatus: http.StatusUnprocessableEntity,
		},
		{
			name:       "empty key",
			body:       `{"provider":"openai","key":"  "}`,
			wantStatus: http.StatusUnprocessableEntity,
		},
		{
			name:       "malformed JSON",
			body:       `{"provider":`,
			wantStatus: http.StatusBadRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeOpenAI(t, "sk-valid")
			// stubDB: all of these must be rejected before database work.
			h := newSettingsRouter(t, &stubDB{}, fake.server.URL)

			rec := putKey(t, h, tt.body)
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d (body %q)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantSubstr != "" && !strings.Contains(rec.Body.String(), tt.wantSubstr) {
				t.Errorf("body %q does not mention %q", rec.Body.String(), tt.wantSubstr)
			}
			if tt.name != "empty key" && fake.calls.Load() != 0 {
				t.Errorf("a rejected request still made %d validation calls", fake.calls.Load())
			}
		})
	}
}

// DELETE removes the stored key; asking about a key that is not there is a 404.
func TestLLMKeyDeleteAndAbsenceAre404Aware(t *testing.T) {
	const apiKey = "sk-test-delete-me-9999"

	pool := testPool(t)
	fake := newFakeOpenAI(t, apiKey)
	h := newSettingsRouter(t, pool, fake.server.URL)

	// Nothing stored yet: GET and DELETE both 404.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, localRequest(http.MethodGet, "/api/settings/llm-key", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET with no key = %d, want 404", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, localRequest(http.MethodDelete, "/api/settings/llm-key", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("DELETE with no key = %d, want 404", rec.Code)
	}

	if rec := putKey(t, h, `{"provider":"openai","key":"`+apiKey+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d (body %q)", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, localRequest(http.MethodDelete, "/api/settings/llm-key", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want 204 (body %q)", rec.Code, rec.Body.String())
	}
	if n := queryInt(t, pool, `SELECT count(*) FROM user_llm_keys`); n != 0 {
		t.Errorf("stored keys = %d after DELETE, want 0", n)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, localRequest(http.MethodGet, "/api/settings/llm-key", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET after DELETE = %d, want 404", rec.Code)
	}
}

// Chat without a stored key is the exact 409 the frontend routes to settings —
// and nothing is created: no conversation, no message, no run, no job.
func TestChatWithoutAKeyIs409LLMKeyRequired(t *testing.T) {
	pool := testPool(t)
	enqueuer := &stubEnqueuer{}
	h := newChatRouter(pool, enqueuer)

	rec := postChat(t, h, `{"message":"who blocked ATLAS-1?"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %q)", rec.Code, rec.Body.String())
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode 409 body %q: %v", rec.Body.String(), err)
	}
	if body.Error != "llm_key_required" {
		t.Errorf(`error = %q, want "llm_key_required" (the frontend routes on this exact string)`, body.Error)
	}
	if enqueuer.callCount() != 0 {
		t.Errorf("a keyless chat enqueued %d runs, want 0", enqueuer.callCount())
	}
	if n := queryInt(t, pool, `SELECT count(*) FROM conversations`); n != 0 {
		t.Errorf("conversations = %d after a 409, want 0", n)
	}
	if n := queryInt(t, pool, `SELECT count(*) FROM agent_runs`); n != 0 {
		t.Errorf("agent_runs = %d after a 409, want 0", n)
	}
}
