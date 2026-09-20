package pgstore

import (
	"context"
	"time"

	pgvector "github.com/pgvector/pgvector-go"

	"github.com/urmzd/saige/rag/knowledge/types"
)

// SearchByEmbedding searches for facts using HNSW vector similarity on entity embeddings.
func (s *Store) SearchByEmbedding(ctx context.Context, embedding []float32, opts *types.SearchOptions) ([]types.ScoredFact, error) {
	if opts == nil {
		opts = &types.SearchOptions{}
	}

	emb := pgvector.NewVector(embedding)

	var query string
	var args []any

	if opts.GroupID != "" {
		query = searchEmbeddingGroupSQL
		args = []any{emb, opts.GroupID}
	} else {
		query = searchEmbeddingSQL
		args = []any{emb}
	}

	return s.queryFacts(ctx, query, args)
}

// SearchByText searches for facts using fulltext search on entity name and summary.
func (s *Store) SearchByText(ctx context.Context, queryText string, opts *types.SearchOptions) ([]types.ScoredFact, error) {
	if opts == nil {
		opts = &types.SearchOptions{}
	}

	var query string
	var args []any

	if opts.GroupID != "" {
		query = searchTextGroupSQL
		args = []any{queryText, opts.GroupID}
	} else {
		query = searchTextSQL
		args = []any{queryText}
	}

	return s.queryFacts(ctx, query, args)
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
