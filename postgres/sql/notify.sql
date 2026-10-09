CREATE TABLE IF NOT EXISTS saige_notifications (
    id         BIGSERIAL PRIMARY KEY,
    channel    TEXT NOT NULL,
    payload    BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
)
---
CREATE INDEX IF NOT EXISTS idx_saige_notifications_expires ON saige_notifications(expires_at)
---
CREATE UNLOGGED TABLE IF NOT EXISTS saige_cache (
    key        TEXT PRIMARY KEY,
    value      BYTEA NOT NULL,
    expires_at TIMESTAMPTZ
)
---
CREATE INDEX IF NOT EXISTS idx_saige_cache_expires ON saige_cache(expires_at) WHERE expires_at IS NOT NULL
