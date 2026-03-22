package knowledge

import "github.com/urmzd/saige/knowledge/types"

// FactsToStrings converts facts to string representations.
func FactsToStrings(facts []types.Fact) []string {
	result := make([]string, len(facts))
	for i, f := range facts {
		result[i] = f.SourceNode.Name + " -> " + f.TargetNode.Name + ": " + f.FactText
	}
	return result
}

// FilterFacts filters facts by a predicate.
func FilterFacts(facts []types.Fact, pred func(types.Fact) bool) []types.Fact {
	result := make([]types.Fact, 0)
	for _, f := range facts {
		if pred(f) {
			result = append(result, f)
		}
	}
	return result
}

// FilterByType returns facts with a matching relation type.
func FilterByType(facts []types.Fact, relType string) []types.Fact {
	return FilterFacts(facts, func(f types.Fact) bool {
		return f.Name == relType
	})
}

// Subgraph collects a subgraph around a starting node.
func Subgraph(detail *types.NodeDetail) *types.GraphData {
	nodes := make([]types.GraphNode, 0, len(detail.Neighbors)+1)
	nodes = append(nodes, detail.Node)
	nodes = append(nodes, detail.Neighbors...)
	return &types.GraphData{Nodes: nodes, Edges: detail.Edges}
}
