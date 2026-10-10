package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/urmzd/saige/internal/must"
	"github.com/urmzd/saige/rag"
	"github.com/urmzd/saige/rag/pgstore"
	"github.com/urmzd/saige/rag/source"
	ragtypes "github.com/urmzd/saige/rag/types"
)

func TestRagSyncFlagsSource(t *testing.T) {
	if _, err := (&ragSyncFlags{}).source(); err == nil {
		t.Fatal("missing --dir accepted")
	}
	f := ragSyncFlags{dir: ".", include: []string{"docs/**"}, exclude: []string{"build/**"}, recursive: true, includeHidden: true, noIgnoreFiles: true}
	src, err := f.source()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(src.Dir) || src.Include[0] != "docs/**" || src.Exclude[0] != "build/**" ||
		!src.Recursive || !src.IncludeHidden || !src.NoIgnoreFiles || src.AllowSecretNames || src.IncludeToolDirs {
		t.Fatalf("source = %+v", src)
	}
}

func TestRagSyncCmdFlags(t *testing.T) {
	cmd := newRagSyncCmd(context.Background())
	for _, name := range []string{"dir", "include", "exclude", "include-hidden", "include-tool-dirs", "allow-secret-names", "no-ignore-files", "ext", "recursive", "prune"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("rag sync has no --%s flag", name)
		}
	}
}

func TestRagSyncReportCountsSkips(t *testing.T) {
	rep := newRagSyncReport(&ragtypes.SyncResult{
		Created: []string{"d1"},
		Skipped: []ragtypes.SkippedSource{{SourceURI: "/r/.env", Reason: source.SkipHidden}, {SourceURI: "/r/a.pem", Reason: source.SkipSecret}, {SourceURI: "/r/b.key", Reason: source.SkipSecret}},
		Failed:  []ragtypes.SyncError{{SourceURI: "/r/x.md", Err: errors.New("boom")}},
	})
	if rep.SkippedByReason[source.SkipSecret] != 2 || rep.SkippedByReason[source.SkipHidden] != 1 {
		t.Fatalf("skipped by reason = %v", rep.SkippedByReason)
	}
	if len(rep.Failed) != 1 || rep.Failed[0].Error != "boom" {
		t.Fatalf("failed = %+v", rep.Failed)
	}
}

// TestRAGSyncSkipsSecretsAndKeepsSkippedDocuments runs the source rag sync
// builds against Postgres: secrets and VCS files are not ingested, and a
// file a new --exclude now skips keeps its document under --prune.
func TestRAGSyncSkipsSecretsAndKeepsSkippedDocuments(t *testing.T) {
	pool := ragTestPool(t)
	ctx := context.Background()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"guide.md":      "The okapi is a forest giraffe.",
		"notes/kiwi.md": "The kiwi is a flightless bird.",
		".env":          "API_KEY=do-not-index",
		"server.pem":    "-----BEGIN PRIVATE KEY-----",
		".git/config":   "[remote]",
		"build/out.md":  "generated",
		".gitignore":    "build/\n",
	} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pipe, err := rag.New(rag.Config{}, ragPipelineOptions(must.Get(pgstore.New(pgstore.Config{Pool: pool})), hashEmbedder())...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pipe.Close(ctx) }()

	flags := ragSyncFlags{dir: dir, recursive: true}
	sync := func() *ragtypes.SyncResult {
		t.Helper()
		src, err := flags.source()
		if err != nil {
			t.Fatal(err)
		}
		r, err := rag.SyncSource(ctx, pipe, src, ragtypes.SyncOptions{Prune: true, PrunePrefix: src.Dir + string(filepath.Separator)})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	first := sync()
	if len(first.Created) != 2 {
		t.Fatalf("first sync created %d documents, want guide.md and notes/kiwi.md: %+v", len(first.Created), first)
	}
	if rep := newRagSyncReport(first); rep.SkippedByReason[source.SkipSecret] != 1 ||
		rep.SkippedByReason[source.SkipIgnored] != 1 || rep.SkippedByReason[source.SkipToolDir] != 1 {
		t.Fatalf("skipped = %v", rep.SkippedByReason)
	}

	flags.exclude = []string{"notes/**"}
	second := sync()
	if len(second.Pruned) != 0 || len(second.Unchanged) != 1 {
		t.Fatalf("second sync = %+v; an excluded file must not be pruned", second)
	}
}
