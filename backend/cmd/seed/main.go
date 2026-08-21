// Command seed loads the committed Vantage Labs fixtures into Jira, Notion, and
// Gmail.
//
// The fixtures in seeder/fixtures/ are generated once and committed, so seeding
// is deterministic — the same run produces the same company every time, and the
// eval ground truth in evals/ground_truth.json stays true.
//
// Three targets, one safety contract. `--target jira|notion|gmail|all` selects
// what to seed; planning is the default and `--confirm` is required to write,
// because every target makes irreversible changes to a live account and there
// is no undo short of deleting things by hand.
//
// What each target is for, and why it works the way it does:
//
//   - jira — projects, issues, and their history. Jira history cannot be
//     fabricated: the REST API will not backdate `created` and will not accept
//     injected changelog rows, so a fixture that merely *states* "the due date
//     moved from June to August" produces an issue with an empty changelog. The
//     only way the agent can discover that a deadline moved is for this command
//     to actually move it, one call per recorded change.
//
//   - notion — the plan, roadmap, retro, and meeting-note pages. These carry the
//     facts Jira deliberately omits, above all the name of the payments vendor.
//
//   - gmail — ~15 fixture emails, inserted rather than sent so each one keeps
//     its own date. The vendor's delay notice has to sit in June, before the
//     launch-date decision it caused; sending would stamp them all with today.
//
// Together they are what makes a question multi-hop: the Jira ticket says
// "blocked on the provider", the Notion plan names Nordwind Payments, and only
// the email says why and when.
//
// Re-running any target is safe. Jira skips an issue whose exact summary
// already exists, Notion leaves a page it already created alone unless
// --replace is given, and Gmail skips a message whose exact subject is already
// in the mailbox.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"cortex/internal/config"
	"cortex/internal/tools/gmail"
)

// seedTarget names one seedable system.
type seedTarget string

const (
	targetJira   seedTarget = "jira"
	targetNotion seedTarget = "notion"
	targetGmail  seedTarget = "gmail"
	targetAll    seedTarget = "all"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	confirm := flag.Bool("confirm", false,
		"actually write; without this the command only prints the plan")
	target := flag.String("target", string(targetAll),
		"what to seed: jira, notion, gmail, or all")
	fixturesDir := flag.String("fixtures", "",
		"directory holding the fixture files (default: ../seeder/fixtures)")
	only := flag.String("project", "",
		"jira only: seed just this project key, e.g. ATLAS")
	limit := flag.Int("limit", 0,
		"jira only: seed at most this many issues (0 means all); useful for a first smoke test")
	replace := flag.Bool("replace", false,
		"notion only: rewrite the contents of pages that already exist, instead of leaving them alone")
	flag.Parse()

	err := run(logger, seedTarget(strings.ToLower(*target)), *confirm, *fixturesDir, *only, *limit, *replace)
	if err != nil {
		logger.Error("seed failed", "error", err)
		os.Exit(1)
	}
}

func run(
	logger *slog.Logger,
	target seedTarget,
	confirm bool,
	fixturesDir, only string,
	limit int,
	replace bool,
) error {
	switch target {
	case targetJira, targetNotion, targetGmail, targetAll:
	default:
		return fmt.Errorf("unknown --target %q; expected jira, notion, gmail, or all", target)
	}

	// Gmail's credentials are checked before ANY target runs. They are the one
	// input that configuration cannot supply — cmd/gmail-auth has to have been
	// run — and the seed order is jira → notion → gmail. Discovering the missing
	// token last would mean ~450 irreversible Jira mutations and five Notion
	// pages, then a hard stop; the operator fixes it and re-runs into a
	// half-seeded account.
	if target == targetGmail || target == targetAll {
		if confirm {
			if err := checkGmailCredentials(); err != nil {
				return err
			}
		}
	}

	if target == targetJira || target == targetAll {
		if err := runJira(logger, confirm, fixturesDir, only, limit); err != nil {
			return err
		}
	}
	if target == targetNotion || target == targetAll {
		if err := runNotion(logger, confirm, fixturesDir, replace); err != nil {
			return err
		}
	}
	if target == targetGmail || target == targetAll {
		if err := runGmail(logger, confirm, fixturesDir); err != nil {
			return err
		}
	}
	return nil
}

// checkGmailCredentials verifies the one-time OAuth flow has been run, without
// making a request.
func checkGmailCredentials() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if _, err := gmail.LoadCredentials(cfg.GmailCredentialsPath); err != nil {
		return err
	}
	// The error already names `make gmail-auth`.
	if _, err := gmail.LoadToken(cfg.GmailTokenPath); err != nil {
		return err
	}
	return nil
}

// fixtureCandidates are the paths tried when --fixtures is not given. `make
// seed` runs from backend/, but running the command from the repo root is the
// obvious thing to try by hand.
var fixtureCandidates = []string{"../seeder/fixtures", "seeder/fixtures"}

// resolveFixturesDir finds the fixtures directory.
func resolveFixturesDir(given string) (string, error) {
	if given != "" {
		if !isFixturesDir(given) {
			return "", fmt.Errorf("%s contains none of company.json, issues.json, notion.json or gmail.json", given)
		}
		return given, nil
	}
	for _, candidate := range fixtureCandidates {
		if isFixturesDir(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("could not find the fixtures directory (tried %s); pass --fixtures",
		strings.Join(fixtureCandidates, ", "))
}

// isFixturesDir reports whether dir holds any fixture file we recognize.
//
// Deliberately not "holds all of them": seeding only Notion on a checkout
// without the Jira fixtures is a legitimate thing to do, and each target
// reports its own missing file with a path when it tries to read it.
func isFixturesDir(dir string) bool {
	for _, name := range []string{"company.json", "issues.json", "notion.json", "gmail.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

// readJSON decodes a fixture file, rejecting unknown fields so a typo in a
// fixture key fails loudly instead of silently seeding an issue with no history.
func readJSON(path string, out any) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close() //nolint:errcheck // read-only

	dec := json.NewDecoder(file)
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
