CREATE TABLE IF NOT EXISTS eval_run (
    tenant      TEXT NOT NULL DEFAULT '',
    id          TEXT NOT NULL,
    suite       TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL,
    revision    TEXT NOT NULL DEFAULT '',
    labels      JSONB NOT NULL DEFAULT '{}',
    started_at  TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ,
    record      JSONB NOT NULL,
    PRIMARY KEY (tenant, id)
)
---
CREATE INDEX IF NOT EXISTS idx_eval_run_started ON eval_run(tenant, started_at DESC, id COLLATE "C" DESC)
---
CREATE INDEX IF NOT EXISTS idx_eval_run_suite ON eval_run(tenant, suite, started_at DESC, id COLLATE "C" DESC)
---
CREATE INDEX IF NOT EXISTS idx_eval_run_suite_status ON eval_run(tenant, suite, status, started_at DESC, id COLLATE "C" DESC)
---
CREATE TABLE IF NOT EXISTS eval_unit (
    tenant         TEXT NOT NULL DEFAULT '',
    run_id         TEXT NOT NULL,
    key            TEXT NOT NULL,
    seq            BIGSERIAL,
    attempt        INT NOT NULL,
    observation_id TEXT NOT NULL,
    labels         JSONB NOT NULL DEFAULT '{}',
    recorded_at    TIMESTAMPTZ NOT NULL,
    unit           JSONB NOT NULL,
    PRIMARY KEY (tenant, run_id, key),
    FOREIGN KEY (tenant, run_id) REFERENCES eval_run(tenant, id) ON DELETE CASCADE
)
---
CREATE INDEX IF NOT EXISTS idx_eval_unit_run_seq ON eval_unit(tenant, run_id, seq)
---
CREATE INDEX IF NOT EXISTS idx_eval_unit_labels ON eval_unit USING GIN (labels jsonb_path_ops)
---
CREATE INDEX IF NOT EXISTS idx_eval_unit_case ON eval_unit(tenant, observation_id, recorded_at DESC)
---
CREATE TABLE IF NOT EXISTS eval_unit_attempt (
    tenant      TEXT NOT NULL DEFAULT '',
    run_id      TEXT NOT NULL,
    key         TEXT NOT NULL,
    attempt     INT NOT NULL,
    recorded_at TIMESTAMPTZ NOT NULL,
    unit        JSONB NOT NULL,
    PRIMARY KEY (tenant, run_id, key, attempt),
    FOREIGN KEY (tenant, run_id) REFERENCES eval_run(tenant, id) ON DELETE CASCADE
)
---
CREATE TABLE IF NOT EXISTS eval_score (
    tenant  TEXT NOT NULL DEFAULT '',
    run_id  TEXT NOT NULL,
    key     TEXT NOT NULL,
    name    TEXT NOT NULL,
    value   DOUBLE PRECISION NOT NULL,
    errored BOOLEAN NOT NULL DEFAULT false,
    passed  BOOLEAN,
    PRIMARY KEY (tenant, run_id, name, key),
    FOREIGN KEY (tenant, run_id, key) REFERENCES eval_unit(tenant, run_id, key) ON DELETE CASCADE
)
---
CREATE INDEX IF NOT EXISTS idx_agent_node_finished ON agent_node(created_at, uuid) WHERE role = 'assistant'
