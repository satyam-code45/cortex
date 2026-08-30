package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"cortex/internal/llm"
	"cortex/internal/store"
	"cortex/internal/tools"
)

const (
	// toolName is the identifier the model calls.
	toolName = "search_knowledge_base"

	// DefaultSearchLimit is how many chunks a search returns.
	//
	// Eight is a compromise the agent loop forces: every result is re-sent on
	// every later iteration, so a generous top-k is paid for many times over.
	// Eight is enough for the relevant passage to be in there when the query is
	// roughly right, and small enough that a wrong query is cheap.
	DefaultSearchLimit = 8

	// searchTimeout bounds the embedding call plus the query.
	searchTimeout = 30 * time.Second
)

// SearchConfig configures the knowledge base tool.
type SearchConfig struct {
	DB       store.DBTX
	Embedder Embedder
	// Sources are the source names the tool advertises in its schema. Naming
	// them lets the model filter without guessing.
	Sources []string
	// Limit is the top-k. Zero uses DefaultSearchLimit.
	Limit  int
	Logger *slog.Logger
}

// searchTool is the tools.Tool implementation.
type searchTool struct {
	db       store.DBTX
	embedder Embedder
	sources  []string
	limit    int
	logger   *slog.Logger
}

var _ tools.Tool = (*searchTool)(nil)

// NewSearchTool builds the knowledge base search tool.
func NewSearchTool(cfg SearchConfig) (tools.Tool, error) {
	if cfg.DB == nil {
		return nil, errors.New("rag: DB is required for the knowledge base tool")
	}
	if cfg.Embedder == nil {
		return nil, errors.New("rag: Embedder is required for the knowledge base tool")
	}
	t := &searchTool{
		db:       cfg.DB,
		embedder: cfg.Embedder,
		sources:  cfg.Sources,
		limit:    cfg.Limit,
		logger:   cfg.Logger,
	}
	if t.limit <= 0 {
		t.limit = DefaultSearchLimit
	}
	if t.logger == nil {
		t.logger = slog.Default()
	}
	return t, nil
}

func (t *searchTool) Name() string { return toolName }

func (t *searchTool) Description() string {
	return "Semantic search over the indexed archive of the demo workspace's Jira issues and their " +
		"comments, Notion pages, and email — it does not index user-connected sources. " +
		"Use it when you do not know which document holds what you need, when a keyword " +
		"search has come back empty, or when the question is about what was written, argued or " +
		"decided rather than about current state. It matches meaning, not exact words, so ask it in " +
		"the words of the question. The index is a snapshot: never use it for a status, an assignee, " +
		"or a date that could have changed — read those from the live source."
}

func (t *searchTool) Schema() json.RawMessage {
	sources := "jira, notion or gmail"
	if len(t.sources) > 0 {
		sources = strings.Join(t.sources, ", ")
	}
	return json.RawMessage(fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "What you are looking for, phrased as a question or a description. Full sentences work better than keywords."
    },
    "source": {
      "type": "string",
      "description": "Optional: restrict the search to one source (%s). Omit to search everything."
    }
  },
  "required": ["query"],
  "additionalProperties": false
}`, sources))
}

// searchArgs are the tool's arguments.
type searchArgs struct {
	Query  string `json:"query"`
	Source string `json:"source"`
}

// Execute embeds the query and returns the nearest chunks.
func (t *searchTool) Execute(ctx context.Context, args json.RawMessage) (tools.Result, error) {
	var input searchArgs
	if err := json.Unmarshal(args, &input); err != nil {
		return tools.Result{}, fmt.Errorf("%s: arguments are not valid JSON: %w", toolName, tools.ErrInvalidArgument)
	}
	query := strings.TrimSpace(input.Query)
	if query == "" {
		return tools.Result{}, fmt.Errorf("%s: query must not be empty: %w", toolName, tools.ErrInvalidArgument)
	}
	source := strings.ToLower(strings.TrimSpace(input.Source))
	if source != "" && len(t.sources) > 0 && !slices.Contains(t.sources, source) {
		// Wrapped as an argument error so the loop does not retry it, and named
		// precisely so the model can correct itself in one step.
		return tools.Result{}, fmt.Errorf("%s: unknown source %q; use one of %s, or omit it: %w",
			toolName, source, strings.Join(t.sources, ", "), tools.ErrInvalidArgument)
	}

	callCtx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()

	vectors, err := t.embedder.Embed(callCtx, []string{query})
	if err != nil {
		// Classified, never wrapped raw. A tool error is written to
		// tool_calls.error, into the tool_call_finished event, into the model's
		// transcript, and is served verbatim by the trace endpoint — four places
		// at once. openai-go's error formats the request URL and the upstream
		// response body, and a 401 body carries a partially-masked API key while
		// a gateway-style OPENAI_BASE_URL can carry a credential in its query
		// string. The orchestrator routes its own provider calls through
		// SafeErrorMessage for exactly this reason; a tool reaching the provider
		// has to do the same. The original goes to the log, which is not served.
		t.logger.Warn("rag: knowledge base query embedding failed", "error", err)
		return tools.Result{}, fmt.Errorf("%s: embed query: %s", toolName, llm.SafeErrorMessage(err))
	}
	if len(vectors) != 1 || len(vectors[0]) == 0 {
		return tools.Result{}, fmt.Errorf("%s: the embedding provider returned no vector", toolName)
	}

	params := store.SearchDocumentChunksParams{
		QueryEmbedding: formatVector(vectors[0]),
		ResultLimit:    int32(t.limit),
	}
	if source != "" {
		params.Source = &source
	}

	rows, err := store.New(t.db).SearchDocumentChunks(callCtx, params)
	if err != nil {
		return tools.Result{}, fmt.Errorf("%s: search chunks: %w", toolName, err)
	}
	if len(rows) == 0 {
		// An empty index and an unlucky query look identical to the model, so
		// say which this is: "nothing is indexed" is a fact about the system it
		// should stop searching over, while "no match" is a cue to rephrase.
		if t.indexEmpty(callCtx, source) {
			return tools.Result{
				Content: "The knowledge base is empty — nothing has been indexed yet. " +
					"Use the live Jira, Notion and Gmail tools instead.",
				Evidence: []tools.EvidenceItem{},
			}, nil
		}
		return tools.Result{
			Content: fmt.Sprintf("No indexed content matched %q. Try different wording, "+
				"or search the live sources directly.", query),
			Evidence: []tools.EvidenceItem{},
		}, nil
	}

	return tools.Result{
		Content:  renderResults(query, source, rows),
		Evidence: buildEvidence(rows),
	}, nil
}

// indexEmpty reports whether there is anything indexed at all for the scope that
// was searched.
//
// The unfiltered case counts the whole table rather than probing the configured
// source list. Deriving it from t.sources was wrong in a way that only shows up
// in a misconfiguration: an empty list made the probe loop iterate nothing and
// report "empty" over a fully populated index, telling the model to give up on a
// knowledge base that had everything it needed.
//
// A failed count returns false. Claiming the index is empty is a claim, and one
// that stops the agent using the tool for the rest of the run — so it is only
// made on a definite answer.
func (t *searchTool) indexEmpty(ctx context.Context, source string) bool {
	q := store.New(t.db)

	var (
		count int64
		err   error
	)
	if source == "" {
		count, err = q.CountDocuments(ctx)
	} else {
		count, err = q.CountDocumentsBySource(ctx, source)
	}
	if err != nil {
		t.logger.Warn("rag: could not count indexed documents", "source", source, "error", err)
		return false
	}
	return count == 0
}

// renderResults formats the hits for the model.
//
// One block per chunk, headed by the document it came from, so the model can see
// which passages belong together and can name the source in its answer. The
// similarity is shown because it is genuinely informative: a top hit at 0.31
// means the query missed and the model should rephrase rather than build on it.
func renderResults(query, source string, rows []store.SearchDocumentChunksRow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d indexed passages matched %q", len(rows), query)
	if source != "" {
		fmt.Fprintf(&b, " in %s", source)
	}
	b.WriteString(".\n")

	for i, row := range rows {
		title := row.Title
		if title == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(&b, "\n%d. [%s %s] %s (similarity %.2f", i+1, row.Source, row.ExternalID, title, row.Similarity)
		if row.SourceTimestamp.Valid {
			fmt.Fprintf(&b, ", %s", row.SourceTimestamp.Time.UTC().Format(time.DateOnly))
		}
		b.WriteString(")\n")
		if row.Url != "" {
			fmt.Fprintf(&b, "   %s\n", row.Url)
		}
		b.WriteString(indent(row.Content))
		b.WriteString("\n")
	}
	return b.String()
}

// indent shifts a chunk's body so it reads as belonging to its heading.
func indent(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i, line := range lines {
		lines[i] = "   " + line
	}
	return strings.Join(lines, "\n")
}

// buildEvidence turns the hits into citable items.
//
// One item per document, not per chunk: three passages from the same Notion page
// are one source, and emitting three would give the run three citation numbers
// pointing at the same URL. The first (highest-scoring) chunk supplies the
// snippet, since that is the passage the answer will actually rest on.
func buildEvidence(rows []store.SearchDocumentChunksRow) []tools.EvidenceItem {
	var (
		items []tools.EvidenceItem
		seen  = make(map[string]struct{}, len(rows))
	)
	for _, row := range rows {
		key := row.Source + "\x00" + row.ExternalID
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}

		item := tools.EvidenceItem{
			Source:     row.Source,
			ExternalID: row.ExternalID,
			Title:      row.Title,
			URL:        row.Url,
			Snippet:    tools.Snippet(row.Content),
		}
		if row.SourceTimestamp.Valid {
			at := row.SourceTimestamp.Time.UTC()
			item.Timestamp = &at
		}
		items = append(items, item)
	}
	return items
}
