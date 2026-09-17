package connections

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"cortex/internal/actions"
	"cortex/internal/agent"
	"cortex/internal/tools"
	"cortex/internal/tools/gmail"
	"cortex/internal/tools/jira"
	"cortex/internal/tools/notion"
)

// Per-source write enablement, and the two registries built from one
// connection's clients.
//
// The structural guarantee this file exists to provide: a run whose owner has
// not explicitly enabled writes for a source has NO write tool in its registry
// at all. Not a disabled one, not one that refuses — absent. The model cannot
// name a tool that is not in its tool list, so there is no prompt, however
// crafted, that produces a proposal against an account that never opted in.
//
// Demo mode gets the same guarantee for free and by a different route: a
// demo-mode run is handed the shared demo registry, which is assembled from read
// tools only and never sees a user's connection. The demo workspace is a real
// Jira site and a real mailbox belonging to a real person, and a stranger with a
// signed-in account must not be able to propose a write against it — let alone
// execute one.

// sourceClients holds the constructed clients for one connection, plus what the
// registries need to know about it.
type sourceClients struct {
	source string

	jira   *jira.Client
	notion *notion.Client
	gmail  *gmail.Client

	// gmailSender is the mailbox address mail would be sent from, read from the
	// connection's stored identity. It appears in the proposal payload and in
	// the approval copy, so a person always knows whose name is on the message.
	gmailSender string

	// writesEnabled is the connection's own flag, already reduced by any
	// source-specific precondition — a Gmail token without the send scope
	// arrives here as false.
	writesEnabled bool

	// allowedDomains restricts proposed email recipients; empty means any.
	allowedDomains []string
}

// tools returns the tool set for a run: the read tools always, the write tools
// only when this connection has writes enabled.
func (c *sourceClients) tools() []tools.Tool {
	switch c.source {
	case SourceJira:
		set := jira.NewTools(c.jira)
		if c.writesEnabled {
			set = append(set, jira.NewWriteTools(c.jira)...)
		}
		return set

	case SourceNotion:
		set := notion.NewTools(c.notion)
		if c.writesEnabled {
			set = append(set, notion.NewWriteTools(c.notion)...)
		}
		return set

	case SourceGmail:
		set := gmail.NewTools(c.gmail)
		if c.writesEnabled {
			set = append(set, gmail.NewWriteTools(c.gmail, c.gmailWriteConfig())...)
		}
		return set

	default:
		return nil
	}
}

// writers returns the executors for approved actions, or nothing when this
// connection has writes disabled.
//
// The same flag gates both halves on purpose. A connection whose writes were
// switched off after an approval was granted cannot execute it — the executor is
// simply not there — and the write worker reports that as a failed action rather
// than performing it. Revoking permission has to mean revoking it.
func (c *sourceClients) writers() []tools.Writer {
	if !c.writesEnabled {
		return nil
	}
	switch c.source {
	case SourceJira:
		return jira.NewWriters(c.jira)
	case SourceNotion:
		return notion.NewWriters(c.notion)
	case SourceGmail:
		return gmail.NewWriters(c.gmail, c.gmailWriteConfig())
	default:
		return nil
	}
}

// gmailWriteConfig is shared by the send tool and its executor so both enforce
// the same sender and the same allowlist.
func (c *sourceClients) gmailWriteConfig() gmail.WriteConfig {
	return gmail.WriteConfig{Sender: c.gmailSender, AllowedDomains: c.allowedDomains}
}

// WritersForUser builds the executors for one user's approved writes.
//
// The factory the write execution job is given. It returns an empty registry
// rather than an error when the user has no write-enabled connection: an
// approval whose capability has since been switched off is a failed action with
// a clear reason, not a job that retries forever.
func (b *RegistryBuilder) WritersForUser(ctx context.Context, userID uuid.UUID) (*actions.Registry, error) {
	infos, err := b.cfg.Service.List(ctx, userID)
	if err != nil {
		return nil, err
	}

	// Demo mode has no writers at all, checked before anything is built. A user
	// on the demo workspace has no connections of their own to execute against,
	// and the demo credentials are not theirs to write with.
	useDemo, err := b.cfg.Service.UseDemo(ctx, userID)
	if err != nil {
		return nil, err
	}
	// Same authority as ForUser: the builder's demo registry IS whether this
	// deployment has a demo workspace. Asking the service instead would be a
	// second source of truth for one fact, and the two disagreeing is a user
	// who reads as demo mode on one path and user mode on the other.
	if Mode(len(infos), useDemo, b.cfg.Demo != nil) == agent.ModeDemo {
		return actions.NewRegistry()
	}

	var writers []tools.Writer
	for _, info := range infos {
		if info.Status == "error" || !info.WritesEnabled {
			continue
		}
		clients, err := b.buildSource(ctx, userID, info)
		if err != nil {
			return nil, err
		}
		if clients == nil {
			continue
		}
		writers = append(writers, clients.writers()...)
	}
	return actions.NewRegistry(writers...)
}

// ErrWritesUnavailable marks a source that cannot have writes enabled as things
// stand, with a message written for the person reading it.
var ErrWritesUnavailable = errors.New("connections: writes unavailable")

// CheckWriteReadiness reports whether writes can be enabled for one source.
//
// Called before the flag is flipped, so a user is never told writes are on for a
// credential that cannot perform them. What "ready" means differs per source,
// and the differences are not arbitrary:
//
//   - Jira: an API token carries whatever its account can do, so nothing new is
//     granted by enabling writes — but the account may simply lack permission on
//     the site. That is checkable, so it is checked.
//   - Gmail: the token must carry the send scope, which only re-consent can add.
//   - Notion: no endpoint reports an integration's capabilities, so there is
//     nothing to check. Attempting a real write to find out would be the exact
//     unattended side effect the approval gate exists to prevent, so enablement
//     proceeds and a missing capability surfaces at execution — behind the gate,
//     as an error naming the checkbox to tick.
func (b *RegistryBuilder) CheckWriteReadiness(ctx context.Context, userID uuid.UUID, source string) error {
	// Loaded for its error only: this is what establishes the source is
	// connected at all before any per-source capability check runs.
	if _, err := b.connectionInfo(ctx, userID, source); err != nil {
		return err
	}

	switch source {
	case SourceJira:
		var creds JiraCredentials
		if err := b.load(ctx, userID, source, &creds); err != nil {
			return err
		}
		baseURL, err := ValidateJiraBaseURL(creds.BaseURL)
		if err != nil {
			return fmt.Errorf("%w: %s", ErrWritesUnavailable, err)
		}
		client, err := jira.NewClient(jira.Config{
			BaseURL:       baseURL,
			Email:         creds.Email,
			APIToken:      creds.APIToken,
			AllowUnscoped: true, // the user's own site; writes never touch the demo workspace
			Logger:        b.cfg.Logger,
		})
		if err != nil {
			return fmt.Errorf("%w: %s", ErrWritesUnavailable, err)
		}
		missing, err := client.MissingWritePermissions(ctx, "")
		if err != nil {
			// Unreachable right now, which is not the same as unauthorized.
			// Returned as a plain error so the handler answers "try again"
			// rather than "you lack permission".
			return fmt.Errorf("check jira write permissions: %w", err)
		}
		if len(missing) > 0 {
			return fmt.Errorf("%w: this Jira account is missing the permissions Cortex would need "+
				"(%s). Ask a site admin to grant them, then try again",
				ErrWritesUnavailable, strings.Join(missing, ", "))
		}
		return nil

	case SourceGmail:
		var creds GmailCredentials
		if err := b.load(ctx, userID, source, &creds); err != nil {
			return err
		}
		if !GmailCanSend(creds) {
			return fmt.Errorf("%w: sending mail needs permission this connection was not given. "+
				"Reconnect Gmail to grant it — Google will ask you specifically about sending mail "+
				"on your behalf", ErrWritesUnavailable)
		}
		return nil

	case SourceNotion:
		// Nothing to check; see the doc comment. The connection info was still
		// loaded above, which is what confirms the source is connected at all.
		return nil

	default:
		return fmt.Errorf("%w: unknown source %q", ErrWritesUnavailable, source)
	}
}

// connectionInfo finds one source in the user's connection list.
func (b *RegistryBuilder) connectionInfo(ctx context.Context, userID uuid.UUID, source string) (Info, error) {
	infos, err := b.cfg.Service.List(ctx, userID)
	if err != nil {
		return Info{}, err
	}
	for _, info := range infos {
		if info.Source == source {
			return info, nil
		}
	}
	return Info{}, ErrNoConnection
}

// identityEmail pulls the mailbox address out of a connection's stored identity.
//
// Tolerant of an absent or malformed value: the address is display copy, and an
// empty one renders as "this mailbox" rather than failing a run.
func identityEmail(identity json.RawMessage) string {
	if len(identity) == 0 {
		return ""
	}
	var fields struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(identity, &fields); err != nil {
		return ""
	}
	return strings.TrimSpace(fields.Email)
}
