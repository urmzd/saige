package main

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/urmzd/saige/agent/tui"
	"github.com/urmzd/saige/rag"
	"github.com/urmzd/saige/rag/source"
	ragtypes "github.com/urmzd/saige/rag/types"
)

// ragSyncFlags holds the flags of rag sync that shape the filesystem source.
type ragSyncFlags struct {
	dir              string
	extensions       []string
	include, exclude []string
	recursive        bool
	includeHidden    bool
	includeToolDirs  bool
	allowSecretNames bool
	noIgnoreFiles    bool
}

// source builds the filesystem source. The directory is made absolute so
// source URIs, and with them the prune prefix, do not depend on the working
// directory.
func (f *ragSyncFlags) source() (*source.Filesystem, error) {
	if f.dir == "" {
		return nil, errors.New("--dir is required")
	}
	dir, err := filepath.Abs(f.dir)
	if err != nil {
		return nil, err
	}
	return &source.Filesystem{
		Dir:              dir,
		Extensions:       f.extensions,
		Recursive:        f.recursive,
		Include:          f.include,
		Exclude:          f.exclude,
		IncludeHidden:    f.includeHidden,
		IncludeToolDirs:  f.includeToolDirs,
		AllowSecretNames: f.allowSecretNames,
		NoIgnoreFiles:    f.noIgnoreFiles,
	}, nil
}

// ragSyncReport is what rag sync prints: the document UUIDs per outcome,
// and what the filters skipped, with a count per reason.
type ragSyncReport struct {
	Created         []string                 `json:"created"`
	Updated         []string                 `json:"updated"`
	Unchanged       []string                 `json:"unchanged"`
	Pruned          []string                 `json:"pruned"`
	Skipped         []ragtypes.SkippedSource `json:"skipped"`
	SkippedByReason map[string]int           `json:"skipped_by_reason"`
	Failed          []ragSyncFailure         `json:"failed,omitempty"`
}

type ragSyncFailure struct {
	SourceURI string `json:"source_uri"`
	Error     string `json:"error"`
}

func newRagSyncReport(r *ragtypes.SyncResult) ragSyncReport {
	rep := ragSyncReport{
		Created: r.Created, Updated: r.Updated, Unchanged: r.Unchanged, Pruned: r.Pruned,
		Skipped: r.Skipped, SkippedByReason: map[string]int{},
	}
	for _, s := range r.Skipped {
		rep.SkippedByReason[s.Reason]++
	}
	for _, f := range r.Failed {
		rep.Failed = append(rep.Failed, ragSyncFailure{SourceURI: f.SourceURI, Error: f.Err.Error()})
	}
	return rep
}

func newRagSyncCmd(ctx context.Context) *cobra.Command {
	var db, tmplName string
	var prune bool
	var f ragSyncFlags

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Sync a directory into the RAG store",
		Long: `Sync a directory into the RAG store: new files are ingested, changed files
replace their document in place, and with --prune, documents of files that
no longer exist under the directory are deleted.

By default the walk skips .git, .hg, .svn and node_modules, dot files and dot
directories, file names that usually hold credentials (.env*, *.pem, *.key,
id_rsa*, credentials*.json, *.tfstate and similar), and paths matched by
.gitignore or .saigeignore. Skipped files are reported and never pruned.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := tui.ResolveOutput(persistentFlagVars.isJSON(), tui.TemplateByName(tmplName))
			out.Header(tui.OutputHeader{Operation: "rag sync"})

			src, err := f.source()
			if err != nil {
				return reported(out, err)
			}
			opts := ragtypes.SyncOptions{Prune: prune, PrunePrefix: src.Dir + string(filepath.Separator)}

			pipeline, cleanup, err := openRAGPipeline(ctx, db, true)
			if err != nil {
				return reported(out, err)
			}
			defer cleanup()

			result, syncErr := rag.SyncSource(ctx, pipeline, src, opts)
			if result == nil {
				return reported(out, syncErr)
			}
			if err := out.Result(newRagSyncReport(result)); err != nil {
				return reported(out, err)
			}
			if syncErr != nil {
				return reported(out, syncErr)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&db, "db", "", "Postgres DSN [$SAIGE_RAG_DB]")
	cmd.Flags().StringVar(&f.dir, "dir", "", "Directory to sync")
	cmd.Flags().BoolVar(&f.recursive, "recursive", true, "Walk subdirectories")
	cmd.Flags().StringSliceVar(&f.extensions, "ext", nil, "Only these file extensions, e.g. .md,.txt")
	cmd.Flags().StringArrayVar(&f.include, "include", nil, "Only paths (relative to --dir) matching this doublestar glob; repeatable")
	cmd.Flags().StringArrayVar(&f.exclude, "exclude", nil, "Skip paths (relative to --dir) matching this doublestar glob; repeatable")
	cmd.Flags().BoolVar(&f.includeHidden, "include-hidden", false, "Walk dot files and dot directories")
	cmd.Flags().BoolVar(&f.includeToolDirs, "include-tool-dirs", false, "Walk .git, .hg, .svn and node_modules (dot names also need --include-hidden)")
	cmd.Flags().BoolVar(&f.allowSecretNames, "allow-secret-names", false, "Do not skip file names that usually hold credentials")
	cmd.Flags().BoolVar(&f.noIgnoreFiles, "no-ignore-files", false, "Do not read .gitignore and .saigeignore")
	cmd.Flags().BoolVar(&prune, "prune", false, "Delete documents of files that no longer exist under --dir")
	cmd.Flags().StringVar(&tmplName, "template", "default", "Output template (default|minimal|detailed)")

	return cmd
}
