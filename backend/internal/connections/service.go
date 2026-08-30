// Package connections stores and serves per-user source connections (Day 8):
// a user's own Jira, Notion, and Gmail credentials, encrypted with the same
// AES-256-GCM envelope as LLM keys. It also builds the per-run tool registry
// from those connections — demo mode is all-or-nothing, so a run sees either
// the full demo workspace or ONLY the user's connected sources, never a mix.
package connections

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"cortex/internal/agent"
	"cortex/internal/keys"
	"cortex/internal/store"
)

// Source names, matching the user_connections.source CHECK constraint.
const (
	SourceJira   = "jira"
	SourceNotion = "notion"
	SourceGmail  = "gmail"
)

// ValidSource reports whether name is a connectable source.
func ValidSource(name string) bool {
	return name == SourceJira || name == SourceNotion || name == SourceGmail
}

// ErrNoConnection is returned when a user has no stored connection for a source.
var ErrNoConnection = errors.New("connections: no connection on file")

// ErrUnusableCredentials is returned when stored credentials exist but cannot
// be decrypted (the encryption secret rotated, or the blob is corrupt). Like
// ErrNoConnection it does not heal on retry — the user must reconnect.
var ErrUnusableCredentials = errors.New("connections: stored credentials are unusable")

// JiraCredentials is the plaintext shape encrypted for a jira connection.
type JiraCredentials struct {
	BaseURL  string `json:"base_url"`
	Email    string `json:"email"`
	APIToken string `json:"api_token"`
}

// NotionCredentials is the plaintext shape encrypted for a notion connection.
type NotionCredentials struct {
	Token string `json:"token"`
}

// GmailCredentials is the plaintext shape encrypted for a gmail connection.
type GmailCredentials struct {
	RefreshToken string `json:"refresh_token"`
}

// Info is what the API may show about a connection: identity and status,
// never credentials. Identity holds display facts only (site URL, account
// name, mailbox address) — the paste/connect handlers construct it, and
// nothing secret ever goes in.
type Info struct {
	Source    string
	Status    string // "active" | "error"
	LastError string // safe message, set only when Status == "error"
	Identity  json.RawMessage
	UpdatedAt time.Time
}

// Service is the one path to the user_connections table. Everything above it
// handles either ciphertext or short-lived plaintext; nothing else touches
// the cipher.
type Service struct {
	db     store.DBTX
	cipher *keys.Cipher
}

// NewService builds a Service.
func NewService(db store.DBTX, cipher *keys.Cipher) *Service {
	return &Service{db: db, cipher: cipher}
}

// Save encrypts and stores a user's credentials for one source, replacing any
// existing connection and clearing error state. The caller has already
// validated the credentials live against the provider; identity is the
// display facts that validation returned.
func (s *Service) Save(ctx context.Context, userID uuid.UUID, source string, credentials, identity any) error {
	if !ValidSource(source) {
		return fmt.Errorf("connections: unknown source %q", source)
	}
	plaintext, err := json.Marshal(credentials)
	if err != nil {
		return fmt.Errorf("connections: marshal credentials: %w", err)
	}
	ciphertext, err := s.cipher.Encrypt(string(plaintext))
	if err != nil {
		return fmt.Errorf("connections: encrypt credentials: %w", err)
	}
	identityJSON, err := json.Marshal(identity)
	if err != nil {
		return fmt.Errorf("connections: marshal identity: %w", err)
	}
	_, err = store.New(s.db).UpsertUserConnection(ctx, store.UpsertUserConnectionParams{
		UserID:                userID,
		Source:                source,
		CredentialsCiphertext: ciphertext,
		Identity:              identityJSON,
	})
	if err != nil {
		return fmt.Errorf("connections: store connection: %w", err)
	}
	return nil
}

// Load decrypts a user's stored credentials for one source into credentials
// (a pointer to the source's *Credentials struct). The plaintext exists only
// in the caller's memory for the duration of one request or run:
// decrypt → construct client → discard.
func (s *Service) Load(ctx context.Context, userID uuid.UUID, source string, credentials any) error {
	row, err := store.New(s.db).GetUserConnection(ctx, store.GetUserConnectionParams{
		UserID: userID,
		Source: source,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoConnection
	}
	if err != nil {
		return fmt.Errorf("connections: load connection: %w", err)
	}
	return s.decrypt(row.CredentialsCiphertext, credentials)
}

func (s *Service) decrypt(ciphertext []byte, credentials any) error {
	plaintext, err := s.cipher.Decrypt(ciphertext)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnusableCredentials, err)
	}
	if err := json.Unmarshal([]byte(plaintext), credentials); err != nil {
		return fmt.Errorf("%w: %w", ErrUnusableCredentials, err)
	}
	return nil
}

// List reports every connection a user has, identity and status only —
// credentials stay ciphertext.
func (s *Service) List(ctx context.Context, userID uuid.UUID) ([]Info, error) {
	rows, err := store.New(s.db).ListUserConnections(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("connections: list connections: %w", err)
	}
	infos := make([]Info, 0, len(rows))
	for _, row := range rows {
		info := Info{
			Source:    row.Source,
			Status:    row.Status,
			Identity:  json.RawMessage(row.Identity),
			UpdatedAt: row.UpdatedAt.Time,
		}
		if row.LastError != nil {
			info.LastError = *row.LastError
		}
		infos = append(infos, info)
	}
	return infos, nil
}

// Delete removes a user's connection for one source. ErrNoConnection when
// none was stored.
func (s *Service) Delete(ctx context.Context, userID uuid.UUID, source string) error {
	rows, err := store.New(s.db).DeleteUserConnection(ctx, store.DeleteUserConnectionParams{
		UserID: userID,
		Source: source,
	})
	if err != nil {
		return fmt.Errorf("connections: delete connection: %w", err)
	}
	if rows == 0 {
		return ErrNoConnection
	}
	return nil
}

// MarkError records that a stored credential stopped working (e.g. a revoked
// Gmail refresh token). The row is kept so the UI can offer "reconnect" and
// the user's runs stay in user mode — an errored source drops out of the
// run's tool registry, never silently replaced by demo data. msg must be a
// safe provider message, never credentials.
func (s *Service) MarkError(ctx context.Context, userID uuid.UUID, source, msg string) error {
	err := store.New(s.db).SetUserConnectionError(ctx, store.SetUserConnectionErrorParams{
		UserID:    userID,
		Source:    source,
		LastError: &msg,
	})
	if err != nil {
		return fmt.Errorf("connections: mark connection error: %w", err)
	}
	return nil
}

// UseDemo reports the user's "Use demo workspace" toggle.
func (s *Service) UseDemo(ctx context.Context, userID uuid.UUID) (bool, error) {
	useDemo, err := store.New(s.db).GetUserDemoWorkspace(ctx, userID)
	if err != nil {
		return false, fmt.Errorf("connections: load demo toggle: %w", err)
	}
	return useDemo, nil
}

// SetUseDemo flips the user's "Use demo workspace" toggle.
func (s *Service) SetUseDemo(ctx context.Context, userID uuid.UUID, useDemo bool) error {
	err := store.New(s.db).SetUserDemoWorkspace(ctx, store.SetUserDemoWorkspaceParams{
		ID:               userID,
		UseDemoWorkspace: useDemo,
	})
	if err != nil {
		return fmt.Errorf("connections: set demo toggle: %w", err)
	}
	return nil
}

// ValidateJiraBaseURL normalizes and vets a user-supplied Jira site URL. The
// server fetches this URL (validation at paste time, tools on every run), so
// it is an SSRF surface: HTTPS is required — every Jira Cloud site is HTTPS,
// and plain HTTP would both hand the pasted token to a cleartext host and
// invite probing internal services. Loopback hosts are exempt (local dev and
// the httptest fakes); userinfo, query, and fragment have no place in a site
// URL and are refused rather than silently dropped.
func ValidateJiraBaseURL(raw string) (string, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return "", errors.New("base_url must be your Jira site URL, e.g. https://your-site.atlassian.net")
	}
	loopback := parsed.Hostname() == "localhost" || parsed.Hostname() == "::1" ||
		strings.HasPrefix(parsed.Hostname(), "127.")
	switch parsed.Scheme {
	case "https":
	case "http":
		if !loopback {
			return "", errors.New("base_url must use https — Jira Cloud sites are always https")
		}
	default:
		return "", errors.New("base_url must be your Jira site URL, e.g. https://your-site.atlassian.net")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("base_url must be a plain site URL without credentials, query, or fragment")
	}
	return trimmed, nil
}

// Mode decides which workspace a user's runs see: agent.ModeDemo with zero
// connections or the demo toggle on, agent.ModeUser otherwise. All-or-nothing
// by design — an errored connection still counts as a connection, so a broken
// credential never silently swaps real sources for demo data.
func Mode(connectionCount int, useDemo bool) string {
	if connectionCount == 0 || useDemo {
		return agent.ModeDemo
	}
	return agent.ModeUser
}
