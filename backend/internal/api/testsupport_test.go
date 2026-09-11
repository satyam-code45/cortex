package api_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"cortex/internal/api"
	"cortex/internal/auth"
)

const devUserEmail = "dev@cortex.local"

// testAPIToken is the static bearer token test routers accept. Most tests
// authenticate with it (the operator credential — make index, scripts); the
// session-cookie path has its own tests.
const testAPIToken = "test-api-token"

// stubEnqueuer is a hand-written api.Enqueuer: no mocking framework, the locked
// stack has no test dependencies.
//
// It replaces the original stubProvider. Since the chat handler became asynchronous
// it makes no LLM calls at all, and the thing worth asserting is whether a run
// was queued — and, when err is set, that a failed enqueue takes the whole
// transaction down with it.
type stubEnqueuer struct {
	mu      sync.Mutex
	err     error
	runIDs  []uuid.UUID
	indexed []string

	// writes records the actions queued for execution, and resumed the runs
	// queued to continue. The approval tests assert on these: that approving
	// queues exactly one write, and that rejecting the last outstanding action
	// queues exactly one resume.
	writes  []uuid.UUID
	resumed []uuid.UUID

	// lastIndexFinishedAt configures NewestFinalizedIndexJob; the zero value
	// means no index job has ever finished (no cooldown).
	lastIndexFinishedAt time.Time
}

var _ api.Enqueuer = (*stubEnqueuer)(nil)

func (s *stubEnqueuer) EnqueueAgentRun(_ context.Context, _ pgx.Tx, runID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runIDs = append(s.runIDs, runID)
	return s.err
}

// EnqueueExecuteWrite records a queued write execution. It shares err with the
// other enqueues: the failure path under test is "the queue is down", and for an
// approval that has to take the whole transaction with it — an approval that
// commits without a job would be a decision nothing ever acts on.
func (s *stubEnqueuer) EnqueueExecuteWrite(_ context.Context, _ pgx.Tx, actionID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes = append(s.writes, actionID)
	return s.err
}

// EnqueueResumeRun records a queued run continuation.
func (s *stubEnqueuer) EnqueueResumeRun(_ context.Context, _ pgx.Tx, runID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resumed = append(s.resumed, runID)
	return s.err
}

// queuedWrites returns the actions queued for execution.
func (s *stubEnqueuer) queuedWrites() []uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]uuid.UUID(nil), s.writes...)
}

// queuedResumes returns the runs queued to continue.
func (s *stubEnqueuer) queuedResumes() []uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]uuid.UUID(nil), s.resumed...)
}

// EnqueueIndexSource records a queued reindex. It shares err with
// EnqueueAgentRun: both failure paths are "the queue is down".
func (s *stubEnqueuer) EnqueueIndexSource(_ context.Context, source string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.indexed = append(s.indexed, source)
	return s.err
}

func (s *stubEnqueuer) NewestFinalizedIndexJob(_ context.Context) (time.Time, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastIndexFinishedAt, !s.lastIndexFinishedAt.IsZero(), nil
}

func (s *stubEnqueuer) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runIDs)
}

func (s *stubEnqueuer) lastRunID(t *testing.T) uuid.UUID {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.runIDs) == 0 {
		t.Fatal("enqueuer was never called")
	}
	return s.runIDs[len(s.runIDs)-1]
}

// stubDB satisfies api.DB without a real Postgres, for the handler paths that
// return before touching the database and for the health check.
//
// Every query method fails loudly rather than returning empty results: these
// tests assert that a request was rejected *before* any database work, and a
// silently-succeeding stub would let a regression through.
type stubDB struct {
	pingErr  error
	beginErr error
	begins   int
}

func (d *stubDB) Begin(_ context.Context) (pgx.Tx, error) {
	d.begins++
	if d.beginErr != nil {
		return nil, d.beginErr
	}
	return nil, errors.New("stubDB: Begin is not supported; this test must not reach the database")
}

func (d *stubDB) Ping(_ context.Context) error { return d.pingErr }

func (d *stubDB) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errStubDBQuery
}

func (d *stubDB) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return nil, errStubDBQuery
}

func (d *stubDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	// The one query the bearer-auth middleware makes before any handler runs.
	// Answering it — and only it — keeps stubDB usable for tests that assert a
	// request is rejected before real database work.
	if strings.Contains(sql, "INSERT INTO users") {
		email := devUserEmail
		if len(args) == 1 {
			if s, ok := args[0].(string); ok {
				email = s
			}
		}
		return userRow{email: email}
	}
	return errRow{}
}

// stubUserID is the fixed id userRow scans; tests that care which user acted
// can compare against it.
var stubUserID = uuid.MustParse("00000000-0000-0000-0000-00000000d0d0")

// userRow satisfies the UpsertUser scan (id, email, created_at, name,
// avatar_url, google_sub, use_demo_workspace).
type userRow struct{ email string }

func (r userRow) Scan(dest ...any) error {
	if len(dest) != 7 {
		return fmt.Errorf("userRow: %d scan destinations, want 7", len(dest))
	}
	*(dest[0].(*uuid.UUID)) = stubUserID
	*(dest[1].(*string)) = r.email
	*(dest[2].(*pgtype.Timestamptz)) = pgtype.Timestamptz{Time: time.Unix(0, 0).UTC(), Valid: true}
	*(dest[3].(**string)) = nil
	*(dest[4].(**string)) = nil
	*(dest[5].(**string)) = nil
	*(dest[6].(*bool)) = false
	return nil
}

// errStubDBQuery is returned by every stubDB query path.
var errStubDBQuery = errors.New("stubDB: queries are not supported; this test must not reach the database")

// errRow is a pgx.Row that always fails, so a QueryRow reached by mistake
// surfaces as a test failure rather than as a zero value.
type errRow struct{}

func (errRow) Scan(_ ...any) error { return errStubDBQuery }

// localRequest builds a request that carries a Host the server answers to and
// the test bearer token.
//
// httptest.NewRequest defaults Host to "example.com", which the hostCheck
// middleware correctly rejects with 421. Real clients dial localhost, so tests
// have to as well — otherwise every test would be exercising the rebinding
// guard instead of the handler it is about. The bearer default is the same
// idea for auth: a test about a handler must not be exercising the 401 path
// instead; auth's own tests build unauthenticated requests deliberately.
func localRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.Host = "localhost:8080"
	req.Header.Set("Authorization", "Bearer "+testAPIToken)
	return req
}

// anonymousRequest is localRequest without credentials, for tests about the
// 401 path itself.
func anonymousRequest(method, target string, body io.Reader) *http.Request {
	req := localRequest(method, target, body)
	req.Header.Del("Authorization")
	return req
}

// humanRequest builds a request authenticated as a signed-in person rather than
// with the operator token.
//
// The endpoints that decide a write — approve, reject, and the per-source writes
// toggle — refuse the static bearer token deliberately: the guarantee they carry
// is that a PERSON read the payload and approved it, and a shared machine
// credential that lives in scripts, shell history and CI config is not a person.
// Tests about those handlers therefore have to arrive the way a browser does.
//
// The session is created for devUserEmail, the same email the action fixtures
// seed their owner with, so the caller is also the owner of what it decides.
func humanRequest(t *testing.T, pool *pgxpool.Pool, method, target string, body io.Reader) *http.Request {
	t.Helper()
	req := anonymousRequest(method, target, body)
	cookie, _ := createSessionForEmail(t, pool, devUserEmail)
	req.AddCookie(cookie)
	return req
}

// putJSONAsHuman is putJSON for an endpoint that refuses the operator token.
func putJSONAsHuman(
	t *testing.T,
	pool *pgxpool.Pool,
	h http.Handler,
	target, body string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := humanRequest(t, pool, http.MethodPut, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// withTestAuth fills the auth-related Deps every test router needs: bearer
// auth on, acting as the dev user.
func withTestAuth(deps api.Deps) api.Deps {
	deps.APIToken = testAPIToken
	deps.BearerEmail = devUserEmail
	return deps
}

// giveLLMKey stores a (fake-ciphertext) LLM key row for email, so POST
// /api/chat passes the BYOK existence check. The bytes only matter to the
// worker's decrypt path, which these handler tests never reach.
func giveLLMKey(t *testing.T, pool *pgxpool.Pool, email string) {
	t.Helper()
	ctx := context.Background()
	var userID uuid.UUID
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email) VALUES ($1) ON CONFLICT (email) DO UPDATE SET email = EXCLUDED.email RETURNING id",
		email).Scan(&userID); err != nil {
		t.Fatalf("insert user %s: %v", email, err)
	}
	if _, err := pool.Exec(ctx,
		"INSERT INTO user_llm_keys (user_id, provider, key_ciphertext, key_last4) VALUES ($1, 'openai', '\\x00'::bytea, '0000') ON CONFLICT (user_id) DO NOTHING",
		userID); err != nil {
		t.Fatalf("insert llm key for %s: %v", email, err)
	}
}

// createSessionForEmail inserts a user and a live session directly, returning
// the cookie a browser would hold and the user's id. It goes through
// auth.NewSessionToken so the hash-only-in-DB invariant is what's exercised.
func createSessionForEmail(t *testing.T, pool *pgxpool.Pool, email string) (*http.Cookie, uuid.UUID) {
	t.Helper()
	ctx := context.Background()

	var userID uuid.UUID
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email) VALUES ($1) ON CONFLICT (email) DO UPDATE SET email = EXCLUDED.email RETURNING id",
		email).Scan(&userID); err != nil {
		t.Fatalf("insert user %s: %v", email, err)
	}
	token, hash, err := auth.NewSessionToken()
	if err != nil {
		t.Fatalf("mint session token: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"INSERT INTO sessions (user_id, token_hash, expires_at) VALUES ($1, $2, now() + interval '1 hour')",
		userID, hash); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	return &http.Cookie{Name: auth.SessionCookieName, Value: token}, userID
}

// discardLogger keeps test output readable; handlers log errors on purpose.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testDatabaseLockID namespaces the advisory lock that serializes database-backed
// tests across packages. Any stable constant works; this one spells "cortex".
const testDatabaseLockID = int64(0x636F72746578)

// testPool connects to TEST_DATABASE_URL and truncates the core tables.
// Integration tests SKIP (never fail) when the variable is unset.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database-backed test")
	}
	assertNotDevDatabase(t, dsn)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to TEST_DATABASE_URL: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping TEST_DATABASE_URL: %v", err)
	}
	// users cascades to conversations → messages/agent_runs → llm_calls/run_events.
	// `go test ./...` runs packages in parallel, and every database-backed test
	// in this repository truncates. A session-level advisory lock serializes
	// them across packages: without it, another package's TRUNCATE lands in the
	// middle of this test and the failures look random.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire test database connection: %v", err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", testDatabaseLockID); err != nil {
		conn.Release()
		t.Fatalf("lock test database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", testDatabaseLockID); err != nil {
			t.Errorf("unlock test database: %v", err)
		}
		conn.Release()
	})

	if _, err := pool.Exec(ctx, "TRUNCATE users CASCADE"); err != nil {
		t.Fatalf("truncate test database: %v", err)
	}
	return pool
}

// queryInt runs a scalar count/aggregate against the test database. Verification
// SQL lives in the test rather than in db/queries so the assertions do not go
// through the same sqlc code the handler is being tested on.
func queryInt(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return n
}

// assertNotDevDatabase refuses to run against the development database.
//
// testPool truncates users, which cascades to every table the app writes. The
// natural way to "fix" a missing test database is to repoint
// TEST_DATABASE_URL at DATABASE_URL, and that would silently wipe real data on
// the next test run — so make it a hard failure instead of a comment in
// .env.example.
func assertNotDevDatabase(t *testing.T, dsn string) {
	t.Helper()

	name, err := databaseName(dsn)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	if devDSN := os.Getenv("DATABASE_URL"); devDSN != "" {
		if devName, err := databaseName(devDSN); err == nil && devName == name {
			t.Fatalf("TEST_DATABASE_URL points at the development database %q; tests truncate it", name)
		}
	}
	if !strings.HasSuffix(name, "_test") {
		t.Fatalf("TEST_DATABASE_URL database %q must end in _test; tests truncate it", name)
	}
}

// databaseName extracts the database name from a Postgres DSN. The error never
// includes the DSN itself, which carries the password.
func databaseName(dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		// Deliberately not %w: url.Parse returns *url.Error, whose Error()
		// formats the raw URL — password included — and the caller t.Fatalf's
		// this into captured test output.
		return "", errors.New("invalid DSN")
	}
	name := strings.TrimPrefix(u.Path, "/")
	if name == "" {
		return "", fmt.Errorf("DSN has no database name")
	}
	return name, nil
}
