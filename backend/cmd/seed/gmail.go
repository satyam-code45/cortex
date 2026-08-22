package main

// The Gmail half of the seeder.
//
// These fixtures are the third hop. Jira records that the refunds work was
// blocked on "the provider"; Notion names Nordwind Payments; only the mail says
// what Nordwind actually told us, and when. The dates are the point — which is
// why messages are inserted with their own Date header rather than sent, and
// why an undated fixture is rejected outright.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"cortex/internal/config"
	"cortex/internal/tools/gmail"
)

// gmailSeedTimeout bounds the Gmail seed.
const gmailSeedTimeout = 10 * time.Minute

// mePlaceholder is substituted with the authenticated mailbox address.
//
// The fixtures are committed to a public repository, so no real address goes in
// them; and substituting at seed time means the same fixtures work for whoever
// runs the seeder.
const mePlaceholder = "{{me}}"

// gmailFile is the shape of gmail.json.
type gmailFile struct {
	Note  string `json:"note"`
	Label string `json:"label"`
	// Probe is a plain query the seeder runs after writing, to prove the
	// fixtures can be found the way the agent will look for them.
	Probe    string             `json:"probe"`
	Messages []gmailMessageSpec `json:"messages"`
}

// gmailMessageSpec is one fixture email.
type gmailMessageSpec struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Cc      string `json:"cc"`
	Subject string `json:"subject"`
	// Date is RFC 3339. It becomes the message's Date header and, via
	// internalDateSource=dateHeader, Gmail's own timestamp for it.
	Date string `json:"date"`
	Body string `json:"body"`
}

func runGmail(logger *slog.Logger, confirm bool, fixturesDir string) error {
	dir, err := resolveFixturesDir(fixturesDir)
	if err != nil {
		return err
	}
	var file gmailFile
	if err := readJSON(filepath.Join(dir, "gmail.json"), &file); err != nil {
		return err
	}
	if len(file.Messages) == 0 {
		return fmt.Errorf("gmail.json contains no messages")
	}

	// Parsed up front so a malformed date fails before anything is written,
	// rather than half way through the mailbox.
	dates := make([]time.Time, len(file.Messages))
	for i, spec := range file.Messages {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(spec.Date))
		if err != nil {
			return fmt.Errorf("message %q has an unparseable date %q (want RFC 3339, e.g. 2026-06-08T08:47:00+02:00): %w",
				spec.Subject, spec.Date, err)
		}
		dates[i] = parsed
	}
	logger.Info("gmail fixtures loaded", "dir", dir, "messages", len(file.Messages), "label", file.Label)

	if !confirm {
		reportGmailPlan(file, dates)
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	client, err := buildGmailClient(cfg, logger)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, gmailSeedTimeout)
	defer cancel()

	me, err := client.UserEmail(ctx)
	if err != nil {
		return err
	}
	logger.Info("authenticated to gmail", "mailbox", me)

	fixtureLabelID := ""
	if strings.TrimSpace(file.Label) != "" {
		id, err := client.EnsureLabel(ctx, file.Label)
		if err != nil {
			return err
		}
		fixtureLabelID = id
		logger.Info("fixture label ready", "name", file.Label, "id", id)
	}
	// INBOX is included unconditionally. Without it a fixture is in the mailbox
	// but unreachable by any ordinary search, which is what made the Day 3
	// acceptance test fail with all sixteen messages present and correct.
	labelIDs := gmail.FixtureLabelIDs(fixtureLabelID)

	var inserted, skipped int
	var unreachable []string
	for i, spec := range file.Messages {
		if ctx.Err() != nil {
			return fmt.Errorf("gmail seed interrupted after %d messages: %w", inserted, ctx.Err())
		}

		existing, exists, err := client.FindBySubject(ctx, spec.Subject)
		if err != nil {
			return err
		}
		if exists {
			skipped++
			if !existing.InInbox {
				// Seeded before the INBOX fix. Re-inserting would duplicate it
				// and repairing it needs a write scope the token deliberately
				// does not carry, so it is reported rather than silently skipped
				// — a fixture the agent cannot find is worse than none.
				unreachable = append(unreachable, spec.Subject)
			}
			logger.Debug("message already seeded, skipping",
				"id", existing.ID, "subject", spec.Subject, "in_inbox", existing.InInbox)
			continue
		}

		id, err := client.InsertMessage(ctx, gmail.FixtureMessage{
			From:    spec.From,
			To:      substituteMe(spec.To, me),
			Cc:      substituteMe(spec.Cc, me),
			Subject: spec.Subject,
			Date:    dates[i],
			Body:    spec.Body,
		}, labelIDs)
		if err != nil {
			return fmt.Errorf("insert %q: %w", spec.Subject, err)
		}
		inserted++
		logger.Info("message inserted", "id", id, "date", dates[i].Format("2006-01-02"), "subject", spec.Subject)
	}

	logger.Info("gmail seed complete", "inserted", inserted, "skipped", skipped)

	if len(unreachable) > 0 {
		// Worth saying, not worth failing over. These predate the INBOX fix and
		// cannot be repaired from here — the token carries no write scope beyond
		// insert — but the probe below decides whether that actually matters.
		logger.Warn("some fixtures predate the INBOX fix and are not in the inbox; "+
			"they may still be searchable, in which case this is cosmetic",
			"count", len(unreachable), "label", file.Label)
	}

	return verifyReachable(ctx, client, logger, file)
}

// verifyReachable checks that a seeded fixture can be found the way the agent
// will look for it.
//
// This is the invariant BUG-3.A was really about, and the one no unit test could
// have caught: the fixtures were present, correct, and correctly dated, and the
// agent still could not see them. It is checked with an ordinary query — no
// in:anywhere, no includeSpamTrash — because an ordinary query is all the agent
// has.
//
// Gmail does not index an inserted message immediately, so an empty probe
// moments after a seed may simply be early. The failure says so rather than
// asserting the data is wrong.
func verifyReachable(ctx context.Context, client *gmail.Client, logger *slog.Logger, file gmailFile) error {
	probe := strings.TrimSpace(file.Probe)
	if probe == "" {
		return nil
	}

	count, err := client.CountMatching(ctx, probe)
	if err != nil {
		return fmt.Errorf("verify fixtures are searchable: %w", err)
	}
	if count == 0 {
		return fmt.Errorf("seeded fixtures are not searchable: a plain query for %q returns nothing.\n"+
			"They may still be indexing — Gmail does not make an inserted message searchable "+
			"immediately. Wait a minute and re-run `make seed-gmail`; it re-probes without "+
			"inserting anything.\n"+
			"If it stays at zero the agent cannot reach the email hop, and the Day 3 acceptance "+
			"test will fail however it phrases its search.", probe)
	}
	logger.Info("fixtures verified searchable by an ordinary query", "probe", probe, "matches", count)
	return nil
}

// buildGmailClient wires the cached refresh token into a Gmail client.
func buildGmailClient(cfg *config.Config, logger *slog.Logger) (*gmail.Client, error) {
	creds, err := gmail.LoadCredentials(cfg.GmailCredentialsPath)
	if err != nil {
		return nil, err
	}
	token, err := gmail.LoadToken(cfg.GmailTokenPath)
	if err != nil {
		return nil, err
	}
	source, err := gmail.NewTokenSource(gmail.TokenSourceConfig{
		Credentials: creds,
		Token:       token,
		TokenPath:   cfg.GmailTokenPath,
	})
	if err != nil {
		return nil, err
	}
	return gmail.NewClient(gmail.Config{
		TokenSource: source,
		// Deliberately unscoped: the duplicate check has to see the whole
		// mailbox. Scoping it to the fixture label would let a fixture that was
		// seeded before the label existed be inserted a second time.
		Logger: logger,
	})
}

// substituteMe replaces the committed placeholder with the real mailbox.
func substituteMe(address, me string) string {
	return strings.ReplaceAll(address, mePlaceholder, me)
}

// reportGmailPlan prints what a real run would do, without writing anything.
func reportGmailPlan(file gmailFile, dates []time.Time) {
	fmt.Println("PLAN ONLY — no requests will be sent to Gmail. Re-run with --confirm to apply.")
	fmt.Println()
	if file.Label != "" {
		fmt.Printf("Label applied to every fixture: %s (created if absent)\n\n", file.Label)
	}
	fmt.Println("Messages to insert, in date order as they will appear in the mailbox:")

	order := make([]int, len(file.Messages))
	for i := range order {
		order[i] = i
	}
	// Sorted for the plan only; insertion order does not matter, because each
	// message carries its own date.
	slices.SortStableFunc(order, func(a, b int) int { return dates[a].Compare(dates[b]) })
	for _, i := range order {
		spec := file.Messages[i]
		fmt.Printf("  %s  %-46s %s\n",
			dates[i].Format("2006-01-02"), truncate(spec.Subject, 46), senderName(spec.From))
	}

	fmt.Println()
	fmt.Printf("Total: %d messages, inserted (not sent) so each keeps the date above.\n", len(file.Messages))
	fmt.Println("A message whose exact subject is already in the mailbox is skipped, so re-running is safe.")
	fmt.Println("To apply:  make seed-gmail")
}

// senderName renders just the display name of an address, for the plan.
func senderName(address string) string {
	if open := strings.Index(address, "<"); open > 0 {
		return strings.TrimSpace(address[:open])
	}
	return address
}

// truncate shortens a string for column output.
func truncate(text string, n int) string {
	runes := []rune(text)
	if len(runes) <= n {
		return text
	}
	return string(runes[:n-1]) + "…"
}
