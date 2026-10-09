package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	kgtypes "github.com/urmzd/saige/rag/knowledge/types"
)

// Graph is the part of a knowledge graph KGStore uses. A
// knowledge.Graph satisfies it.
type Graph interface {
	IngestEpisode(ctx context.Context, input *kgtypes.EpisodeInput) (*kgtypes.IngestResult, error)
	SearchFacts(ctx context.Context, query string, opts ...kgtypes.SearchOption) (*kgtypes.SearchFactsResult, error)
}

// KGStore keeps memories in a knowledge graph. Each memory is ingested as
// an episode under a group derived from the scope, so searches never cross
// tenants, and recall returns the facts extracted from it.
//
// The graph matches groups exactly, so a scope sees only its own namespace,
// not sub-namespaces. Forget takes the ID Remember returned and needs a graph
// that implements kgtypes.DocumentEpisodeDeleter; Remember of an existing ID
// replaces its episode when the graph supports deletion.
type KGStore struct {
	Graph Graph
	now   func() time.Time
}

var _ Store = (*KGStore)(nil)

// NewKGStore returns a store over g.
func NewKGStore(g Graph) *KGStore { return &KGStore{Graph: g, now: time.Now} }

// GroupID returns the graph group a scope's memories live in.
func GroupID(s Scope) string {
	sum := sha256.Sum256([]byte(s.Key()))
	return "memory-" + hex.EncodeToString(sum[:16])
}

// Remember implements Store.
func (k *KGStore) Remember(ctx context.Context, r Record) (string, error) {
	if err := r.Scope.Validate(); err != nil {
		return "", err
	}
	if r.Scope.ReadOnly {
		return "", ErrReadOnly
	}
	if r.Kind == "" {
		r.Kind = KindSemantic
	}
	if r.ID == "" {
		r.ID = recordID(r)
	}
	group := GroupID(r.Scope)
	if del, ok := k.Graph.(kgtypes.DocumentEpisodeDeleter); ok {
		// Replace rather than duplicate: a repeated write with the same ID
		// leaves one episode.
		if err := del.DeleteDocumentEpisodes(ctx, group, r.ID); err != nil {
			return "", fmt.Errorf("memory: replace episode: %w", err)
		}
	}
	created := r.CreatedAt
	if created.IsZero() {
		created = k.now().UTC()
	}
	meta := map[string]string{"memory_kind": string(r.Kind)}
	if len(r.Tags) > 0 {
		meta["memory_tags"] = strings.Join(r.Tags, ",")
	}
	if r.Source.Agent != "" {
		meta["memory_agent"] = r.Source.Agent
	}
	if r.Source.ToolCallID != "" {
		meta["memory_tool_call_id"] = r.Source.ToolCallID
	}
	if !r.ExpiresAt.IsZero() {
		meta["memory_expires"] = r.ExpiresAt.Format(time.RFC3339)
	}
	source := r.Source.Source
	if source == "" {
		source = "agent memory"
	}
	if _, err := k.Graph.IngestEpisode(ctx, &kgtypes.EpisodeInput{
		Name:          r.ID,
		Body:          r.Content,
		Source:        source,
		GroupID:       group,
		DocumentID:    r.ID,
		ReferenceTime: created,
		Metadata:      meta,
	}); err != nil {
		return "", err
	}
	return r.ID, nil
}

// Recall implements Store. Each fact becomes a semantic record whose ID is
// the fact's UUID.
func (k *KGStore) Recall(ctx context.Context, s Scope, query string, budget int) ([]Record, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(query) == "" {
		return nil, nil // a graph search needs a query
	}
	res, err := k.Graph.SearchFacts(ctx, query, kgtypes.WithGroupID(GroupID(s)), kgtypes.WithLimit(50))
	if err != nil {
		return nil, err
	}
	if budget <= 0 {
		budget = DefaultRecallBudget
	}
	var out []Record
	used := 0
	for _, f := range res.Facts {
		cost := EstimateTokens(f.FactText)
		if used+cost > budget {
			break
		}
		used += cost
		out = append(out, Record{ID: f.UUID, Scope: s, Kind: KindSemantic, Content: f.FactText, CreatedAt: f.CreatedAt})
	}
	return out, nil
}

// Forget implements Store.
func (k *KGStore) Forget(ctx context.Context, s Scope, id string) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if s.ReadOnly {
		return ErrReadOnly
	}
	del, ok := k.Graph.(kgtypes.DocumentEpisodeDeleter)
	if !ok {
		return fmt.Errorf("%w: graph cannot delete episodes", ErrUnsupported)
	}
	return del.DeleteDocumentEpisodes(ctx, GroupID(s), id)
}
