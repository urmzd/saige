CREATE TABLE IF NOT EXISTS saige_batch_jobs (
    id         TEXT PRIMARY KEY,
    provider   TEXT NOT NULL,
    model      TEXT NOT NULL DEFAULT '',
    manifest   TEXT NOT NULL,
    state      TEXT NOT NULL,
    batch_id   TEXT NOT NULL DEFAULT '',
    owner      TEXT NOT NULL DEFAULT '',
    record     JSONB NOT NULL,
    version    BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
)
---
CREATE INDEX IF NOT EXISTS idx_saige_batch_jobs_state ON saige_batch_jobs(state, created_at)
