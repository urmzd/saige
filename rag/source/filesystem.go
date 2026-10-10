// Package source provides types.Source implementations for fetching documents.
package source

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/urmzd/saige/rag/types"
)

// Reasons a Filesystem reports in types.SkippedSource.Reason.
const (
	SkipToolDir      = "tool-dir"      // a VCS or dependency directory (DefaultSkipDirs)
	SkipHidden       = "hidden"        // a dot file or dot directory
	SkipSecret       = "secret"        // a name on the secret deny list (DefaultDenyPatterns)
	SkipIgnored      = "ignored"       // matched by .gitignore or .saigeignore
	SkipExcluded     = "excluded"      // matched an Exclude glob
	SkipNotIncluded  = "not-included"  // matched no Include glob
	SkipExtension    = "extension"     // not one of Extensions
	SkipNotRecursive = "not-recursive" // a subdirectory of a non-recursive walk
)

// DefaultSkipDirs are directory names a Filesystem never descends into
// unless IncludeToolDirs is set: version control metadata and dependency
// trees, which hold credentials, history, and vendored code rather than
// documents.
var DefaultSkipDirs = []string{".git", ".hg", ".svn", "node_modules"}

// DefaultDenyPatterns are file name globs a Filesystem skips unless
// AllowSecretNames is set, because files with these names usually hold
// credentials. They match the base name, case-insensitively.
var DefaultDenyPatterns = []string{
	".env", ".env.*", "*.env",
	"*.pem", "*.key",
	"id_rsa", "id_rsa.*", "id_dsa", "id_dsa.*", "id_ecdsa", "id_ecdsa.*", "id_ed25519", "id_ed25519.*",
	"*.p12", "*.pfx", "*.kdbx",
	"credentials*.json",
	"*.tfstate", "*.tfstate.*",
	".netrc", "_netrc", ".npmrc", ".pypirc",
}

// IgnoreFiles are the ignore files a Filesystem reads in every directory it
// walks, in this order, unless NoIgnoreFiles is set. A later file's rules
// take precedence, so .saigeignore can re-include what .gitignore drops.
var IgnoreFiles = []string{".gitignore", ".saigeignore"}

// Filesystem fetches documents from a local directory.
//
// By default it skips version control and dependency directories
// (DefaultSkipDirs), dot files and dot directories, files whose names
// suggest credentials (DefaultDenyPatterns), and paths matched by
// .gitignore or .saigeignore files in the walked tree. Rules are checked in
// that order, then Exclude, Include, and Extensions. A skipped directory is
// not descended into, so nothing under it can be re-included.
//
// FetchWithSkips reports what was skipped; SyncSource uses it so that a
// skipped file is never pruned as absent.
type Filesystem struct {
	Dir        string
	Extensions []string // e.g., [".txt", ".html", ".pdf"]. Empty means all files.
	Recursive  bool
	// MaxBytes limits each file. Zero means DefaultMaxBytes and a negative
	// value disables the limit. A larger file fails with an error wrapping
	// ErrTooLarge.
	MaxBytes int64
	// ContinueOnError keeps walking after a file or directory fails. Fetch
	// then returns the documents that succeeded together with the joined
	// per-path errors.
	ContinueOnError bool

	// Include, when set, keeps only files whose path relative to Dir (with
	// forward slashes) matches one of these doublestar globs, such as
	// "docs/**/*.md".
	Include []string
	// Exclude skips files and directories whose path relative to Dir
	// matches one of these doublestar globs, such as "build/**".
	Exclude []string
	// IncludeHidden walks dot files and dot directories. DefaultSkipDirs,
	// the deny list, and ignore files still apply.
	IncludeHidden bool
	// IncludeToolDirs walks DefaultSkipDirs. Their dot names still need
	// IncludeHidden.
	IncludeToolDirs bool
	// AllowSecretNames turns off the DefaultDenyPatterns check.
	AllowSecretNames bool
	// NoIgnoreFiles turns off .gitignore and .saigeignore handling.
	NoIgnoreFiles bool
}

// Fetch walks the directory and returns a RawDocument for each matching file.
// It stops with ctx.Err() as soon as ctx is done.
func (s *Filesystem) Fetch(ctx context.Context) ([]types.RawDocument, error) {
	docs, _, err := s.FetchWithSkips(ctx)
	return docs, err
}

// FetchWithSkips is Fetch that also reports what the rules skipped. A
// skipped directory is one types.SkippedSource with Prefix set, covering
// every path under it.
func (s *Filesystem) FetchWithSkips(ctx context.Context) ([]types.RawDocument, []types.SkippedSource, error) {
	if err := validateGlobs(s.Include, "include"); err != nil {
		return nil, nil, err
	}
	if err := validateGlobs(s.Exclude, "exclude"); err != nil {
		return nil, nil, err
	}

	var docs []types.RawDocument
	var skipped []types.SkippedSource
	var errs []error
	limit := effectiveLimit(s.MaxBytes)
	ignores := ignoreSet{}

	// fail records err and keeps walking under ContinueOnError, or aborts.
	fail := func(err error) error {
		if s.ContinueOnError {
			errs = append(errs, err)
			return nil
		}
		return err
	}
	skip := func(p string, dir bool, reason string) error {
		if dir {
			skipped = append(skipped, types.SkippedSource{SourceURI: p + string(filepath.Separator), Prefix: true, Reason: reason})
			return filepath.SkipDir
		}
		skipped = append(skipped, types.SkippedSource{SourceURI: p, Reason: reason})
		return nil
	}

	walkFn := func(p string, info os.FileInfo, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			if info != nil && info.IsDir() && s.ContinueOnError {
				errs = append(errs, err)
				return filepath.SkipDir
			}
			return fail(err)
		}
		rel, err := filepath.Rel(s.Dir, p)
		if err != nil {
			return fail(err)
		}
		rel = filepath.ToSlash(rel)
		isDir := info.IsDir()
		if rel != "." {
			if reason := s.skipReason(rel, isDir, ignores); reason != "" {
				return skip(p, isDir, reason)
			}
		}
		if isDir {
			if !s.Recursive && rel != "." {
				return skip(p, true, SkipNotRecursive)
			}
			if !s.NoIgnoreFiles {
				if err := ignores.load(p, rel, limit); err != nil {
					return fail(err)
				}
			}
			return nil
		}
		if limit >= 0 && info.Size() > limit {
			return fail(fmt.Errorf("read %s: %w: %d bytes exceeds %d", p, ErrTooLarge, info.Size(), limit))
		}

		data, err := readFile(p, limit)
		if err != nil {
			return fail(fmt.Errorf("read %s: %w", p, err))
		}

		docs = append(docs, types.RawDocument{
			SourceURI:        p,
			MIMEType:         detectMIME(p),
			Data:             data,
			SourceModifiedAt: info.ModTime(),
		})
		return nil
	}

	if err := filepath.Walk(s.Dir, walkFn); err != nil {
		return nil, nil, err
	}
	return docs, skipped, errors.Join(errs...)
}

// skipReason returns the rule that skips rel, a slash-separated path below
// Dir, or "" when it is kept.
func (s *Filesystem) skipReason(rel string, isDir bool, ignores ignoreSet) string {
	name := path.Base(rel)
	if isDir && !s.IncludeToolDirs && containsFold(DefaultSkipDirs, name) {
		return SkipToolDir
	}
	if !s.IncludeHidden && strings.HasPrefix(name, ".") {
		return SkipHidden
	}
	if !isDir && !s.AllowSecretNames && matchAny(DefaultDenyPatterns, strings.ToLower(name)) {
		return SkipSecret
	}
	if !s.NoIgnoreFiles && ignores.ignored(rel, isDir) {
		return SkipIgnored
	}
	if matchAny(s.Exclude, rel) {
		return SkipExcluded
	}
	if isDir {
		return ""
	}
	if len(s.Include) > 0 && !matchAny(s.Include, rel) {
		return SkipNotIncluded
	}
	if !s.matchesExtension(rel) {
		return SkipExtension
	}
	return ""
}

// readFile reads path with the size limit enforced on the bytes actually
// read, which also covers files that grow after the size check.
func readFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // path comes from filepath.Walk of a trusted root
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return readLimited(f, limit)
}

func (s *Filesystem) matchesExtension(path string) bool {
	if len(s.Extensions) == 0 {
		return true
	}
	ext := strings.ToLower(filepath.Ext(path))
	for _, allowed := range s.Extensions {
		if ext == strings.ToLower(allowed) {
			return true
		}
	}
	return false
}

func validateGlobs(globs []string, field string) error {
	for _, g := range globs {
		if !doublestar.ValidatePattern(g) {
			return fmt.Errorf("filesystem source: invalid %s glob %q", field, g)
		}
	}
	return nil
}

// matchAny reports whether name matches one of the doublestar globs. The
// globs are validated up front, so a match error cannot occur.
func matchAny(globs []string, name string) bool {
	for _, g := range globs {
		if ok, _ := doublestar.Match(g, name); ok {
			return true
		}
	}
	return false
}

func containsFold(names []string, name string) bool {
	for _, n := range names {
		if strings.EqualFold(n, name) {
			return true
		}
	}
	return false
}

func detectMIME(path string) string {
	ext := filepath.Ext(path)
	if mimeType := mime.TypeByExtension(ext); mimeType != "" {
		// Strip charset parameters.
		if idx := strings.Index(mimeType, ";"); idx >= 0 {
			mimeType = strings.TrimSpace(mimeType[:idx])
		}
		return mimeType
	}
	// Common fallbacks.
	switch strings.ToLower(ext) {
	case ".txt", ".text":
		return "text/plain"
	case ".html", ".htm":
		return "text/html"
	case ".pdf":
		return "application/pdf"
	case ".md", ".markdown":
		return "text/markdown"
	case ".json":
		return "application/json"
	case ".csv":
		return "text/csv"
	default:
		return "application/octet-stream"
	}
}
