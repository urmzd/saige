// Package skills discovers skill packages (a directory holding a SKILL.md
// and optional resources), snapshots them, and exposes them to an agent
// through three tools with progressive disclosure:
//
//   - Tier 1: each reachable skill's name and description are listed in the
//     system prompt.
//   - Tier 2: load_skill returns a skill's instructions as a tool result,
//     with a manifest of its resource files.
//   - Tier 3: read_skill_resource returns one manifest-listed file.
//
// A skill supplies content. It never grants permission: its allowed-tools
// field can only narrow the tools an agent already has, every call still
// passes the agent's ToolGate, and the skill tools never run a script.
// Running a skill's scripts needs a separately registered execution tool,
// behind its own gates.
//
// Skill packages are copied into an immutable, hashed snapshot when a
// source is listed. Later changes on disk do not reach a running catalog,
// and every resource read is checked against the hash taken at snapshot
// time.
package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

// SkillFile is the name of the file that defines a skill package.
const SkillFile = "SKILL.md"

// Errors returned by sources, catalogs, and the skill tools.
var (
	ErrInvalidSkill      = errors.New("skills: invalid skill")
	ErrSkillNotFound     = errors.New("skills: skill not found")
	ErrResourceNotFound  = errors.New("skills: resource not found")
	ErrResourceTooLarge  = errors.New("skills: resource too large")
	ErrIntegrity         = errors.New("skills: resource does not match its snapshot hash")
	ErrBoundsExceeded    = errors.New("skills: package exceeds snapshot bounds")
	ErrOutsideSkillRoot  = errors.New("skills: path escapes the skill package")
	ErrResourceNotText   = errors.New("skills: resource is not UTF-8 text")
	ErrSkillNotReachable = errors.New("skills: skill not available to this agent")
)

// SkillMeta is what the catalog knows about a skill without reading its
// instructions: the frontmatter fields, where it came from, and its hash.
type SkillMeta struct {
	Name          string `json:"name"`
	Description   string `json:"description"`
	License       string `json:"license,omitempty"`
	Compatibility string `json:"compatibility,omitempty"`
	// AllowedTools lists the tools the skill expects to use. nil means the
	// field was absent. It only ever narrows an agent's tools (see
	// EffectiveTools); it is never a grant or a pre-approval.
	AllowedTools []string          `json:"allowed_tools,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
	// Source names the source the skill was loaded from.
	Source string `json:"source"`
	// Trusted is false for skills from a source marked untrusted. An
	// untrusted skill is reachable only when named explicitly; AllowAll
	// never includes it.
	Trusted bool `json:"trusted"`
	// Hash is "sha256:<hex>" over every file's path and content hash, so a
	// renamed file changes it as much as an edited one.
	Hash string `json:"hash"`
}

// SkillResource is one file in a skill package other than SKILL.md.
type SkillResource struct {
	Path   string `json:"path"` // slash-separated, relative to the package root
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Skill is an immutable snapshot of a skill package.
//
// Copies of a Skill share the snapshot's file contents, which are never
// modified; every accessor returns fresh copies.
type Skill struct {
	SkillMeta
	// Body is the instructions after the frontmatter.
	Body string
	// Resources is the manifest of files other than SKILL.md, sorted by path.
	Resources []SkillResource

	files map[string][]byte
}

// Resource returns the manifest entry for a path.
func (s Skill) Resource(p string) (SkillResource, bool) {
	i, ok := slices.BinarySearchFunc(s.Resources, p, func(r SkillResource, p string) int {
		return strings.Compare(r.Path, p)
	})
	if !ok {
		return SkillResource{}, false
	}
	return s.Resources[i], true
}

// ReadResource returns a copy of one manifest-listed file, at most max
// bytes (max <= 0 means no limit). The path must name a manifest entry
// exactly after cleaning; a path that is absolute or climbs out with ".."
// is rejected before any lookup. The content is re-hashed and compared with
// the manifest before it is returned.
func (s Skill) ReadResource(p string, max int64) ([]byte, SkillResource, error) {
	clean, err := CleanResourcePath(p)
	if err != nil {
		return nil, SkillResource{}, err
	}
	res, ok := s.Resource(clean)
	data, present := s.files[clean]
	if !ok || !present {
		return nil, SkillResource{}, fmt.Errorf("%w: %s in skill %s", ErrResourceNotFound, clean, s.Name)
	}
	if max > 0 && res.Size > max {
		return nil, res, fmt.Errorf("%w: %s is %d bytes, limit %d", ErrResourceTooLarge, clean, res.Size, max)
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != res.SHA256 {
		return nil, res, fmt.Errorf("%w: %s in skill %s", ErrIntegrity, clean, s.Name)
	}
	return slices.Clone(data), res, nil
}

// CleanResourcePath normalizes a resource path from a caller and rejects one
// that could leave the package: absolute paths, backslashes, and any ".."
// that climbs above the root.
func CleanResourcePath(p string) (string, error) {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("%w: %q", ErrOutsideSkillRoot, p)
	}
	clean := path.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || !fs.ValidPath(clean) {
		return "", fmt.Errorf("%w: %q", ErrOutsideSkillRoot, p)
	}
	return clean, nil
}

// Bounds limits what a snapshot copies from one skill package. A package
// that exceeds any bound is rejected whole rather than truncated, because a
// partial package could reference files that are not there.
type Bounds struct {
	MaxFiles      int   // files, including SKILL.md
	MaxFileBytes  int64 // per file
	MaxTotalBytes int64 // all files together
}

// DefaultBounds returns 200 files, 5 MiB per file, and 20 MiB in total.
func DefaultBounds() Bounds {
	return Bounds{MaxFiles: 200, MaxFileBytes: 5 << 20, MaxTotalBytes: 20 << 20}
}

func (b Bounds) withDefaults() Bounds {
	d := DefaultBounds()
	if b.MaxFiles <= 0 {
		b.MaxFiles = d.MaxFiles
	}
	if b.MaxFileBytes <= 0 {
		b.MaxFileBytes = d.MaxFileBytes
	}
	if b.MaxTotalBytes <= 0 {
		b.MaxTotalBytes = d.MaxTotalBytes
	}
	return b
}

// SnapshotOptions describes where a package came from.
type SnapshotOptions struct {
	Source  string
	Trusted bool
	Bounds  Bounds // zero fields use DefaultBounds
	// FollowLinks follows symbolic links to files. Set it only for a
	// filesystem that confines links to its root, such as os.Root.FS;
	// os.DirFS follows a link anywhere. Without it a link rejects the
	// package.
	FollowLinks bool
}

// Snapshot copies the skill package at dir in fsys into an immutable Skill
// and validates it. dir's last element must equal the skill's name.
//
// Every regular file under dir is copied and hashed, within opts.Bounds.
// With opts.FollowLinks a symbolic link to a file is followed through fsys
// (an os.Root-backed filesystem refuses a link that leaves its root, which
// rejects the package); links to directories are never descended, so a
// link cannot create a cycle or pull in another tree.
func Snapshot(fsys fs.FS, dir string, opts SnapshotOptions) (Skill, error) {
	bounds := opts.Bounds.withDefaults()
	files := map[string][]byte{}
	var total int64
	err := fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, dir), "/")
		if dir == "." {
			rel = p
		}
		if d.Type()&fs.ModeSymlink != 0 && !opts.FollowLinks {
			return fmt.Errorf("%w: %s is a symbolic link", ErrOutsideSkillRoot, rel)
		}
		info, err := fs.Stat(fsys, p) // follows a link, within fsys
		if err != nil {
			return fmt.Errorf("%w: %s: %v", ErrOutsideSkillRoot, rel, err)
		}
		if info.IsDir() || !info.Mode().IsRegular() {
			return nil
		}
		if len(files) >= bounds.MaxFiles {
			return fmt.Errorf("%w: more than %d files", ErrBoundsExceeded, bounds.MaxFiles)
		}
		if info.Size() > bounds.MaxFileBytes {
			return fmt.Errorf("%w: %s is %d bytes, limit %d", ErrBoundsExceeded, rel, info.Size(), bounds.MaxFileBytes)
		}
		data, err := readBounded(fsys, p, bounds.MaxFileBytes)
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		total += int64(len(data))
		if total > bounds.MaxTotalBytes {
			return fmt.Errorf("%w: more than %d bytes in total", ErrBoundsExceeded, bounds.MaxTotalBytes)
		}
		files[rel] = data
		return nil
	})
	if err != nil {
		return Skill{}, err
	}
	raw, ok := files[SkillFile]
	if !ok {
		return Skill{}, fmt.Errorf("%w: no %s", ErrInvalidSkill, SkillFile)
	}
	meta, body, err := ParseSkill(raw)
	if err != nil {
		return Skill{}, err
	}
	if base := path.Base(dir); meta.Name != base {
		return Skill{}, fmt.Errorf("%w: name %q does not match directory %q", ErrInvalidSkill, meta.Name, base)
	}

	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	pkg := sha256.New()
	var resources []SkillResource
	for _, p := range paths {
		sum := sha256.Sum256(files[p])
		digest := hex.EncodeToString(sum[:])
		fmt.Fprintf(pkg, "%s\x00%s\n", p, digest)
		if p != SkillFile {
			resources = append(resources, SkillResource{Path: p, Size: int64(len(files[p])), SHA256: digest})
		}
	}
	meta.Source, meta.Trusted = opts.Source, opts.Trusted
	meta.Hash = "sha256:" + hex.EncodeToString(pkg.Sum(nil))
	delete(files, SkillFile)
	return Skill{SkillMeta: meta, Body: body, Resources: resources, files: files}, nil
}

// readBounded reads at most limit bytes and fails if the file is longer,
// which catches a file that grew after it was measured.
func readBounded(fsys fs.FS, p string, limit int64) ([]byte, error) {
	f, err := fsys.Open(p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: larger than %d bytes", ErrBoundsExceeded, limit)
	}
	return data, nil
}

// Validation limits from the skill package format.
const (
	MaxNameLength          = 64
	MaxDescriptionLength   = 1024
	MaxCompatibilityLength = 500
)

var namePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ParseSkill parses and validates a SKILL.md. It does not check the name
// against a directory; Snapshot does.
//
// Required: name (lowercase letters, digits, and single hyphens, at most 64
// characters) and description (at most 1024 characters). Optional: license,
// compatibility (at most 500 characters), metadata (a mapping of strings),
// and allowed-tools (a space-separated string or a list). Other keys are
// ignored, so a header written for another harness still loads.
func ParseSkill(data []byte) (SkillMeta, string, error) {
	if !utf8.Valid(data) {
		return SkillMeta{}, "", fmt.Errorf("%w: %s is not UTF-8", ErrInvalidSkill, SkillFile)
	}
	fm, body, err := ParseSkillFile(data)
	if err != nil {
		return SkillMeta{}, "", err
	}
	var problems []string
	str := func(key string) string {
		v, ok := fm[key]
		if !ok {
			return ""
		}
		s, ok := v.(string)
		if !ok {
			problems = append(problems, key+" must be a string")
		}
		return strings.TrimSpace(s)
	}
	meta := SkillMeta{
		Name:          str("name"),
		Description:   str("description"),
		License:       str("license"),
		Compatibility: str("compatibility"),
	}
	switch {
	case meta.Name == "":
		problems = append(problems, "name is required")
	case len(meta.Name) > MaxNameLength:
		problems = append(problems, fmt.Sprintf("name is longer than %d characters", MaxNameLength))
	case !namePattern.MatchString(meta.Name):
		problems = append(problems, "name must be lowercase letters, digits, and single hyphens, not starting or ending with a hyphen")
	}
	switch {
	case meta.Description == "":
		problems = append(problems, "description is required")
	case utf8.RuneCountInString(meta.Description) > MaxDescriptionLength:
		problems = append(problems, fmt.Sprintf("description is longer than %d characters", MaxDescriptionLength))
	}
	if utf8.RuneCountInString(meta.Compatibility) > MaxCompatibilityLength {
		problems = append(problems, fmt.Sprintf("compatibility is longer than %d characters", MaxCompatibilityLength))
	}
	switch v := fm["metadata"].(type) {
	case nil:
	case map[string]string:
		meta.Metadata = v
	case string:
		if v != "" {
			problems = append(problems, "metadata must be a mapping")
		}
	default:
		problems = append(problems, "metadata must be a mapping")
	}
	switch v := fm["allowed-tools"].(type) {
	case nil:
	case string:
		meta.AllowedTools = strings.Fields(v)
	case []string:
		meta.AllowedTools = v
	default:
		problems = append(problems, "allowed-tools must be a string or a list")
	}
	if len(problems) > 0 {
		return SkillMeta{}, "", fmt.Errorf("%w: %s", ErrInvalidSkill, strings.Join(problems, "; "))
	}
	return meta, body, nil
}

// EffectiveTools returns the agent's tools that a skill's allowed-tools
// permits: the intersection, in ownerTools order. A skill with no
// allowed-tools field leaves the tools unchanged. An entry with an argument
// pattern, such as "Bash(git:*)", cannot be enforced by name and matches
// no tool, so it narrows rather than widens.
func EffectiveTools(ownerTools []string, meta SkillMeta) []string {
	if meta.AllowedTools == nil {
		return slices.Clone(ownerTools)
	}
	out := make([]string, 0, len(ownerTools))
	for _, name := range ownerTools {
		if slices.Contains(meta.AllowedTools, name) {
			out = append(out, name)
		}
	}
	return out
}
