package surrealdb

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	surrealdbgo "github.com/surrealdb/surrealdb.go"
	"github.com/surrealdb/surrealdb.go/pkg/models"

	"github.com/urmzd/saige/knowledge/types"
)

var _ types.Store = (*Store)(nil)

// StoreConfig holds the configuration for creating a Store.
type StoreConfig struct {
	URL       string
	Namespace string
	Database  string
	Username  string
	Password  string
	Logger    *slog.Logger
}

// Store implements types.Store using SurrealDB.
type Store struct {
	db     *surrealdbgo.DB
	logger *slog.Logger
}

// DB exposes the underlying SurrealDB connection for sharing (e.g. event store).
func (s *Store) DB() *surrealdbgo.DB {
	return s.db
}

// NewStore creates a new SurrealDB-backed store.
func NewStore(ctx context.Context, cfg StoreConfig) (*Store, error) {
	db, err := surrealdbgo.FromEndpointURLString(ctx, cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("connect surrealdb: %w", err)
	}

	if err := db.Use(ctx, cfg.Namespace, cfg.Database); err != nil {
		return nil, fmt.Errorf("use namespace/db: %w", err)
	}

	if _, err := db.SignIn(ctx, map[string]string{"user": cfg.Username, "pass": cfg.Password}); err != nil {
		return nil, fmt.Errorf("sign in: %w", err)
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	if err := ensureSchema(ctx, db); err != nil {
		logger.Warn("schema provisioning had errors", "error", err)
	}

	return &Store{db: db, logger: logger}, nil
}

// Close closes the SurrealDB connection.
func (s *Store) Close(ctx context.Context) error {
	s.db.Close(ctx)
	return nil
}

// --- Entity operations ---

// UpsertEntity creates or updates an entity by (name, type), returning its UUID.
func (s *Store) UpsertEntity(ctx context.Context, entity *types.ExtractedEntity, embedding []float32) (string, error) {
	existing, err := surrealdbgo.Query[[]entityRecord](ctx, s.db,
		"SELECT id, uuid FROM entity WHERE name = $name AND type = $type LIMIT 1",
		map[string]any{"name": entity.Name, "type": entity.Type},
	)

	if err == nil && existing != nil && len(*existing) > 0 && len((*existing)[0].Result) > 0 {
		rec := (*existing)[0].Result[0]
		params := map[string]any{"id": rec.ID, "summary": entity.Summary}
		query := "UPDATE $id SET summary = $summary"
		if embedding != nil {
			params["embedding"] = embedding
			query += ", embedding = $embedding"
		}
		if _, err := surrealdbgo.Query[any](ctx, s.db, query, params); err != nil {
			return "", fmt.Errorf("update entity %s: %w", entity.Name, err)
		}
		return rec.UUID, nil
	}

	entUUID := uuid.New().String()
	params := map[string]any{
		"uuid": entUUID, "name": entity.Name, "type": entity.Type, "summary": entity.Summary,
	}
	query := "CREATE entity SET uuid = $uuid, name = $name, type = $type, summary = $summary"
	if embedding != nil {
		params["embedding"] = embedding
		query += ", embedding = $embedding"
	}
	query += " RETURN id, uuid"

	created, err := surrealdbgo.Query[[]entityRecord](ctx, s.db, query, params)
	if err != nil || created == nil || len(*created) == 0 || len((*created)[0].Result) == 0 {
		return "", fmt.Errorf("create entity %s: %w", entity.Name, err)
	}

	return (*created)[0].Result[0].UUID, nil
}

// GetEntity retrieves an entity by UUID.
func (s *Store) GetEntity(ctx context.Context, id string) (*types.Entity, error) {
	result, err := surrealdbgo.Query[[]nodeRecord](ctx, s.db,
		"SELECT id, uuid, name, type, summary FROM entity WHERE uuid = $uuid LIMIT 1",
		map[string]any{"uuid": id},
	)
	if err != nil || result == nil || len(*result) == 0 || len((*result)[0].Result) == 0 {
		return nil, fmt.Errorf("%w: %s", types.ErrNodeNotFound, id)
	}

	rec := (*result)[0].Result[0]
	return &types.Entity{UUID: rec.UUID, Name: rec.Name, Type: rec.Type, Summary: rec.Summary}, nil
}

// FindEntitiesByNameType finds entities with exact name+type match.
func (s *Store) FindEntitiesByNameType(ctx context.Context, name, entityType string) ([]types.Entity, error) {
	result, err := surrealdbgo.Query[[]nodeRecord](ctx, s.db,
		"SELECT id, uuid, name, type, summary FROM entity WHERE name = $name AND type = $type",
		map[string]any{"name": name, "type": entityType},
	)
	if err != nil {
		return nil, err
	}
	return nodeRecordsToEntities(result), nil
}

// FindEntitiesByFuzzyName returns entities whose names might match, for fuzzy dedup.
// Uses BM25 fulltext search to find candidates, then the caller applies fuzzy scoring.
func (s *Store) FindEntitiesByFuzzyName(ctx context.Context, name string, limit int) ([]types.Entity, error) {
	if limit <= 0 {
		limit = 10
	}
	result, err := surrealdbgo.Query[[]nodeRecord](ctx, s.db,
		`SELECT id, uuid, name, type, summary FROM entity
		 WHERE name @@ $name
		 LIMIT $limit`,
		map[string]any{"name": name, "limit": limit},
	)
	if err != nil {
		return nil, err
	}
	return nodeRecordsToEntities(result), nil
}

// --- Relation operations ---

// CreateRelation creates a relation edge between two entities, returning the relation UUID.
func (s *Store) CreateRelation(ctx context.Context, rel *types.RelationInput) (string, error) {
	// Resolve entity UUIDs to SurrealDB record IDs
	srcRID, err := s.entityRecordID(ctx, rel.SourceUUID)
	if err != nil {
		return "", fmt.Errorf("source entity %s: %w", rel.SourceUUID, err)
	}
	tgtRID, err := s.entityRecordID(ctx, rel.TargetUUID)
	if err != nil {
		return "", fmt.Errorf("target entity %s: %w", rel.TargetUUID, err)
	}

	relUUID := uuid.New().String()
	validAt := rel.ValidAt
	if validAt.IsZero() {
		validAt = time.Now()
	}

	if _, err := surrealdbgo.Query[any](ctx, s.db,
		`RELATE $src->relates_to->$tgt
		 SET uuid = $uuid, type = $type, fact = $fact, valid_at = $valid_at`,
		map[string]any{
			"src": srcRID, "tgt": tgtRID,
			"uuid": relUUID, "type": rel.Type, "fact": rel.Fact,
			"valid_at": validAt,
		},
	); err != nil {
		return "", fmt.Errorf("create relation: %w", err)
	}

	return relUUID, nil
}

// InvalidateRelation marks a relation as no longer valid.
func (s *Store) InvalidateRelation(ctx context.Context, relUUID string, invalidAt time.Time) error {
	if _, err := surrealdbgo.Query[any](ctx, s.db,
		"UPDATE relates_to SET invalid_at = $invalid_at WHERE uuid = $uuid",
		map[string]any{"uuid": relUUID, "invalid_at": invalidAt},
	); err != nil {
		return fmt.Errorf("invalidate relation %s: %w", relUUID, err)
	}
	return nil
}

// FindRelationsBetweenEntities returns all valid relations between two entities.
func (s *Store) FindRelationsBetweenEntities(ctx context.Context, srcUUID, tgtUUID string) ([]types.Relation, error) {
	result, err := surrealdbgo.Query[[]relationRecord](ctx, s.db,
		`SELECT uuid, type, fact, created_at, valid_at, invalid_at,
			in.uuid AS in_uuid, out.uuid AS out_uuid
		 FROM relates_to
		 WHERE (in.uuid = $src AND out.uuid = $tgt)
		    OR (in.uuid = $tgt AND out.uuid = $src)`,
		map[string]any{"src": srcUUID, "tgt": tgtUUID},
	)
	if err != nil {
		return nil, err
	}

	if result == nil || len(*result) == 0 {
		return nil, nil
	}

	var rels []types.Relation
	for _, rec := range (*result)[0].Result {
		rels = append(rels, types.Relation{
			UUID:       rec.UUID,
			SourceUUID: rec.InUUID,
			TargetUUID: rec.OutUUID,
			Type:       rec.Type,
			Fact:       rec.Fact,
			CreatedAt:  rec.CreatedAt,
			ValidAt:    rec.ValidAt,
			InvalidAt:  rec.InvalidAt,
		})
	}
	return rels, nil
}

// --- Episode operations ---

// CreateEpisode creates an episode record and links it to entities via mentions edges.
func (s *Store) CreateEpisode(ctx context.Context, input *types.EpisodeInput, entityUUIDs []string) (string, error) {
	episodeUUID := uuid.New().String()

	epResult, err := surrealdbgo.Query[[]episodeRecord](ctx, s.db,
		"CREATE episode SET uuid = $uuid, name = $name, body = $body, source = $source, group_id = $group_id, metadata = $metadata RETURN id",
		map[string]any{
			"uuid": episodeUUID, "name": input.Name, "body": input.Body,
			"source": input.Source, "group_id": input.GroupID, "metadata": input.Metadata,
		},
	)
	if err != nil || epResult == nil || len(*epResult) == 0 || len((*epResult)[0].Result) == 0 {
		return "", fmt.Errorf("create episode %s: %w", input.Name, err)
	}

	epID := (*epResult)[0].Result[0].ID
	for _, entUUID := range entityUUIDs {
		entRID, err := s.entityRecordID(ctx, entUUID)
		if err != nil {
			s.logger.Warn("create mention: entity not found", "uuid", entUUID, "error", err)
			continue
		}
		if _, err := surrealdbgo.Query[any](ctx, s.db,
			"RELATE $ep->mentions->$ent",
			map[string]any{"ep": epID, "ent": entRID},
		); err != nil {
			s.logger.Warn("create mention failed", "episode", input.Name, "error", err)
		}
	}

	return episodeUUID, nil
}

// --- Search operations ---

// SearchByEmbedding searches for facts using vector similarity.
func (s *Store) SearchByEmbedding(ctx context.Context, embedding []float32, opts *types.SearchOptions) ([]types.ScoredFact, error) {
	if opts == nil {
		opts = &types.SearchOptions{}
	}

	var rows []factRow
	if opts.GroupID != "" {
		result, err := surrealdbgo.Query[[]factRow](ctx, s.db,
			`SELECT
				r.uuid AS r_uuid, r.type AS r_type, r.fact AS r_fact,
				r.created_at AS r_created_at, r.valid_at AS r_valid_at, r.invalid_at AS r_invalid_at,
				node.uuid AS src_uuid, node.name AS src_name, node.type AS src_type, node.summary AS src_summary,
				other.uuid AS tgt_uuid, other.name AS tgt_name, other.type AS tgt_type, other.summary AS tgt_summary,
				vector::similarity::cosine(node.embedding, $emb) AS score
			FROM entity AS node
			WHERE node.embedding <|20|> $emb
			AND (SELECT VALUE id FROM episode WHERE group_id = $group_id AND ->mentions->entity CONTAINS node.id) != []
			SPLIT r
			LET r = (SELECT * FROM relates_to WHERE (in = node.id OR out = node.id) AND invalid_at IS NONE)
			LET other = IF r.in = node.id THEN (SELECT * FROM r.out) ELSE (SELECT * FROM r.in) END
			ORDER BY score DESC`,
			map[string]any{"emb": embedding, "group_id": opts.GroupID},
		)
		if err == nil && result != nil && len(*result) > 0 {
			rows = (*result)[0].Result
		}
	} else {
		result, err := surrealdbgo.Query[[]factRow](ctx, s.db,
			`LET $matches = (SELECT id, uuid, name, type, summary, embedding,
				vector::similarity::cosine(embedding, $emb) AS score
				FROM entity WHERE embedding <|20|> $emb ORDER BY score DESC);
			SELECT
				r.uuid AS r_uuid, r.type AS r_type, r.fact AS r_fact,
				r.created_at AS r_created_at, r.valid_at AS r_valid_at, r.invalid_at AS r_invalid_at,
				m.uuid AS src_uuid, m.name AS src_name, m.type AS src_type, m.summary AS src_summary,
				other.uuid AS tgt_uuid, other.name AS tgt_name, other.type AS tgt_type, other.summary AS tgt_summary,
				m.score AS score
			FROM $matches AS m,
				(SELECT * FROM relates_to WHERE (in = m.id OR out = m.id) AND invalid_at IS NONE) AS r,
				IF r.in = m.id THEN (SELECT * FROM entity WHERE id = r.out)[0] ELSE (SELECT * FROM entity WHERE id = r.in)[0] END AS other
			ORDER BY score DESC`,
			map[string]any{"emb": embedding},
		)
		if err == nil && result != nil && len(*result) > 1 {
			rows = (*result)[1].Result
		}
	}

	return s.factRowsToScoredFacts(rows), nil
}

// SearchByText searches for facts using BM25 fulltext search.
func (s *Store) SearchByText(ctx context.Context, query string, opts *types.SearchOptions) ([]types.ScoredFact, error) {
	if opts == nil {
		opts = &types.SearchOptions{}
	}

	var rows []factRow
	if opts.GroupID != "" {
		result, err := surrealdbgo.Query[[]factRow](ctx, s.db,
			`SELECT
				r.uuid AS r_uuid, r.type AS r_type, r.fact AS r_fact,
				r.created_at AS r_created_at, r.valid_at AS r_valid_at, r.invalid_at AS r_invalid_at,
				node.uuid AS src_uuid, node.name AS src_name, node.type AS src_type, node.summary AS src_summary,
				other.uuid AS tgt_uuid, other.name AS tgt_name, other.type AS tgt_type, other.summary AS tgt_summary,
				search::score(1) AS score
			FROM entity AS node
			WHERE (node.name @1@ $query OR node.summary @1@ $query)
			AND (SELECT VALUE id FROM episode WHERE group_id = $group_id AND ->mentions->entity CONTAINS node.id) != []
			SPLIT r
			LET r = (SELECT * FROM relates_to WHERE (in = node.id OR out = node.id) AND invalid_at IS NONE)
			LET other = IF r.in = node.id THEN (SELECT * FROM r.out) ELSE (SELECT * FROM r.in) END
			ORDER BY score DESC`,
			map[string]any{"query": query, "group_id": opts.GroupID},
		)
		if err == nil && result != nil && len(*result) > 0 {
			rows = (*result)[0].Result
		}
	} else {
		result, err := surrealdbgo.Query[[]factRow](ctx, s.db,
			`SELECT
				r.uuid AS r_uuid, r.type AS r_type, r.fact AS r_fact,
				r.created_at AS r_created_at, r.valid_at AS r_valid_at, r.invalid_at AS r_invalid_at,
				m.uuid AS src_uuid, m.name AS src_name, m.type AS src_type, m.summary AS src_summary,
				other.uuid AS tgt_uuid, other.name AS tgt_name, other.type AS tgt_type, other.summary AS tgt_summary,
				search::score(1) AS score
			FROM entity AS m
			WHERE m.name @1@ $query OR m.summary @1@ $query
			SPLIT r
			LET r = (SELECT * FROM relates_to WHERE (in = m.id OR out = m.id) AND invalid_at IS NONE)
			LET other = IF r.in = m.id THEN (SELECT * FROM entity WHERE id = r.out)[0] ELSE (SELECT * FROM entity WHERE id = r.in)[0] END
			ORDER BY score DESC`,
			map[string]any{"query": query},
		)
		if err == nil && result != nil && len(*result) > 0 {
			rows = (*result)[0].Result
		}
	}

	return s.factRowsToScoredFacts(rows), nil
}

// --- Graph operations ---

// GetGraph returns the full graph data (only valid relations).
func (s *Store) GetGraph(ctx context.Context, limit int64) (*types.GraphData, error) {
	result, err := surrealdbgo.Query[[]graphRow](ctx, s.db,
		`SELECT
			in.uuid AS a_uuid, in.name AS a_name, in.type AS a_type, in.summary AS a_summary,
			uuid AS r_uuid, type AS r_type, fact AS r_fact,
			created_at AS r_created_at, valid_at AS r_valid_at, invalid_at AS r_invalid_at,
			out.uuid AS b_uuid, out.name AS b_name, out.type AS b_type, out.summary AS b_summary
		FROM relates_to
		WHERE invalid_at IS NONE
		LIMIT $limit`,
		map[string]any{"limit": limit},
	)
	if err != nil {
		return nil, err
	}

	nodeMap := make(map[string]types.GraphNode)
	var edges []types.GraphEdge

	if result != nil && len(*result) > 0 {
		for _, row := range (*result)[0].Result {
			if row.AUUID == "" || row.BUUID == "" {
				continue
			}
			if _, ok := nodeMap[row.AUUID]; !ok {
				nodeMap[row.AUUID] = types.GraphNode{
					ID: row.AUUID, Name: row.AName, Type: row.AType, Summary: row.ASummary,
				}
			}
			if _, ok := nodeMap[row.BUUID]; !ok {
				nodeMap[row.BUUID] = types.GraphNode{
					ID: row.BUUID, Name: row.BName, Type: row.BType, Summary: row.BSummary,
				}
			}
			edges = append(edges, types.GraphEdge{
				ID: row.RUUID, Source: row.AUUID, Target: row.BUUID,
				Type: row.RType, Fact: row.RFact, Weight: 1.0,
				CreatedAt: row.RCreatedAt, ValidAt: row.RValidAt, InvalidAt: row.RInvalidAt,
			})
		}
	}

	nodes := make([]types.GraphNode, 0, len(nodeMap))
	for _, n := range nodeMap {
		nodes = append(nodes, n)
	}

	return &types.GraphData{Nodes: nodes, Edges: edges}, nil
}

// GetNode returns a node with its neighbors and edges.
// depth controls how many hops to traverse (1 = immediate neighbors).
func (s *Store) GetNode(ctx context.Context, id string, depth int) (*types.NodeDetail, error) {
	if depth < 1 {
		depth = 1
	}

	result, err := surrealdbgo.Query[[]nodeRecord](ctx, s.db,
		"SELECT id, uuid, name, type, summary FROM entity WHERE uuid = $uuid LIMIT 1",
		map[string]any{"uuid": id},
	)
	if err != nil || result == nil || len(*result) == 0 || len((*result)[0].Result) == 0 {
		return nil, fmt.Errorf("%w: %s", types.ErrNodeNotFound, id)
	}

	nodeData := (*result)[0].Result[0]
	rootNode := types.GraphNode{
		ID: nodeData.UUID, Name: nodeData.Name, Type: nodeData.Type, Summary: nodeData.Summary,
	}

	// BFS multi-hop traversal
	visited := map[string]bool{id: true}
	allNeighbors := []types.GraphNode{}
	allEdges := []types.GraphEdge{}
	frontier := []string{id} // UUIDs to expand

	for d := 0; d < depth && len(frontier) > 0; d++ {
		var nextFrontier []string
		for _, nodeUUID := range frontier {
			neighbors, edges, err := s.getNeighbors(ctx, nodeUUID)
			if err != nil {
				s.logger.Warn("get neighbors failed", "uuid", nodeUUID, "error", err)
				continue
			}
			allEdges = append(allEdges, edges...)
			for _, n := range neighbors {
				if !visited[n.ID] {
					visited[n.ID] = true
					allNeighbors = append(allNeighbors, n)
					nextFrontier = append(nextFrontier, n.ID)
				}
			}
		}
		frontier = nextFrontier
	}

	if allNeighbors == nil {
		allNeighbors = []types.GraphNode{}
	}
	if allEdges == nil {
		allEdges = []types.GraphEdge{}
	}

	return &types.NodeDetail{Node: rootNode, Neighbors: allNeighbors, Edges: allEdges}, nil
}

// getNeighbors returns the immediate neighbors and edges for a node UUID.
func (s *Store) getNeighbors(ctx context.Context, nodeUUID string) ([]types.GraphNode, []types.GraphEdge, error) {
	// First get the record ID
	recResult, err := surrealdbgo.Query[[]nodeRecord](ctx, s.db,
		"SELECT id FROM entity WHERE uuid = $uuid LIMIT 1",
		map[string]any{"uuid": nodeUUID},
	)
	if err != nil || recResult == nil || len(*recResult) == 0 || len((*recResult)[0].Result) == 0 {
		return nil, nil, fmt.Errorf("entity not found: %s", nodeUUID)
	}
	recordID := (*recResult)[0].Result[0].ID

	relResult, err := surrealdbgo.Query[[]relRow](ctx, s.db,
		`SELECT
			uuid AS r_uuid, type AS r_type, fact AS r_fact,
			created_at AS r_created_at, valid_at AS r_valid_at, invalid_at AS r_invalid_at,
			(IF in = $record_id THEN out.uuid ELSE in.uuid END) AS n_uuid,
			(IF in = $record_id THEN out.name ELSE in.name END) AS n_name,
			(IF in = $record_id THEN out.type ELSE in.type END) AS n_type,
			(IF in = $record_id THEN out.summary ELSE in.summary END) AS n_summary,
			in = $record_id AS is_outgoing
		FROM relates_to
		WHERE (in = $record_id OR out = $record_id)
		  AND invalid_at IS NONE`,
		map[string]any{"record_id": recordID},
	)
	if err != nil {
		return nil, nil, err
	}

	var neighbors []types.GraphNode
	var edges []types.GraphEdge
	seen := make(map[string]bool)

	if relResult != nil && len(*relResult) > 0 {
		for _, row := range (*relResult)[0].Result {
			if row.NUUID == "" {
				continue
			}
			if !seen[row.NUUID] {
				seen[row.NUUID] = true
				neighbors = append(neighbors, types.GraphNode{
					ID: row.NUUID, Name: row.NName, Type: row.NType, Summary: row.NSummary,
				})
			}
			src, tgt := row.NUUID, nodeUUID
			if row.IsOutgoing {
				src, tgt = nodeUUID, row.NUUID
			}
			edges = append(edges, types.GraphEdge{
				ID: row.RUUID, Source: src, Target: tgt,
				Type: row.RType, Fact: row.RFact, Weight: 1.0,
				CreatedAt: row.RCreatedAt, ValidAt: row.RValidAt, InvalidAt: row.RInvalidAt,
			})
		}
	}

	return neighbors, edges, nil
}

// --- Provenance ---

// GetFactProvenance returns the episodes that mention entities involved in a fact.
func (s *Store) GetFactProvenance(ctx context.Context, factUUID string) ([]types.Episode, error) {
	result, err := surrealdbgo.Query[[]episodeFullRecord](ctx, s.db,
		`SELECT uuid, name, body, source, group_id, created_at
		 FROM episode
		 WHERE ->mentions->entity->relates_to[WHERE uuid = $uuid] != []
		    OR ->mentions->entity<-relates_to[WHERE uuid = $uuid] != []`,
		map[string]any{"uuid": factUUID},
	)
	if err != nil {
		return nil, err
	}

	if result == nil || len(*result) == 0 {
		return nil, nil
	}

	var episodes []types.Episode
	for _, rec := range (*result)[0].Result {
		episodes = append(episodes, types.Episode{
			UUID:      rec.UUID,
			Name:      rec.Name,
			Body:      rec.Body,
			Source:    rec.Source,
			GroupID:   rec.GroupID,
			CreatedAt: rec.CreatedAt,
		})
	}
	return episodes, nil
}

// --- Helpers ---

// entityRecordID resolves an entity UUID to its SurrealDB record ID.
func (s *Store) entityRecordID(ctx context.Context, entityUUID string) (*models.RecordID, error) {
	result, err := surrealdbgo.Query[[]entityRecord](ctx, s.db,
		"SELECT id, uuid FROM entity WHERE uuid = $uuid LIMIT 1",
		map[string]any{"uuid": entityUUID},
	)
	if err != nil || result == nil || len(*result) == 0 || len((*result)[0].Result) == 0 {
		return nil, fmt.Errorf("%w: %s", types.ErrNodeNotFound, entityUUID)
	}
	return (*result)[0].Result[0].ID, nil
}

// factRowsToScoredFacts converts query result rows to ScoredFact slice, deduplicating by UUID.
func (s *Store) factRowsToScoredFacts(rows []factRow) []types.ScoredFact {
	facts := make([]types.ScoredFact, 0, len(rows))
	seen := make(map[string]bool)
	for _, row := range rows {
		if row.RUUID == "" || seen[row.RUUID] {
			continue
		}
		seen[row.RUUID] = true
		facts = append(facts, types.ScoredFact{
			Fact: types.Fact{
				UUID:     row.RUUID,
				Name:     row.RType,
				FactText: row.RFact,
				SourceNode: types.Entity{
					UUID: row.SrcUUID, Name: row.SrcName, Type: row.SrcType, Summary: row.SrcSummary,
				},
				TargetNode: types.Entity{
					UUID: row.TgtUUID, Name: row.TgtName, Type: row.TgtType, Summary: row.TgtSummary,
				},
				CreatedAt: row.RCreatedAt,
				ValidAt:   row.RValidAt,
				InvalidAt: row.RInvalidAt,
			},
			Score: row.Score,
		})
	}
	return facts
}

// nodeRecordsToEntities converts SurrealDB query results to Entity slice.
func nodeRecordsToEntities(result *[]surrealdbgo.QueryResult[[]nodeRecord]) []types.Entity {
	if result == nil || len(*result) == 0 {
		return nil
	}
	var entities []types.Entity
	for _, rec := range (*result)[0].Result {
		entities = append(entities, types.Entity{
			UUID: rec.UUID, Name: rec.Name, Type: rec.Type, Summary: rec.Summary,
		})
	}
	return entities
}
