package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/riverqueue/river"

	"cortex/internal/rag"
)

// IndexSourceQueue is the River queue indexing jobs run on.
//
// Separate from AgentRunQueue on purpose. A full crawl of Jira makes hundreds of
// API calls and can run for minutes; sharing a queue would let one reindex
// occupy every worker slot and stall the agent runs a user is waiting on. Two
// queues means the two workloads cannot starve each other, and their concurrency
// is tuned independently.
const IndexSourceQueue = "index_source"

// IndexSourceArgs identifies the source to reindex.
type IndexSourceArgs struct {
	Source string `json:"source"`
}

// Kind is River's stable identifier for this job type. It is persisted in
// river_job rows, so renaming it would orphan every queued job.
func (IndexSourceArgs) Kind() string { return "index_source" }

// IndexSourceWorker runs one source crawl.
//
// Like AgentRunWorker, it is deliberately thin: everything interesting is in
// rag.Indexer, which knows nothing about River and can therefore be tested with
// a fake source and a counting embedder.
type IndexSourceWorker struct {
	river.WorkerDefaults[IndexSourceArgs]

	indexer *rag.Indexer
	logger  *slog.Logger
}

// NewIndexSourceWorker builds the worker.
func NewIndexSourceWorker(indexer *rag.Indexer, logger *slog.Logger) (*IndexSourceWorker, error) {
	if indexer == nil {
		return nil, errors.New("jobs: indexer is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &IndexSourceWorker{indexer: indexer, logger: logger}, nil
}

// Work reindexes the job's source.
//
// Returning an error tells River to retry, and here that is the right default:
// unlike an agent run, a failed crawl has usually hit a rate limit or a
// transient upstream, and a retry is likely to succeed.
//
// Be precise about what the retry costs, because the content-hash check is
// easy to over-claim. It makes a retry free only for documents already WRITTEN,
// and IndexSource embeds the whole changed set before it writes any of it — so a
// failure during the fetch or the embedding phase has written nothing, and the
// retry re-embeds from scratch. Only a failure inside the write loop resumes
// cheaply. What genuinely bounds the damage is INDEX_MAX_DOCUMENTS.
func (w *IndexSourceWorker) Work(ctx context.Context, job *river.Job[IndexSourceArgs]) error {
	w.logger.Info("index job started",
		"source", job.Args.Source, "job_id", job.ID, "attempt", job.Attempt)

	stats, err := w.indexer.IndexSource(ctx, job.Args.Source)
	if err != nil {
		// An unknown source name will be just as unknown on the next attempt, so
		// it is cancelled rather than retried — otherwise River spends 25
		// attempts over several hours re-deriving the same answer.
		if errors.Is(err, rag.ErrUnknownSource) {
			w.logger.Error("index job cancelled: unknown source",
				"source", job.Args.Source, "job_id", job.ID)
			return river.JobCancel(fmt.Errorf("unknown source %q", job.Args.Source))
		}
		return fmt.Errorf("index source %s: %w", job.Args.Source, err)
	}

	w.logger.Info("index job finished",
		"source", stats.Source, "fetched", stats.Fetched, "changed", stats.Changed,
		"unchanged", stats.Unchanged, "chunks", stats.Chunks, "job_id", job.ID)
	return nil
}
