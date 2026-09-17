package connections

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"cortex/internal/agent"
	"cortex/internal/auth"
	"cortex/internal/tools"
	"cortex/internal/tools/gmail"
	"cortex/internal/tools/jira"
	"cortex/internal/tools/notion"
)

// ErrNoSourcesConnected means the run owner has nothing to search: no
// connections, and either no demo workspace or the demo not asked for.
//
// Distinct from agent.ErrNoUsableSources, which means the user *has* sources
// but every one of them is broken. That is a run that should start and fail
// visibly with a trace; this is a run that should never start at all, and the
// chat handler turns it into a 409 that routes the user to Connections.
//
// It wraps agent.ErrNoSources so that a run which reaches the loop anyway —
// the owner disconnected their last source between enqueue and execution —
// is recognized there as permanent and failed with a reason, rather than
// classified as a transient fault and retried until River gives up while the
// run row never reaches a terminal state.
var ErrNoSourcesConnected = fmt.Errorf("connections: no sources connected: %w", agent.ErrNoSources)

// RegistryBuilderConfig configures a RegistryBuilder.
type RegistryBuilderConfig struct {
	// Service reads and marks the user's stored connections.
	Service *Service
	// Demo is the full demo-workspace registry app.Build assembled. It is
	// returned as-is (same instance, shared httpx pacing state) for users in
	// demo mode.
	Demo *tools.Registry

	// GoogleClientID and GoogleClientSecret are the sign-in Web OAuth client;
	// per-user Gmail refresh tokens were minted against it and refresh
	// through it.
	GoogleClientID     string
	GoogleClientSecret string

	// GoogleTokenURL, NotionBaseURL and GmailBaseURL override provider
	// endpoints; tests point them at httptest servers. Production leaves
	// them empty.
	GoogleTokenURL string
	NotionBaseURL  string
	GmailBaseURL   string

	// GmailSendAllowedDomains restricts who a proposed email may be addressed
	// to. Empty means any domain, which is the default. It is threaded down to
	// the send tool so an out-of-policy recipient is refused at proposal time,
	// where it is a correctable observation rather than a wasted approval.
	GmailSendAllowedDomains []string

	Logger *slog.Logger
}

// RegistryBuilder builds each run's tool registry from the run owner's source
// connections: decrypt → construct clients → discard. It is the
// RegistryForUser factory app.Build hands the orchestrator.
type RegistryBuilder struct {
	cfg RegistryBuilderConfig
}

// NewRegistryBuilder validates the configuration and builds a RegistryBuilder.
func NewRegistryBuilder(cfg RegistryBuilderConfig) (*RegistryBuilder, error) {
	if cfg.Service == nil {
		return nil, errors.New("connections: Service is required")
	}
	// A nil demo registry is legal: it means the deployment has no demo
	// workspace, so demo mode is simply unreachable and every user answers from
	// their own connected sources. It used to be rejected here, which is what
	// made a demo-less server unbuildable.
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &RegistryBuilder{cfg: cfg}, nil
}

// ForUser resolves one run's tool registry. Demo mode is all-or-nothing:
// zero connections (or the demo toggle) gets the full demo registry; any
// connection gets ONLY the user's connected sources — no demo clients and no
// knowledge-base tool, so nothing fictional can enter an answer about real
// data.
//
// A connection whose credential has permanently stopped working (revoked
// refresh token, undecryptable blob) is marked status=error and dropped from
// the registry — never replaced by demo data; the UI offers "reconnect". A
// transient failure (database blip, provider hiccup during the eager Gmail
// refresh) returns a plain error so River retries the run. When every
// connection is errored the returned error wraps agent.ErrNoUsableSources and
// the empty registry and honest sources map come back with it, so the
// orchestrator can claim the run and fail it with a full trace.
func (b *RegistryBuilder) ForUser(ctx context.Context, userID uuid.UUID) (*tools.Registry, agent.Sources, error) {
	infos, err := b.cfg.Service.List(ctx, userID)
	if err != nil {
		return nil, agent.Sources{}, err
	}
	useDemo, err := b.cfg.Service.UseDemo(ctx, userID)
	if err != nil {
		return nil, agent.Sources{}, err
	}
	// A nil demo registry IS this deployment having no demo workspace, so it is
	// the authority Mode is given — one fact, read from the thing that would
	// have to serve the request.
	switch Mode(len(infos), useDemo, b.cfg.Demo != nil) {
	case agent.ModeDemo:
		return b.cfg.Demo, agent.Sources{Mode: agent.ModeDemo}, nil
	case ModeNone:
		// Nothing connected and the demo not asked for. This is a new account,
		// not a failure: the caller turns it into the 409 that routes the user
		// to Connections, and no run row is created.
		return nil, agent.Sources{}, ErrNoSourcesConnected
	}

	var toolset []tools.Tool
	connected := make([]string, 0, len(infos))
	for _, info := range infos {
		if info.Status == "error" {
			// Already marked; stays out of the registry until reconnected.
			continue
		}
		clients, err := b.buildSource(ctx, userID, info)
		if err != nil {
			return nil, agent.Sources{}, err
		}
		if clients == nil {
			// Permanently broken: marked status=error and dropped.
			continue
		}
		toolset = append(toolset, clients.tools()...)
		connected = append(connected, info.Source)
	}

	// ListUserConnections orders by source, so connected is already sorted.
	sources := agent.Sources{Mode: agent.ModeUser, Connected: connected}
	registry, err := tools.NewRegistry(toolset...)
	if err != nil {
		return nil, agent.Sources{}, err
	}
	if len(connected) == 0 {
		return registry, sources, fmt.Errorf("%w: every connection for user %s is errored", agent.ErrNoUsableSources, userID)
	}
	return registry, sources, nil
}

// buildSource decrypts one connection and constructs its clients.
//
// It returns clients rather than tools because two callers need the same
// construction for different purposes: a run needs the read tools (plus the
// write tools, when the owner enabled them), and the write execution job needs
// the executors. Building the clients twice from the same ciphertext would be
// two chances to diverge on which credential, base URL or scope was used — and
// the write must go out through exactly the client the proposal was validated
// against.
//
// A nil, nil return means the connection is permanently broken: it has been
// marked status=error and the source is dropped from this run.
func (b *RegistryBuilder) buildSource(ctx context.Context, userID uuid.UUID, info Info) (*sourceClients, error) {
	source := info.Source
	built := &sourceClients{
		source:         source,
		writesEnabled:  info.WritesEnabled,
		allowedDomains: b.cfg.GmailSendAllowedDomains,
	}

	switch source {
	case SourceJira:
		var creds JiraCredentials
		if err := b.load(ctx, userID, source, &creds); err != nil {
			return b.handleLoadError(ctx, userID, source, err)
		}
		// Re-vetted at construction, not just at paste time, so a stored URL
		// that predates (or slipped past) the paste check still cannot point
		// a run's client at an internal host.
		baseURL, err := ValidateJiraBaseURL(creds.BaseURL)
		if err != nil {
			return b.drop(ctx, userID, source, err.Error())
		}
		// No project pin: the JIRA_PROJECTS pin protects the shared demo
		// workspace's index crawl; a user's own site is theirs to search, and
		// jira_list_projects discovers their projects.
		client, err := jira.NewClient(jira.Config{
			BaseURL:  baseURL,
			Email:    creds.Email,
			APIToken: creds.APIToken,
			// A user's own site, reached with their own token: theirs to search
			// in full. The pin exists to confine the shared demo workspace, and
			// applying it here would silently hide their own projects.
			AllowUnscoped: true,
			Logger:        b.cfg.Logger,
		})
		if err != nil {
			return b.drop(ctx, userID, source, err.Error())
		}
		built.jira = client
		return built, nil

	case SourceNotion:
		var creds NotionCredentials
		if err := b.load(ctx, userID, source, &creds); err != nil {
			return b.handleLoadError(ctx, userID, source, err)
		}
		client, err := notion.NewClient(notion.Config{
			Token:   creds.Token,
			BaseURL: b.cfg.NotionBaseURL,
			Logger:  b.cfg.Logger,
		})
		if err != nil {
			return b.drop(ctx, userID, source, err.Error())
		}
		built.notion = client
		return built, nil

	case SourceGmail:
		var creds GmailCredentials
		if err := b.load(ctx, userID, source, &creds); err != nil {
			return b.handleLoadError(ctx, userID, source, err)
		}
		tokenSource, err := gmail.NewTokenSource(gmail.TokenSourceConfig{
			Credentials: b.googleCredentials(),
			Token:       &gmail.Token{RefreshToken: creds.RefreshToken},
			// No TokenPath and no DB write-back: Google does not rotate
			// refresh tokens on refresh — rotation happens only on
			// re-consent, which flows through the connect callback and
			// re-stores anyway. The in-memory update covers this run.
			Logger: b.cfg.Logger,
		})
		if err != nil {
			return b.drop(ctx, userID, source, err.Error())
		}
		// Eager refresh, one round trip: a revoked token is discovered here —
		// where the connection can still be marked — rather than mid-run
		// inside a tool call, and the fetched access token is cached in the
		// TokenSource for the whole run.
		if _, err := tokenSource.AccessToken(ctx); err != nil {
			var oauthErr *auth.OAuthError
			if errors.As(err, &oauthErr) && oauthErr.Permanent() {
				// The OAuth error carries Google's code and description,
				// never the token.
				return b.drop(ctx, userID, source, oauthErr.Error())
			}
			// Transient (network, 5xx): do not mark the connection — a
			// Google hiccup must not flash "reconnect" at the user.
			return nil, err
		}
		client, err := gmail.NewClient(gmail.Config{
			TokenSource: tokenSource,
			// The mailbox is the user's own; there is nothing to confine the
			// search to.
			AllowUnscoped: true,
			BaseURL:       b.cfg.GmailBaseURL,
			Logger:        b.cfg.Logger,
		})
		if err != nil {
			return b.drop(ctx, userID, source, err.Error())
		}
		built.gmail = client
		// The sender is read from the stored identity rather than fetched: the
		// connect callback already resolved and saved the mailbox address, and
		// it is display copy on the approval card, not a credential. One fewer
		// round trip on every single run.
		built.gmailSender = identityEmail(info.Identity)
		// Writes need the send scope on the token itself. A connection made
		// before writes were enabled has a readonly-only token, so writes stay
		// off until the user re-consents — silently registering a send tool
		// against a token that cannot send would turn an approval into a
		// failure.
		if built.writesEnabled && !GmailCanSend(creds) {
			b.cfg.Logger.Warn("gmail writes are enabled but the stored token has no send scope; "+
				"the write tool is not registered", "user_id", userID)
			built.writesEnabled = false
		}
		return built, nil

	default:
		return b.drop(ctx, userID, source, fmt.Sprintf("unknown source %q", source))
	}
}

// load decrypts one connection's credentials.
func (b *RegistryBuilder) load(ctx context.Context, userID uuid.UUID, source string, creds any) error {
	return b.cfg.Service.Load(ctx, userID, source, creds)
}

// handleLoadError classifies a credential load failure: an undecryptable blob
// is permanent (mark + drop), a connection deleted since the list is a race
// (drop silently), anything else is transient.
func (b *RegistryBuilder) handleLoadError(ctx context.Context, userID uuid.UUID, source string, err error) (*sourceClients, error) {
	if errors.Is(err, ErrUnusableCredentials) {
		return b.drop(ctx, userID, source, "stored credentials could not be decrypted — reconnect the source")
	}
	if errors.Is(err, ErrNoConnection) {
		// Deleted between List and Load; nothing to mark.
		return nil, nil
	}
	return nil, err
}

// drop marks a connection permanently broken and removes its source from this
// run. msg must be safe to show and store — provider wording, never
// credentials.
func (b *RegistryBuilder) drop(ctx context.Context, userID uuid.UUID, source, msg string) (*sourceClients, error) {
	b.cfg.Logger.Warn("connections: dropping errored source from run",
		"user_id", userID, "source", source, "reason", msg)
	if err := b.cfg.Service.MarkError(ctx, userID, source, msg); err != nil {
		return nil, err
	}
	return nil, nil
}

// googleCredentials shapes the Web OAuth client for gmail.NewTokenSource.
func (b *RegistryBuilder) googleCredentials() *gmail.Credentials {
	tokenURL := b.cfg.GoogleTokenURL
	if tokenURL == "" {
		tokenURL = auth.GoogleTokenURL
	}
	return &gmail.Credentials{
		ClientID:     b.cfg.GoogleClientID,
		ClientSecret: b.cfg.GoogleClientSecret,
		AuthURI:      auth.GoogleAuthURL,
		TokenURI:     tokenURL,
	}
}

// ValidateGmail proves an exchanged token can read its mailbox and returns
// the mailbox address for the connection's identity. The connect callback
// calls it before storing anything, which makes it the Gmail flow's
// equivalent of the paste flows' live validation. baseURL overrides the Gmail
// API root for tests.
func ValidateGmail(ctx context.Context, creds *gmail.Credentials, token *gmail.Token, baseURL string, logger *slog.Logger) (string, error) {
	source, err := gmail.NewTokenSource(gmail.TokenSourceConfig{
		Credentials: creds,
		Token:       token,
		Logger:      logger,
	})
	if err != nil {
		return "", err
	}
	client, err := gmail.NewClient(gmail.Config{
		TokenSource:   source,
		AllowUnscoped: true,
		BaseURL:       baseURL,
		Logger:        logger,
	})
	if err != nil {
		return "", err
	}
	return client.UserEmail(ctx)
}
