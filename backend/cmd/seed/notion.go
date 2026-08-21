package main

// The Notion half of the seeder.
//
// Notion pages carry what Jira deliberately does not: the vendor's name, the
// contacts, the plan the dates were promised against, and the retro that says
// the delay notice arrived by email. Without these pages the multi-hop chain
// has no middle.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"cortex/internal/config"
	"cortex/internal/tools/notion"
)

// notionSeedTimeout bounds the Notion seed. Each page is one create plus a
// handful of throttled appends, so this is generous.
const notionSeedTimeout = 10 * time.Minute

// notionFile is the shape of notion.json.
type notionFile struct {
	Note  string           `json:"note"`
	Pages []notionPageSpec `json:"pages"`
}

// notionPageSpec is one page to create, its body written in the markdown
// subset internal/tools/notion understands.
type notionPageSpec struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

func runNotion(logger *slog.Logger, confirm bool, fixturesDir string, replace bool) error {
	dir, err := resolveFixturesDir(fixturesDir)
	if err != nil {
		return err
	}
	var file notionFile
	if err := readJSON(filepath.Join(dir, "notion.json"), &file); err != nil {
		return err
	}
	if len(file.Pages) == 0 {
		return fmt.Errorf("notion.json contains no pages")
	}
	logger.Info("notion fixtures loaded", "dir", dir, "pages", len(file.Pages))

	if !confirm {
		reportNotionPlan(file.Pages, replace)
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	client, err := notion.NewClient(notion.Config{Token: cfg.NotionToken, Logger: logger})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, notionSeedTimeout)
	defer cancel()

	parentID, err := resolveParentPage(ctx, client, cfg.NotionParentPageID)
	if err != nil {
		return err
	}
	parentTitle, err := client.PageTitle(ctx, parentID)
	if err != nil {
		return err
	}
	logger.Info("seeding under notion page", "title", parentTitle, "id", parentID)

	var created, updated, skipped int
	for _, spec := range file.Pages {
		if ctx.Err() != nil {
			return fmt.Errorf("notion seed interrupted: %w", ctx.Err())
		}

		existingID, exists, err := client.FindChildPage(ctx, parentID, spec.Title)
		if err != nil {
			return err
		}
		switch {
		case exists && !replace:
			skipped++
			logger.Info("page already exists, leaving it alone", "title", spec.Title, "id", existingID)
		case exists:
			if err := client.ReplacePageContent(ctx, existingID, spec.Body); err != nil {
				return fmt.Errorf("replace %q: %w", spec.Title, err)
			}
			updated++
			logger.Info("page contents replaced", "title", spec.Title, "id", existingID)
		default:
			id, pageURL, err := client.CreatePage(ctx, parentID, spec.Title, spec.Body)
			if err != nil {
				return fmt.Errorf("create %q: %w", spec.Title, err)
			}
			created++
			logger.Info("page created", "title", spec.Title, "id", id, "url", pageURL)
		}
	}

	logger.Info("notion seed complete", "created", created, "updated", updated, "skipped", skipped)
	if skipped > 0 && !replace {
		// Said explicitly because it is the case that looks clean and is not:
		// a fixture edited after the first seed is silently not applied.
		logger.Info("pages that already existed were left unchanged; " +
			"re-run with --replace to rewrite them from the fixtures")
	}
	return nil
}

// resolveParentPage decides which page the fixtures are created under.
//
// Configuration wins. Falling back to discovery is what makes the very first
// run work with nothing but a token: a fresh integration can see exactly the
// pages it has been shared with, and in the intended setup that is one page.
// Guessing between several would be worse than refusing.
func resolveParentPage(ctx context.Context, client *notion.Client, configured string) (string, error) {
	if strings.TrimSpace(configured) != "" {
		return configured, nil
	}

	ids, titles, err := client.SharedPageIDs(ctx, 10)
	if err != nil {
		return "", err
	}
	switch len(ids) {
	case 0:
		return "", fmt.Errorf("no Notion pages are shared with this integration; " +
			"open the page you want the fixtures under, use Share to add the integration, then re-run")
	case 1:
		return ids[0], nil
	default:
		var b strings.Builder
		b.WriteString("several Notion pages are shared with this integration, so the parent is ambiguous; " +
			"set NOTION_PARENT_PAGE_ID to one of:\n")
		for i, id := range ids {
			fmt.Fprintf(&b, "  %s  %s\n", id, titles[i])
		}
		return "", fmt.Errorf("%s", b.String())
	}
}

// reportNotionPlan prints what a real run would do, without writing anything.
func reportNotionPlan(pages []notionPageSpec, replace bool) {
	fmt.Println("PLAN ONLY — no requests will be sent to Notion. Re-run with --confirm to apply.")
	fmt.Println()
	fmt.Println("Pages to create under the shared parent page:")
	for _, spec := range pages {
		blocks := notion.BlockCount(spec.Body)
		fmt.Printf("  %-32s %3d blocks, %d characters\n", spec.Title, blocks, len(spec.Body))
	}
	fmt.Println()
	if replace {
		fmt.Println("--replace is set: a page that already exists will have its contents rewritten.")
	} else {
		fmt.Println("A page that already exists will be left alone. Pass --replace to rewrite it.")
	}
	fmt.Println("To apply:  make seed-notion")
}
