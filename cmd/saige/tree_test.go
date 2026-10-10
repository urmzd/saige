package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/urmzd/saige/agent/tree"
)

const legacyTestdata = "../../agent/internal/testdata/legacy/"

func copyLegacy(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(legacyTestdata + name)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), filepath.Base(name))
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTreeMigrate(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		file, kind string
	}{
		{"tree/v1_tree.json", "tree"},
		{"filewal/v1.wal", "wal"},
	} {
		t.Run(tt.kind, func(t *testing.T) {
			path := copyLegacy(t, tt.file)
			before, _ := os.ReadFile(path)
			n, kind, err := migrateTree(ctx, path, false)
			if err != nil || kind != tt.kind || n == 0 {
				t.Fatalf("dry run = %d %s %v", n, kind, err)
			}
			if after, _ := os.ReadFile(path); !bytes.Equal(after, before) {
				t.Fatal("dry run changed the file")
			}
			if m, _, err := migrateTree(ctx, path, true); err != nil || m != n {
				t.Fatalf("migration = %d, %v; want %d", m, err, n)
			}
			if m, _, err := migrateTree(ctx, path, false); err != nil || m != 0 {
				t.Fatalf("after migration = %d, %v", m, err)
			}
			if tt.kind == "tree" {
				raw, _ := os.ReadFile(path)
				tr := &tree.Tree{}
				if err := tr.UnmarshalJSON(raw); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestTreeMigrateCommand(t *testing.T) {
	path := copyLegacy(t, "tree/v1_tree.json")
	var out bytes.Buffer
	cmd := newTreeCmd(context.Background())
	cmd.SetArgs([]string{"migrate", path})
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "tree: would rewrite 7 node messages (format version 2)\n" {
		t.Fatalf("output = %q", got)
	}
}
