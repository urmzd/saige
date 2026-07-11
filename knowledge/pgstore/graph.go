package pgstore

import (
	"context"
	"fmt"
	"time"

	"github.com/urmzd/saige/knowledge/types"
)

// GetGraph returns all active relations with their entities for visualization.
func (s *Store) GetGraph(ctx context.Context, limit int64) (*types.GraphData, error) {
	rows, err := s.pool.Query(ctx, graphGetSQL, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	nodeMap := make(map[string]types.GraphNode)
	var edges []types.GraphEdge

	for rows.Next() {
		var (
			aUUID, aName, aType, aSumm string
			rUUID, rType, rFact        string
			rCreatedAt, rValidAt       time.Time
			rInvalidAt                 *time.Time
			bUUID, bName, bType, bSumm string
		)
		if err := rows.Scan(
			&aUUID, &aName, &aType, &aSumm,
			&rUUID, &rType, &rFact, &rCreatedAt, &rValidAt, &rInvalidAt,
			&bUUID, &bName, &bType, &bSumm,
		); err != nil {
			return nil, err
		}

		if _, ok := nodeMap[aUUID]; !ok {
			nodeMap[aUUID] = types.GraphNode{ID: aUUID, Name: aName, Type: aType, Summary: aSumm}
		}
		if _, ok := nodeMap[bUUID]; !ok {
			nodeMap[bUUID] = types.GraphNode{ID: bUUID, Name: bName, Type: bType, Summary: bSumm}
		}
		edges = append(edges, types.GraphEdge{
			ID: rUUID, Source: aUUID, Target: bUUID,
			Type: rType, Fact: rFact, Weight: 1.0,
			CreatedAt: rCreatedAt, ValidAt: rValidAt, InvalidAt: rInvalidAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	nodes := make([]types.GraphNode, 0, len(nodeMap))
	for _, n := range nodeMap {
		nodes = append(nodes, n)
	}

	return &types.GraphData{Nodes: nodes, Edges: edges}, nil
}

// GetNode returns a node with its multi-hop neighbors and edges (BFS).
func (s *Store) GetNode(ctx context.Context, id string, depth int) (*types.NodeDetail, error) {
	if depth < 1 {
		depth = 1
	}

	entity, err := s.GetEntity(ctx, id)
	if err != nil {
		return nil, err
	}

	rootNode := types.GraphNode{
		ID: entity.UUID, Name: entity.Name, Type: entity.Type, Summary: entity.Summary,
	}

	visited := map[string]bool{id: true}
	var allNeighbors []types.GraphNode
	var allEdges []types.GraphEdge
	frontier := []string{id}

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

// GetFactProvenance returns episodes that mention entities involved in a relation.
func (s *Store) GetFactProvenance(ctx context.Context, factUUID string) ([]types.Episode, error) {
	rows, err := s.pool.Query(ctx, graphFactProvenanceSQL, factUUID)
	if err != nil {
		return nil, fmt.Errorf("get fact provenance: %w", err)
	}
	defer rows.Close()

	var episodes []types.Episode
	for rows.Next() {
		var e types.Episode
		var metadata []byte
		if err := rows.Scan(&e.UUID, &e.Name, &e.Body, &e.Source, &e.GroupID, &metadata, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Metadata = decodeEpisodeMetadata(metadata)
		episodes = append(episodes, e)
	}
	return episodes, rows.Err()
}
