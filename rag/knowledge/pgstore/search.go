package pgstore

import (
	"context"
	"time"

	pgvector "github.com/pgvector/pgvector-go"

	"github.com/urmzd/saige/rag/knowledge/types"
)

// defaultSearchLimit bounds a search whose options set no limit.
const defaultSearchLimit = 20

// searchArgs returns the shared group, limit, and as-of parameters of the
// search queries. An empty GroupID searches every group.
func searchArgs(opts *types.SearchOptions) (group *string, limit int, validAt *time.Time) {
	if opts == nil {
		return nil, defaultSearchLimit, nil
	}
	if opts.GroupID != "" {
		g := opts.GroupID
		group = &g
	}
	limit = opts.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	return group, limit, opts.ValidAt
}

// SearchByEmbedding searches for facts using HNSW vector similarity on
// entity embeddings. Each matched entity contributes at most Limit of its
// newest edges, and at most Limit facts are returned.
func (s *Store) SearchByEmbedding(ctx context.Context, embedding []float32, opts *types.SearchOptions) ([]types.ScoredFact, error) {
	group, limit, validAt := searchArgs(opts)
	return s.queryFacts(ctx, searchEmbeddingSQL, []any{pgvector.NewVector(embedding), group, limit, validAt})
}

// SearchByText searches for facts using Postgres full-text search (ts_rank)
// on entity name and summary and on the fact text and relation type. Each
// matched entity contributes at most Limit of its newest edges, and at most
// Limit facts are returned.
func (s *Store) SearchByText(ctx context.Context, queryText string, opts *types.SearchOptions) ([]types.ScoredFact, error) {
	group, limit, validAt := searchArgs(opts)
	return s.queryFacts(ctx, searchTextSQL, []any{queryText, group, limit, validAt})
}

// queryFacts executes a fact query and returns deduplicated ScoredFacts.
func (s *Store) queryFacts(ctx context.Context, query string, args []any) ([]types.ScoredFact, error) {
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var facts []types.ScoredFact
	seen := make(map[string]bool)

	for rows.Next() {
		var (
			srcUUID, srcName, srcType, srcSummary string
			rUUID, rType, rFact                   string
			rCreatedAt, rValidAt                  time.Time
			rInvalidAt                            *time.Time
			tgtUUID, tgtName, tgtType, tgtSummary string
			score                                 float64
		)
		if err := rows.Scan(
			&srcUUID, &srcName, &srcType, &srcSummary,
			&rUUID, &rType, &rFact, &rCreatedAt, &rValidAt, &rInvalidAt,
			&tgtUUID, &tgtName, &tgtType, &tgtSummary,
			&score,
		); err != nil {
			return nil, err
		}

		if seen[rUUID] {
			continue
		}
		seen[rUUID] = true

		facts = append(facts, types.ScoredFact{
			Fact: types.Fact{
				UUID:     rUUID,
				Name:     rType,
				FactText: rFact,
				SourceNode: types.Entity{
					UUID: srcUUID, Name: srcName, Type: srcType, Summary: srcSummary,
				},
				TargetNode: types.Entity{
					UUID: tgtUUID, Name: tgtName, Type: tgtType, Summary: tgtSummary,
				},
				CreatedAt: rCreatedAt,
				ValidAt:   rValidAt,
				InvalidAt: rInvalidAt,
			},
			Score: score,
		})
	}

	return facts, rows.Err()
}
