package main

import (
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestConsolidatedCommands(t *testing.T) {
	root := newRootCmd(context.Background())
	for _, path := range []string{"agent run", "evals run", "evals list", "knowledge search", "knowledge show", "knowledge graph export", "knowledge graph neighbors"} {
		parts := strings.Fields(path)
		cmd, rest, err := root.Find(parts)
		if err != nil || len(rest) != 0 || cmd.Name() != parts[len(parts)-1] {
			t.Errorf("%s: %v %v", path, rest, err)
		}
	}
	for _, name := range []string{"chat", "ask", "rag", "kg", "eval"} {
		if _, _, err := root.Find([]string{name}); err == nil {
			t.Errorf("legacy command %s still registered", name)
		}
	}
}

func TestKnowledgeBackendDispatch(t *testing.T) {
	for _, backend := range []string{"rag", "kg"} {
		t.Run(backend, func(t *testing.T) {
			called := ""
			makeBackend := func(name string) *cobra.Command {
				var query string
				c := &cobra.Command{Use: "search", RunE: func(*cobra.Command, []string) error { called = name + ":" + query; return nil }}
				c.Flags().StringVar(&query, "query", "", "query")
				return c
			}
			c := knowledgeOperation("search", makeBackend("rag"), makeBackend("kg"))
			c.SetArgs([]string{"--backend", backend, "--query", "needle"})
			if err := c.Execute(); err != nil {
				t.Fatal(err)
			}
			if called != backend+":needle" {
				t.Fatalf("dispatch %q", called)
			}
		})
	}
}

func TestKnowledgeRejectsUnsupportedOperations(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"show", "--backend", "kg"}, "does not support"},
		{[]string{"search", "--backend", "unknown"}, "unknown knowledge backend"},
		{[]string{"ingest", "--backend", "kg", "--file", "paper.pdf"}, "--file is not supported"},
		{[]string{"ingest", "--name", "episode"}, "--name is not supported"},
	} {
		c := newKnowledgeCmd(context.Background())
		c.SilenceUsage = true
		c.SilenceErrors = true
		c.SetArgs(tt.args)
		if err := c.Execute(); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%v: %v", tt.args, err)
		}
	}
}
