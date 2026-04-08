// Package graph provides formatting utilities for knowledge graph visualization.
package graph

import (
	"fmt"
	"strings"

	kgtypes "github.com/urmzd/saige/knowledge/types"
)

// ToDOT converts GraphData into Graphviz DOT format.
func ToDOT(data *kgtypes.GraphData) string {
	var b strings.Builder
	b.WriteString("digraph knowledge {\n")
	b.WriteString("  rankdir=LR;\n")
	b.WriteString("  node [shape=box, style=\"rounded,filled\", fillcolor=\"#f0f0f0\", fontname=\"Helvetica\"];\n")
	b.WriteString("  edge [fontname=\"Helvetica\", fontsize=10];\n\n")

	for _, n := range data.Nodes {
		label := escDOT(n.Name)
		if n.Type != "" {
			label = fmt.Sprintf("%s\\n[%s]", escDOT(n.Name), escDOT(n.Type))
		}
		fmt.Fprintf(&b, "  %q [label=%q];\n", n.ID, label)
	}

	b.WriteString("\n")

	for _, e := range data.Edges {
		label := escDOT(e.Type)
		fmt.Fprintf(&b, "  %q -> %q [label=%q];\n", e.Source, e.Target, label)
	}

	b.WriteString("}\n")
	return b.String()
}

// ToText converts GraphData into a human/AI-readable text summary.
func ToText(data *kgtypes.GraphData) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Knowledge Graph: %d nodes, %d edges\n\n", len(data.Nodes), len(data.Edges))

	b.WriteString("## Entities\n")
	for _, n := range data.Nodes {
		fmt.Fprintf(&b, "- %s", n.Name)
		if n.Type != "" {
			fmt.Fprintf(&b, " (%s)", n.Type)
		}
		if n.Summary != "" {
			fmt.Fprintf(&b, ": %s", n.Summary)
		}
		b.WriteString("\n")
	}

	b.WriteString("\n## Relations\n")
	names := make(map[string]string, len(data.Nodes))
	for _, n := range data.Nodes {
		names[n.ID] = n.Name
	}
	for _, e := range data.Edges {
		src := names[e.Source]
		tgt := names[e.Target]
		if src == "" {
			src = e.Source
		}
		if tgt == "" {
			tgt = e.Target
		}
		fmt.Fprintf(&b, "- %s -[%s]-> %s", src, e.Type, tgt)
		if e.Fact != "" {
			fmt.Fprintf(&b, ": %s", e.Fact)
		}
		b.WriteString("\n")
	}

	return b.String()
}

func escDOT(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}
