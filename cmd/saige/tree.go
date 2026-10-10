package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/urmzd/saige/agent/pgstore"
	"github.com/urmzd/saige/agent/store/filewal"
	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
)

func newTreeCmd(ctx context.Context) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tree",
		Short: "Maintain stored conversation trees",
	}
	cmd.AddCommand(newTreeMigrateCmd(ctx))
	return cmd
}

func newTreeMigrateCmd(ctx context.Context) *cobra.Command {
	var write bool
	cmd := &cobra.Command{
		Use:   "migrate TARGET",
		Short: "Rewrite stored node messages in the current format",
		Long: "Count, and with --write rewrite, the node messages an older release stored,\n" +
			"in the current message format. TARGET is a PostgreSQL URL (every conversation\n" +
			"in agent_node), a tree JSON document, or a file WAL.\n\n" +
			"This is optional: reads convert older messages on the fly. Nothing writes the\n" +
			"older format and there is no way back, so take a snapshot or copy first.\n" +
			"Stop writers of a file WAL while it is rewritten.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			n, kind, err := migrateTree(ctx, args[0], write)
			if err != nil {
				return err
			}
			verb := "would rewrite"
			if write {
				verb = "rewrote"
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s: %s %d node messages (format version %d)\n", kind, verb, n, tree.MessageFormatVersion)
			return err
		},
	}
	cmd.Flags().BoolVar(&write, "write", false, "Rewrite the messages (default: count them)")
	return cmd
}

// migrateTree migrates the store target names and reports how many
// messages were (or would be) rewritten and what kind of store it was.
func migrateTree(ctx context.Context, target string, write bool) (int, string, error) {
	if isPostgresStore(target) {
		pool, err := connectPostgres(ctx, target)
		if err != nil {
			return 0, "", err
		}
		defer pool.Close()
		res, err := pgstore.MigrateAllMessages(ctx, pool, !write)
		if write {
			return res.Rewritten, "postgres", err
		}
		return res.Outdated, "postgres", err
	}
	raw, err := os.ReadFile(target) // #nosec G304 -- the operator names the file to migrate.
	if err != nil {
		return 0, "", err
	}
	if isTreeDocument(raw) {
		n, err := migrateTreeFile(target, raw, write)
		return n, "tree", err
	}
	w, err := filewal.New(filewal.Config{Path: target})
	if err != nil {
		return 0, "", err
	}
	n, err := w.MigrateMessages(ctx, !write)
	if cerr := w.Close(ctx); err == nil {
		err = cerr
	}
	return n, "wal", err
}

// isTreeDocument reports whether raw is one JSON tree document rather than
// a WAL, which holds one record per line.
func isTreeDocument(raw []byte) bool {
	var doc map[string]json.RawMessage
	if json.Unmarshal(raw, &doc) != nil {
		return false
	}
	_, ok := doc["root_id"]
	return ok
}

func migrateTreeFile(path string, raw []byte, write bool) (int, error) {
	var doc struct {
		Nodes []struct {
			Role    types.Role      `json:"role"`
			Message json.RawMessage `json:"message"`
		} `json:"nodes"`
		Content []struct {
			Role    types.Role      `json:"role"`
			Message json.RawMessage `json:"message"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return 0, err
	}
	n := 0
	for _, node := range append(doc.Nodes, doc.Content...) {
		_, changed, err := tree.MigrateMessage(node.Role, node.Message)
		if err != nil {
			return 0, err
		}
		if changed {
			n++
		}
	}
	if !write || n == 0 {
		return n, nil
	}
	tr := &tree.Tree{}
	if err := json.Unmarshal(raw, tr); err != nil {
		return 0, err
	}
	out, err := json.Marshal(tr)
	if err != nil {
		return 0, err
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, out, "", "  "); err != nil {
		return 0, err
	}
	indented.WriteByte('\n')
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return n, replaceFile(path, indented.Bytes(), info.Mode().Perm())
}

// replaceFile replaces path with data through a synced temporary file.
func replaceFile(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".migrate-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, err = io.Copy(tmp, bytes.NewReader(data))
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(name, perm)
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		_ = os.Remove(name)
	}
	return err
}
