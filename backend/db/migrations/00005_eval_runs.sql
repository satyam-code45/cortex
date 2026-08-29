-- +goose Up
-- One row per `make eval` invocation, so answer-quality regressions are
-- visible across runs. metrics holds the same summary the CLI prints: pass
-- rates per metric, overall and per category. Per-case detail lives in
-- evals/results/*.jsonl, not here — this table is for trend lines, not
-- forensics.
--
-- started_at is written explicitly by the runner (no DEFAULT now()) so the
-- JSONL filename, this row, and the printed report all carry one timestamp.
CREATE TABLE eval_runs (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    git_sha    text        NOT NULL,
    started_at timestamptz NOT NULL,
    metrics    jsonb       NOT NULL
);

-- +goose Down
DROP TABLE eval_runs;
