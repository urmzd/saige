package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"

	"github.com/urmzd/saige/agent/types"
)

// ExtractionSource is the Provenance.Source of records ExtractAfterRun
// writes.
const ExtractionSource = "post-run extraction"

// Extractor proposes memories from a finished run's messages, for example
// by asking a model for durable facts. Extraction is opt-in: nothing in the
// agent loop calls it. The host calls Policy.ExtractAfterRun once a run has
// ended, from wherever it handles a finished run.
type Extractor interface {
	Extract(ctx context.Context, messages []types.Message) ([]Record, error)
}

// ExtractorFunc adapts a function to Extractor.
type ExtractorFunc func(ctx context.Context, messages []types.Message) ([]Record, error)

// Extract implements Extractor.
func (f ExtractorFunc) Extract(ctx context.Context, messages []types.Message) ([]Record, error) {
	return f(ctx, messages)
}

// ExtractAfterRun runs ex over a finished run and stores what it proposes
// through Remember, so every record passes the same checks as a tool
// write. The scope is resolved for owner from Policy.Scope and replaces any
// scope the extractor set. runID names the run: each record's idempotency
// key is derived from it, the record's position, and its content, so
// calling ExtractAfterRun again for the same run, as a replayed hook does,
// stores nothing new. It returns the stored IDs and stops at the first
// failed write.
func (p Policy) ExtractAfterRun(ctx context.Context, store Store, ex Extractor, owner, runID string, messages []types.Message) ([]string, error) {
	if runID == "" {
		return nil, fmt.Errorf("memory: extraction needs a run ID for idempotency")
	}
	s, err := p.ResolveScope(ctx, owner)
	if err != nil {
		return nil, err
	}
	recs, err := ex.Extract(ctx, messages)
	if err != nil {
		return nil, fmt.Errorf("memory: extract: %w", err)
	}
	ids := make([]string, 0, len(recs))
	for i, r := range recs {
		r.Scope = s
		r.ID = ""
		if r.Source.Source == "" {
			r.Source.Source = ExtractionSource
		}
		if r.Source.Agent == "" {
			r.Source.Agent = owner
		}
		sum := sha256.Sum256([]byte(runID + "\x00" + strconv.Itoa(i) + "\x00" + r.Content))
		r.IdempotencyKey = "extract:" + hex.EncodeToString(sum[:12])
		id, err := p.Remember(ctx, store, r)
		if err != nil {
			return ids, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}
