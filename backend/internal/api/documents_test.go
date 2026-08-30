package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"cortex/internal/api"
)

// TEST-7.4 — documents API + rate-limited refresh (REQ-7.4, REQ-7.5).
//
// The listing is read from our documents table (never live APIs): source
// filter, ILIKE search across title and content, per-source counts, a hard
// limit cap of 100, and a 404 for an unknown id. The refresh endpoint enforces
// the server-side cooldown: a finalized index job younger than
// INDEX_REFRESH_COOLDOWN answers 429 with retry_after_seconds; older or absent
// enqueues all three sources.

const testRefreshCooldown = 15 * time.Minute

func newDocumentsRouter(db api.DB, enqueuer api.Enqueuer) http.Handler {
	return api.NewRouter(withTestAuth(api.Deps{
		DB:                   db,
		Enqueuer:             enqueuer,
		Model:                testModel,
		IndexSources:         indexSources,
		Logger:               discardLogger(),
		IndexRefreshCooldown: testRefreshCooldown,
	}))
}

// seedDocument inserts one document row directly (raw SQL, not sqlc — the
// assertions must not go through the code under test) and returns its id.
func seedDocument(t *testing.T, pool *pgxpool.Pool, source, title, content string, ts *time.Time) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO documents (source, external_id, title, url, content, content_hash, source_timestamp)
		 VALUES ($1, $2, $3, $4, $5, md5($5), $6) RETURNING id`,
		source, source+"-"+uuid.NewString(), title,
		"https://example.com/"+source+"/"+strings.ReplaceAll(title, " ", "-"),
		content, ts).Scan(&id); err != nil {
		t.Fatalf("insert document %q: %v", title, err)
	}
	return id
}

// documentsPool truncates documents (testPool only truncates the users
// cascade) and returns the pool.
func documentsPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testPool(t)
	if _, err := pool.Exec(context.Background(), "TRUNCATE documents CASCADE"); err != nil {
		t.Fatalf("truncate documents: %v", err)
	}
	return pool
}

// listBody is the GET /api/documents response shape the frontend consumes.
type listBody struct {
	Documents []struct {
		ID      string `json:"id"`
		Source  string `json:"source"`
		Title   string `json:"title"`
		URL     string `json:"url"`
		Snippet string `json:"snippet"`
	} `json:"documents"`
	Counts map[string]int64 `json:"counts"`
}

func getDocuments(t *testing.T, h http.Handler, query string) (*httptest.ResponseRecorder, listBody) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, localRequest(http.MethodGet, "/api/documents"+query, nil))
	var body listBody
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode documents response %q: %v", rec.Body.String(), err)
		}
	}
	return rec, body
}

func TestListDocumentsFiltersSearchesAndCounts(t *testing.T) {
	pool := documentsPool(t)
	now := time.Now().UTC().Truncate(time.Second)
	older := now.Add(-48 * time.Hour)

	seedDocument(t, pool, "jira", "ATLAS-1 payments sandbox down", "the sandbox is unreachable", &older)
	seedDocument(t, pool, "jira", "ATLAS-2 login flaky", "sessions drop intermittently", &now)
	seedDocument(t, pool, "notion", "Q3 launch plan", "the launch depends on the payments fix", &older)
	gmailID := seedDocument(t, pool, "gmail", "Nordwind shipment delayed",
		"Hello team, the Nordwind Logistics shipment is delayed until Friday. Regards", &now)

	h := newDocumentsRouter(pool, &stubEnqueuer{})

	t.Run("unfiltered listing counts every source", func(t *testing.T) {
		rec, body := getDocuments(t, h, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
		}
		if len(body.Documents) != 4 {
			t.Errorf("documents = %d, want 4", len(body.Documents))
		}
		want := map[string]int64{"jira": 2, "notion": 1, "gmail": 1}
		for source, n := range want {
			if body.Counts[source] != n {
				t.Errorf("counts[%s] = %d, want %d", source, body.Counts[source], n)
			}
		}
	})

	t.Run("source filter returns only that source", func(t *testing.T) {
		rec, body := getDocuments(t, h, "?source=jira")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
		}
		if len(body.Documents) != 2 {
			t.Fatalf("documents = %d, want 2", len(body.Documents))
		}
		for _, d := range body.Documents {
			if d.Source != "jira" {
				t.Errorf("document %q has source %q, want jira", d.Title, d.Source)
			}
		}
	})

	t.Run("an unknown source is a client error", func(t *testing.T) {
		rec, _ := getDocuments(t, h, "?source=slack")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
		}
	})

	t.Run("search matches content case-insensitively with a contextual snippet", func(t *testing.T) {
		rec, body := getDocuments(t, h, "?q=nordwind")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
		}
		if len(body.Documents) != 1 {
			t.Fatalf("documents = %d, want just the delay email (got %+v)", len(body.Documents), body.Documents)
		}
		got := body.Documents[0]
		if got.ID != gmailID.String() {
			t.Errorf("matched document = %s, want the Nordwind email %s", got.ID, gmailID)
		}
		if !strings.Contains(got.Snippet, "Nordwind") {
			t.Errorf("snippet %q does not contain the match", got.Snippet)
		}
		if body.Counts["gmail"] != 1 {
			t.Errorf("counts[gmail] = %d under the search filter, want 1", body.Counts["gmail"])
		}
	})

	t.Run("search matches titles too", func(t *testing.T) {
		rec, body := getDocuments(t, h, "?q=ATLAS-2")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
		}
		if len(body.Documents) != 1 || body.Documents[0].Title != "ATLAS-2 login flaky" {
			t.Errorf("documents = %+v, want the ATLAS-2 issue only", body.Documents)
		}
	})

	t.Run("ordering is source_timestamp DESC", func(t *testing.T) {
		rec, body := getDocuments(t, h, "?source=jira")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
		}
		if len(body.Documents) != 2 {
			t.Fatalf("documents = %d, want 2", len(body.Documents))
		}
		if body.Documents[0].Title != "ATLAS-2 login flaky" {
			t.Errorf("first document = %q, want the newest (ATLAS-2)", body.Documents[0].Title)
		}
	})
}

// The page size is capped at 100 no matter what the caller asks for.
func TestListDocumentsCapsTheLimitAt100(t *testing.T) {
	pool := documentsPool(t)
	ctx := context.Background()
	// Batch insert: 120 rows one-by-one is measurably slow under the advisory lock.
	var sb strings.Builder
	sb.WriteString(`INSERT INTO documents (source, external_id, title, content, content_hash) VALUES `)
	for i := 0; i < 120; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `('jira', 'BULK-%d', 'bulk issue %d', 'filler', 'h%d')`, i, i, i)
	}
	if _, err := pool.Exec(ctx, sb.String()); err != nil {
		t.Fatalf("bulk insert: %v", err)
	}

	h := newDocumentsRouter(pool, &stubEnqueuer{})
	rec, body := getDocuments(t, h, "?limit=100000")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
	}
	if len(body.Documents) != 100 {
		t.Errorf("documents = %d with limit=100000, want the cap of 100", len(body.Documents))
	}
	if body.Counts["jira"] != 120 {
		t.Errorf("counts[jira] = %d, want the full 120 despite the page cap", body.Counts["jira"])
	}
}

func TestGetDocumentDetailAnd404(t *testing.T) {
	pool := documentsPool(t)
	ts := time.Now().UTC().Truncate(time.Second)
	id := seedDocument(t, pool, "gmail", "Nordwind shipment delayed",
		"Full body of the delay email, long enough to prove detail is not a snippet.", &ts)

	h := newDocumentsRouter(pool, &stubEnqueuer{})

	t.Run("detail returns full content and source URL", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, localRequest(http.MethodGet, "/api/documents/"+id.String(), nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
		}
		var got struct {
			ID      string `json:"id"`
			Source  string `json:"source"`
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode detail %q: %v", rec.Body.String(), err)
		}
		if got.ID != id.String() || got.Source != "gmail" {
			t.Errorf("detail = %+v, want the seeded gmail document", got)
		}
		if !strings.Contains(got.Content, "Full body of the delay email") {
			t.Errorf("content = %q, want the full stored content", got.Content)
		}
		if got.URL == "" {
			t.Error("detail has no source URL — the Open in Gmail link depends on it")
		}
	})

	t.Run("an unknown id is 404", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, localRequest(http.MethodGet, "/api/documents/"+uuid.NewString(), nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404 (body %q)", rec.Code, rec.Body.String())
		}
	})

	t.Run("a malformed id is 400", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, localRequest(http.MethodGet, "/api/documents/not-a-uuid", nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
		}
	})
}

func postRefresh(t *testing.T, h http.Handler, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	req := localRequest(http.MethodPost, "/api/documents/refresh", strings.NewReader(`{}`))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// A refresh inside the cooldown answers 429 with retry_after_seconds; outside
// it (or before any index run) it enqueues all three sources.
func TestRefreshDocumentsEnforcesTheCooldown(t *testing.T) {
	t.Parallel()

	t.Run("a fresh finalized index job is a 429 with retry_after_seconds", func(t *testing.T) {
		t.Parallel()
		const elapsed = 5 * time.Minute
		enqueuer := &stubEnqueuer{lastIndexFinishedAt: time.Now().Add(-elapsed)}
		h := newDocumentsRouter(&stubDB{}, enqueuer)

		rec := postRefresh(t, h, "application/json")
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429 (body %q)", rec.Code, rec.Body.String())
		}
		var body struct {
			Error             string `json:"error"`
			RetryAfterSeconds int    `json:"retry_after_seconds"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode 429 body %q: %v", rec.Body.String(), err)
		}
		if body.Error != "refresh_cooldown" {
			t.Errorf(`error = %q, want "refresh_cooldown"`, body.Error)
		}
		// ~10 minutes remain of the 15-minute window; a lax band keeps the
		// assertion honest without racing the clock.
		remaining := int((testRefreshCooldown - elapsed).Seconds())
		if body.RetryAfterSeconds < remaining-5 || body.RetryAfterSeconds > remaining+5 {
			t.Errorf("retry_after_seconds = %d, want ≈%d", body.RetryAfterSeconds, remaining)
		}
		if rec.Header().Get("Retry-After") == "" {
			t.Error("429 carries no Retry-After header")
		}
		if len(enqueuer.indexed) != 0 {
			t.Errorf("a cooled-down refresh queued %v, want nothing", enqueuer.indexed)
		}
	})

	staleCases := []struct {
		name       string
		finishedAt time.Time
	}{
		{name: "an index job older than the cooldown enqueues", finishedAt: time.Now().Add(-testRefreshCooldown - time.Minute)},
		{name: "no index job ever means no cooldown", finishedAt: time.Time{}},
	}
	for _, tt := range staleCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			enqueuer := &stubEnqueuer{lastIndexFinishedAt: tt.finishedAt}
			h := newDocumentsRouter(&stubDB{}, enqueuer)

			rec := postRefresh(t, h, "application/json")
			if rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202 (body %q)", rec.Code, rec.Body.String())
			}
			var body struct {
				Queued []string `json:"queued"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode 202 body %q: %v", rec.Body.String(), err)
			}
			if !equalStringSlices(body.Queued, indexSources) {
				t.Errorf("queued = %v, want %v", body.Queued, indexSources)
			}
			if !equalStringSlices(enqueuer.indexed, indexSources) {
				t.Errorf("enqueuer received %v, want %v", enqueuer.indexed, indexSources)
			}
		})
	}

	t.Run("a non-JSON content type is refused before any queueing", func(t *testing.T) {
		t.Parallel()
		enqueuer := &stubEnqueuer{}
		h := newDocumentsRouter(&stubDB{}, enqueuer)

		rec := postRefresh(t, h, "")
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Errorf("status = %d, want 415 (body %q)", rec.Code, rec.Body.String())
		}
		if len(enqueuer.indexed) != 0 {
			t.Errorf("queued %v on a refused request", enqueuer.indexed)
		}
	})
}
