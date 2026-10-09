package pgstore

import (
	"context"
	"fmt"
	"time"

	"github.com/urmzd/saige/rag/knowledge/types"
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
//
// Each hop runs one query for the whole frontier. Edges are deduplicated by
// relation UUID, and only edges whose endpoints are both admitted nodes are
// returned. The traversal stops at the store's node and edge caps (see
// WithTraversalLimits) and then sets NodeDetail.Truncated. A failed hop
// query fails the call instead of returning a silently partial graph.
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
	seenEdges := make(map[string]bool)
	allNeighbors := []types.GraphNode{}
	allEdges := []types.GraphEdge{}
	frontier := []string{id}
	truncated := false

	for d := 0; d < depth && len(frontier) > 0 && !truncated; d++ {
		remaining := s.maxEdges - len(allEdges)
		edges, nodes, err := s.neighborsOf(ctx, frontier, seenEdges, remaining+1)
		if err != nil {
			return nil, fmt.Errorf("get node %s: hop %d: %w", id, d+1, err)
		}
		if len(edges) > remaining {
			edges = edges[:remaining]
			truncated = true
		}

		var nextFrontier []string
		for _, e := range edges {
			if seenEdges[e.ID] {
				continue
			}
			admitted := true
			for _, end := range []string{e.Source, e.Target} {
				if visited[end] {
					continue
				}
				if len(allNeighbors) >= s.maxNodes {
					admitted = false
					truncated = true
					break
				}
				visited[end] = true
				allNeighbors = append(allNeighbors, nodes[end])
				nextFrontier = append(nextFrontier, end)
			}
			if !admitted {
				continue
			}
			seenEdges[e.ID] = true
			allEdges = append(allEdges, e)
		}
		frontier = nextFrontier
	}

	return &types.NodeDetail{Node: rootNode, Neighbors: allNeighbors, Edges: allEdges, Truncated: truncated}, nil
}

// neighborsOf returns up to limit active edges touching any frontier entity,
// newest first, with the nodes at both ends keyed by UUID. Edges already in
// seen are excluded so they do not count against the limit again.
func (s *Store) neighborsOf(ctx context.Context, frontier []string, seen map[string]bool, limit int) ([]types.GraphEdge, map[string]types.GraphNode, error) {
	exclude := make([]string, 0, len(seen))
	for id := range seen {
		exclude = append(exclude, id)
	}
	rows, err := s.pool.Query(ctx, relationNeighborsBatchSQL, frontier, limit, exclude)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var edges []types.GraphEdge
	nodes := make(map[string]types.GraphNode)
	for rows.Next() {
		var (
			rUUID, rType, rFact        string
			rCreatedAt, rValidAt       time.Time
			rInvalidAt                 *time.Time
			sUUID, sName, sType, sSumm string
			tUUID, tName, tType, tSumm string
		)
		if err := rows.Scan(&rUUID, &rType, &rFact, &rCreatedAt, &rValidAt, &rInvalidAt,
			&sUUID, &sName, &sType, &sSumm, &tUUID, &tName, &tType, &tSumm); err != nil {
			return nil, nil, err
		}
		nodes[sUUID] = types.GraphNode{ID: sUUID, Name: sName, Type: sType, Summary: sSumm}
		nodes[tUUID] = types.GraphNode{ID: tUUID, Name: tName, Type: tType, Summary: tSumm}
		edges = append(edges, types.GraphEdge{
			ID: rUUID, Source: sUUID, Target: tUUID,
			Type: rType, Fact: rFact, Weight: 1.0,
			CreatedAt: rCreatedAt, ValidAt: rValidAt, InvalidAt: rInvalidAt,
		})
	}
	return edges, nodes, rows.Err()
}

// GetFactProvenance returns the episodes that asserted a relation, oldest
// first. For a relation with no recorded episode links (written before links
// existed), it returns the episodes that mention both of its endpoints.
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
		if err := rows.Scan(&e.UUID, &e.Name, &e.Body, &e.Source, &e.GroupID, &e.DocumentID, &metadata, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Metadata = decodeEpisodeMetadata(metadata)
		episodes = append(episodes, e)
	}
	return episodes, rows.Err()
}
