package filewal_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/urmzd/saige/agent/store/filewal"
	"github.com/urmzd/saige/agent/store/memstore"
	"github.com/urmzd/saige/agent/store/walrecover"
	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
)

// TestLegacyWALMigrate recovers a log the release before typed parts
// wrote, migrates its messages and checks that recovery reads the same.
func TestLegacyWALMigrate(t *testing.T) {
	ctx := context.Background()
	golden, err := os.ReadFile("../../internal/testdata/legacy/filewal/v1.wal")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "v1.wal")
	if err := os.WriteFile(path, golden, 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := filewal.New(filewal.Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close(ctx) })

	before, err := w.RecoverOps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	nodes := 0
	for _, tx := range before {
		for _, op := range tx.Ops {
			if op.Node != nil {
				nodes++
			}
		}
	}
	if nodes == 0 {
		t.Fatal("golden log has no node ops")
	}

	// The recovered ops rebuild the tree.
	store := memstore.New()
	for _, tx := range before {
		if err := store.Tx(ctx, func(s types.StoreTx) error { return tree.ApplyOps(ctx, s, tx.Ops) }); err != nil {
			t.Fatal(err)
		}
	}
	var rootID types.NodeID
	for _, op := range before[0].Ops {
		if op.Node != nil && op.Node.ParentID == "" {
			rootID = op.Node.ID
		}
	}
	tr, err := tree.LoadFromStore(ctx, store, rootID, "main")
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := tr.FlattenBranch("main")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 5 {
		t.Fatalf("main branch = %d messages, want 5", len(msgs))
	}
	if _, ok := msgs[1].(types.UserMessage).Parts[1].(types.ImagePart); !ok {
		t.Fatalf("attachment = %T, want an image part", msgs[1].(types.UserMessage).Parts[1])
	}

	n, err := w.MigrateMessages(ctx, true)
	if err != nil || n != nodes {
		t.Fatalf("dry run = %d, %v; want %d", n, err, nodes)
	}
	if raw, _ := os.ReadFile(path); !bytes.Equal(raw, golden) {
		t.Fatal("dry run wrote the log")
	}
	if n, err = w.MigrateMessages(ctx, false); err != nil || n != nodes {
		t.Fatalf("migration = %d, %v; want %d", n, err, nodes)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(`"message":{"content"`)) {
		t.Fatal("log still holds version 1 messages")
	}
	if n, err = w.MigrateMessages(ctx, false); err != nil || n != 0 {
		t.Fatalf("second pass = %d, %v", n, err)
	}

	after, err := w.RecoverOps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatal("recovered ops changed across the migration")
	}

	// The log still appends and recovers after the rewrite.
	tx, err := w.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Commit(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if _, err := walrecover.RecoverWAL(ctx, w, memstore.New()); err != nil {
		t.Fatal(err)
	}
}
