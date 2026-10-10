CREATE TABLE IF NOT EXISTS saige_agent_definitions (
    name       TEXT NOT NULL,
    version    TEXT NOT NULL,
    body       TEXT NOT NULL,
    digest     TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (name, version)
)
---
-- Every change announces the definition's name on the saige_agent_definitions
-- channel, so edits made with plain SQL reach watchers too. The leading 't'
-- is Notifier's marker for a text payload.
CREATE OR REPLACE FUNCTION saige_agent_definitions_notify() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('saige_agent_definitions', 't' || COALESCE(NEW.name, OLD.name));
    RETURN NULL;
END;
$$ LANGUAGE plpgsql
---
CREATE OR REPLACE TRIGGER saige_agent_definitions_changed
    AFTER INSERT OR UPDATE OR DELETE ON saige_agent_definitions
    FOR EACH ROW EXECUTE FUNCTION saige_agent_definitions_notify()
