// Command seed loads the committed Vantage Labs fixtures into Jira.
//
// The fixtures in seeder/fixtures/ are generated once and committed, so seeding
// is deterministic — the same run produces the same company every time, and the
// eval ground truth in evals/ground_truth.json stays true.
//
// The interesting constraint is that Jira history cannot be fabricated. The REST
// API will not backdate `created` and will not accept injected changelog rows, so
// a fixture that merely *states* "the due date moved from June to August"
// produces an issue with an empty changelog, and jira_get_issue_history returns
// nothing. The only way the agent can discover that a deadline moved is for this
// command to actually move it. So each issue is built up in order:
//
//	create (with its original due date)
//	  → post each comment
//	  → PUT each subsequent due date, one call per recorded change
//	  → POST each status transition, one at a time
//
// Every one of those mutations leaves a real trace the agent later finds. It also
// means ~450 requests for 99 issues, which is why the client throttles and
// retries.
//
// Re-running is safe: an issue whose exact summary already exists is skipped, so
// an interrupted seed resumes instead of duplicating.
//
// Writing requires --confirm. Planning is the default because the write path
// makes several hundred irreversible mutations to a live Atlassian account, and
// there is no undo short of deleting the projects.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"cortex/internal/config"
	"cortex/internal/tools/jira"
)

// seedTimeout bounds the whole seed. ~450 throttled requests plus retries needs
// real headroom.
const seedTimeout = 30 * time.Minute

// fixtureCandidates are the paths tried when --fixtures is not given. `make
// seed` runs from backend/, but running the command from the repo root is the
// obvious thing to try by hand.
var fixtureCandidates = []string{"../seeder/fixtures", "seeder/fixtures"}

// person is one of the synthetic employees. Only the name is load-bearing: it is
// what appears in issue descriptions, comment attributions, and owner labels.
type person struct {
	Name string `json:"name"`
	Role string `json:"role"`
	Team string `json:"team"`
}

type project struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Lead        string `json:"lead"`
}

// companyFile is the shape of company.json.
type companyFile struct {
	Company     string    `json:"company"`
	Description string    `json:"description"`
	People      []person  `json:"people"`
	Projects    []project `json:"projects"`
}

// issueFixture is one issue to seed, with the history to replay onto it.
type issueFixture struct {
	Project     string           `json:"project"`
	Type        string           `json:"type"`
	Summary     string           `json:"summary"`
	Description string           `json:"description"`
	Owner       string           `json:"owner"`
	Priority    string           `json:"priority"`
	Labels      []string         `json:"labels"`
	DueDate     string           `json:"due_date"`
	DueChanges  []string         `json:"due_date_changes"`
	Transitions []string         `json:"transitions"`
	Comments    []commentFixture `json:"comments"`
}

type commentFixture struct {
	Author string `json:"author"`
	Body   string `json:"body"`
}

// mutations counts the API calls this fixture will make, for the dry run.
func (f issueFixture) mutations() int {
	// 1 create + one per comment + one per due-date change + two per transition
	// (the available transitions have to be read before each move, because their
	// ids are per-project and depend on the current status).
	return 1 + len(f.Comments) + len(f.DueChanges) + 2*len(f.Transitions)
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	confirm := flag.Bool("confirm", false,
		"actually write to Jira; without this the command only prints the plan")
	fixturesDir := flag.String("fixtures", "",
		"directory holding company.json and issues.json (default: ../seeder/fixtures)")
	only := flag.String("project", "",
		"seed only this project key, e.g. ATLAS")
	limit := flag.Int("limit", 0,
		"seed at most this many issues (0 means all); useful for a first smoke test")
	flag.Parse()

	if err := run(logger, *confirm, *fixturesDir, *only, *limit); err != nil {
		logger.Error("seed failed", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger, confirm bool, fixturesDir, only string, limit int) error {
	dir, err := resolveFixturesDir(fixturesDir)
	if err != nil {
		return err
	}

	var co companyFile
	if err := readJSON(filepath.Join(dir, "company.json"), &co); err != nil {
		return err
	}
	var fixtures []issueFixture
	if err := readJSON(filepath.Join(dir, "issues.json"), &fixtures); err != nil {
		return err
	}

	projects := co.Projects
	if only != "" {
		only = strings.ToUpper(only)
		projects = filterProjects(projects, only)
		if len(projects) == 0 {
			return fmt.Errorf("no project %q in company.json", only)
		}
		fixtures = filterIssues(fixtures, only)
	}
	if limit > 0 && limit < len(fixtures) {
		fixtures = fixtures[:limit]
	}

	logger.Info("fixtures loaded",
		"dir", dir, "company", co.Company, "people", len(co.People),
		"projects", len(projects), "issues", len(fixtures))

	if !confirm {
		reportPlan(logger, projects, fixtures)
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	client, err := jira.NewClient(jira.Config{
		BaseURL:  cfg.JiraBaseURL,
		Email:    cfg.JiraEmail,
		APIToken: cfg.JiraAPIToken,
		Logger:   logger,
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, seedTimeout)
	defer cancel()

	accountID, displayName, err := client.CurrentUser(ctx)
	if err != nil {
		return err
	}
	logger.Info("authenticated to jira", "account", displayName, "base_url", cfg.JiraBaseURL)

	s := &seeder{client: client, logger: logger, leadAccountID: accountID}

	if err := s.ensureProjects(ctx, projects); err != nil {
		return err
	}
	if err := s.loadExisting(ctx, projects); err != nil {
		return err
	}
	return s.seedIssues(ctx, fixtures)
}

// seeder holds the state shared across the seed.
type seeder struct {
	client        *jira.Client
	logger        *slog.Logger
	leadAccountID string

	// existing maps a summary to the key of an issue already in Jira. It is the
	// idempotency check: one search per project up front rather than a lookup
	// per issue, because JQL `summary ~ "..."` is a fuzzy text match and would
	// be both 99 extra requests and wrong.
	existing map[string]string

	created int
	skipped int
	failed  int

	// partialKey is set by seedIssue as soon as the issue exists, so a failure
	// during history replay can name what was left behind.
	partialKey string
	// partial collects the keys of issues that exist with incomplete history.
	partial []string
}

// ensureProjects creates any project that does not exist yet.
//
// A project that is already there is left completely alone. That matters beyond
// idempotency: the site has a pre-existing project (KAN) that is not ours, and
// nothing here may touch it.
func (s *seeder) ensureProjects(ctx context.Context, projects []project) error {
	for _, p := range projects {
		found, err := s.client.GetProject(ctx, p.Key)
		if err != nil {
			return err
		}
		if found != nil {
			s.logger.Info("project already exists", "key", found.Key, "name", found.Name)
			continue
		}
		created, err := s.client.CreateProject(ctx, jira.ProjectSpec{
			Key:           p.Key,
			Name:          p.Name,
			Description:   p.Description,
			LeadAccountID: s.leadAccountID,
		})
		if err != nil {
			return err
		}
		// The name comes from the fixture, not from `created`: POST
		// /rest/api/3/project echoes back only self/id/key, so reading it off the
		// response logs an empty name for a project that was created correctly.
		s.logger.Info("project created", "key", created.Key, "name", p.Name)
	}
	return nil
}

// loadExisting reads the summaries already present in each project.
func (s *seeder) loadExisting(ctx context.Context, projects []project) error {
	s.existing = make(map[string]string)
	for _, p := range projects {
		summaries, err := s.client.ListIssueSummaries(ctx, p.Key)
		if err != nil {
			return err
		}
		for summary, key := range summaries {
			s.existing[summary] = key
		}
		s.logger.Info("existing issues read", "project", p.Key, "count", len(summaries))
	}
	return nil
}

// seedIssues creates each missing issue and replays its history.
//
// A failure on one issue is logged and the seed continues. Aborting the whole
// run because issue 58 of 99 hit a transient Jira error would waste the 57
// before it — and since the seed is idempotent, the fix is simply to run it
// again.
func (s *seeder) seedIssues(ctx context.Context, fixtures []issueFixture) error {
	for i, f := range fixtures {
		if ctx.Err() != nil {
			return fmt.Errorf("seed interrupted after %d issues: %w", i, ctx.Err())
		}
		if key, exists := s.existing[f.Summary]; exists {
			s.skipped++
			s.logger.Debug("issue already seeded, skipping", "key", key, "summary", f.Summary)
			continue
		}
		if err := s.seedIssue(ctx, f); err != nil {
			s.failed++
			// A partial failure needs the issue key in the log, because it is NOT
			// self-healing. If CreateIssue succeeded and a later comment or
			// transition failed, the issue now exists with incomplete history — and
			// the next run's existence check will skip it, so re-running silently
			// reports success over a ticket that is missing the very blocker
			// discussion the evals assert. Naming the key is what lets an operator
			// delete it and re-run.
			s.logger.Error("failed to seed issue",
				"project", f.Project, "summary", f.Summary,
				"partial_key", s.partialKey, "error", err)
			if s.partialKey != "" {
				s.partial = append(s.partial, s.partialKey)
			}
			continue
		}
		s.created++
		if s.created%10 == 0 {
			s.logger.Info("progress", "created", s.created, "of", len(fixtures))
		}
	}

	s.logger.Info("seed complete",
		"created", s.created, "skipped", s.skipped, "failed", s.failed)
	if len(s.partial) > 0 {
		// Deliberately explicit rather than "just re-run": these issues exist, so
		// a re-run skips them and looks clean while the data stays incomplete.
		s.logger.Error("issues exist with INCOMPLETE history and will be skipped on a re-run; "+
			"delete them in Jira, then re-run to rebuild them",
			"keys", strings.Join(s.partial, ", "))
	}
	if s.failed > 0 {
		return fmt.Errorf("%d issue(s) failed to seed; issues that were never created are "+
			"retried on a re-run, but any listed above as incomplete must be deleted first",
			s.failed)
	}
	return nil
}

// seedIssue creates one issue and replays its history onto it, in order.
func (s *seeder) seedIssue(ctx context.Context, f issueFixture) error {
	s.partialKey = ""
	key, err := s.client.CreateIssue(ctx, jira.IssueSpec{
		ProjectKey:  f.Project,
		Summary:     f.Summary,
		Description: f.Description,
		IssueType:   f.Type,
		Priority:    f.Priority,
		Labels:      ownerLabels(f),
		DueDate:     f.DueDate,
	})
	if err != nil {
		return err
	}
	// From here on the issue exists, so any later failure leaves it partial.
	s.partialKey = key

	// Comments before the field changes, so the discussion reads as the reason
	// the dates moved rather than as an afterthought.
	for _, comment := range f.Comments {
		// Every comment is posted by the single account the API token owns, so
		// the intended author is named in the body. That prefix is what the
		// agent actually reads to attribute a statement to a person.
		body := fmt.Sprintf("%s: %s", comment.Author, comment.Body)
		if err := s.client.AddComment(ctx, key, body); err != nil {
			return fmt.Errorf("comment on %s: %w", key, err)
		}
	}

	// One PUT per recorded change: each is a changelog row, and those rows are
	// the only evidence that a deadline ever moved.
	for _, due := range f.DueChanges {
		if err := s.client.SetDueDate(ctx, key, due); err != nil {
			return fmt.Errorf("due date change on %s: %w", key, err)
		}
	}

	for _, status := range f.Transitions {
		if err := s.client.TransitionToStatus(ctx, key, status); err != nil {
			return fmt.Errorf("transition %s to %q: %w", key, status, err)
		}
	}

	s.existing[f.Summary] = key
	s.partialKey = ""
	s.logger.Debug("issue seeded",
		"key", key, "comments", len(f.Comments),
		"due_changes", len(f.DueChanges), "transitions", len(f.Transitions))
	return nil
}

// ownerLabels adds the intended owner as a label alongside the fixture's own.
//
// The free site has exactly one real user, so `assignee` cannot carry the owner.
// Recording it as a label makes it filterable in JQL, and the description names
// the person in prose — which is what the agent reads.
func ownerLabels(f issueFixture) []string {
	labels := make([]string, 0, len(f.Labels)+1)
	labels = append(labels, f.Labels...)
	if f.Owner != "" {
		labels = append(labels, "owner-"+f.Owner)
	}
	return labels
}

// reportPlan prints what a real run would do, without writing anything.
func reportPlan(logger *slog.Logger, projects []project, fixtures []issueFixture) {
	perProject := make(map[string]int, len(projects))
	perProjectCalls := make(map[string]int, len(projects))
	total := 0
	for _, f := range fixtures {
		perProject[f.Project]++
		perProjectCalls[f.Project] += f.mutations()
		total += f.mutations()
	}

	fmt.Println("PLAN ONLY — no requests will be sent to Jira. Re-run with --confirm to apply.")
	fmt.Println()
	fmt.Println("Projects to create if absent:")
	for _, p := range projects {
		fmt.Printf("  %-8s %s\n", p.Key, p.Name)
	}
	fmt.Println()
	fmt.Println("Issues to seed:")
	for _, p := range projects {
		fmt.Printf("  %-8s %3d issues, ~%d API calls\n",
			p.Key, perProject[p.Key], perProjectCalls[p.Key])
	}
	fmt.Println()
	fmt.Printf("Total: %d issues, ~%d API calls (plus %d project lookups and %d existence searches).\n",
		len(fixtures), total, len(projects), len(projects))
	fmt.Printf("At the client's %s minimum spacing that is roughly %s of wall clock.\n",
		jira.DefaultMinInterval, (time.Duration(total) * jira.DefaultMinInterval).Round(time.Second))
	fmt.Println()
	fmt.Println("A real run skips any issue whose exact summary already exists, so re-running is safe.")
	fmt.Println("To apply:  make seed        (which passes --confirm)")

	// A handful of concrete examples: a plan that only shows totals does not let
	// you check that the history is actually going to be replayed.
	fmt.Println()
	fmt.Println("Sample of the history that will be replayed:")
	shown := 0
	for _, f := range fixtures {
		if len(f.Comments) == 0 && len(f.DueChanges) == 0 {
			continue
		}
		fmt.Printf("  [%s] %s\n", f.Project, f.Summary)
		fmt.Printf("      create due=%s, then %d comment(s), %d due-date change(s) %v, %d transition(s) %v\n",
			orNone(f.DueDate), len(f.Comments), len(f.DueChanges), f.DueChanges,
			len(f.Transitions), f.Transitions)
		shown++
		if shown == 5 {
			break
		}
	}
	logger.Info("plan complete; nothing was written", "issues", len(fixtures), "api_calls", total)
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// resolveFixturesDir finds the fixtures directory.
func resolveFixturesDir(given string) (string, error) {
	if given != "" {
		if !isFixturesDir(given) {
			return "", fmt.Errorf("%s does not contain company.json and issues.json", given)
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

func isFixturesDir(dir string) bool {
	for _, name := range []string{"company.json", "issues.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return false
		}
	}
	return true
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

func filterProjects(projects []project, key string) []project {
	var out []project
	for _, p := range projects {
		if p.Key == key {
			out = append(out, p)
		}
	}
	return out
}

func filterIssues(fixtures []issueFixture, key string) []issueFixture {
	var out []issueFixture
	for _, f := range fixtures {
		if f.Project == key {
			out = append(out, f)
		}
	}
	return out
}
