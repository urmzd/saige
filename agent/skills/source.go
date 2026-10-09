package skills

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
)

// SkillSource lists the skill packages in one location. Each call returns
// fresh snapshots.
//
// List returns the valid skills together with an error joining a
// *SkillError for each package that failed, so one malformed package does
// not hide the rest. A missing root is not an error: it lists nothing.
type SkillSource interface {
	Name() string
	List(ctx context.Context) ([]Skill, error)
}

// SkillError reports one package a source could not load.
type SkillError struct {
	Source string
	Dir    string
	Err    error
}

func (e *SkillError) Error() string {
	return fmt.Sprintf("skill source %s: %s: %v", e.Source, e.Dir, e.Err)
}

func (e *SkillError) Unwrap() error { return e.Err }

// SourceOption configures a source.
type SourceOption func(*sourceConfig)

type sourceConfig struct {
	name      string
	untrusted bool
	bounds    Bounds
}

// Untrusted marks every skill from the source as untrusted: AllowAll never
// reaches it, so it must be named in an allow list to be used. Use it for a
// location whose contents you do not control, such as a cloned repository.
func Untrusted() SourceOption { return func(c *sourceConfig) { c.untrusted = true } }

// WithSourceName overrides the name reported in SkillMeta.Source.
func WithSourceName(name string) SourceOption { return func(c *sourceConfig) { c.name = name } }

// WithBounds overrides the snapshot bounds for each package.
func WithBounds(b Bounds) SourceOption { return func(c *sourceConfig) { c.bounds = b } }

// DirSource lists skill packages in the immediate subdirectories of a
// directory on disk. Each subdirectory holding a SKILL.md is one package.
//
// Packages are read through os.Root, so no file outside the directory is
// read: a symbolic link that resolves outside it rejects that package.
type DirSource struct {
	root string
	cfg  sourceConfig
}

// NewDirSource returns a source for root. Its default name is root.
func NewDirSource(root string, opts ...SourceOption) *DirSource {
	cfg := sourceConfig{name: root}
	for _, o := range opts {
		o(&cfg)
	}
	return &DirSource{root: root, cfg: cfg}
}

// Name implements SkillSource.
func (s *DirSource) Name() string { return s.cfg.name }

// List implements SkillSource.
func (s *DirSource) List(ctx context.Context) ([]Skill, error) {
	root, err := os.OpenRoot(s.root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("skill source %s: %w", s.cfg.name, err)
	}
	defer func() { _ = root.Close() }()
	return listPackages(ctx, root.FS(), ".", s.cfg, true)
}

// FSSource lists skill packages under a directory of an fs.FS, such as an
// embed.FS compiled into the binary. Symbolic links reject a package,
// because an arbitrary fs.FS may follow them anywhere.
type FSSource struct {
	fsys fs.FS
	root string
	cfg  sourceConfig
}

// NewFSSource returns a source for the packages under root in fsys ("."
// for the top). name identifies it in SkillMeta.Source.
func NewFSSource(name string, fsys fs.FS, root string, opts ...SourceOption) *FSSource {
	cfg := sourceConfig{name: name}
	for _, o := range opts {
		o(&cfg)
	}
	if root == "" {
		root = "."
	}
	return &FSSource{fsys: fsys, root: root, cfg: cfg}
}

// Name implements SkillSource.
func (s *FSSource) Name() string { return s.cfg.name }

// List implements SkillSource.
func (s *FSSource) List(ctx context.Context) ([]Skill, error) {
	return listPackages(ctx, s.fsys, s.root, s.cfg, false)
}

func listPackages(ctx context.Context, fsys fs.FS, root string, cfg sourceConfig, followLinks bool) ([]Skill, error) {
	entries, err := fs.ReadDir(fsys, root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("skill source %s: %w", cfg.name, err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var (
		skills []Skill
		errs   []error
	)
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !e.IsDir() {
			continue
		}
		dir := path.Join(root, e.Name())
		if _, err := fs.Stat(fsys, path.Join(dir, SkillFile)); err != nil {
			continue // not a skill package
		}
		skill, err := Snapshot(fsys, dir, SnapshotOptions{
			Source:      cfg.name,
			Trusted:     !cfg.untrusted,
			Bounds:      cfg.bounds,
			FollowLinks: followLinks,
		})
		if err != nil {
			errs = append(errs, &SkillError{Source: cfg.name, Dir: e.Name(), Err: err})
			continue
		}
		skills = append(skills, skill)
	}
	return skills, errors.Join(errs...)
}

// StandardSources returns the conventional locations in precedence order:
// the project's .agents/skills and .claude/skills, then the user's
// ~/.agents/skills. Either directory may be empty to skip it. Pass
// Untrusted in projectOpts when the project checkout is not trusted.
func StandardSources(projectDir, homeDir string, projectOpts ...SourceOption) []SkillSource {
	var out []SkillSource
	if projectDir != "" {
		out = append(out,
			NewDirSource(filepath.Join(projectDir, ".agents", "skills"), projectOpts...),
			NewDirSource(filepath.Join(projectDir, ".claude", "skills"), projectOpts...),
		)
	}
	if homeDir != "" {
		out = append(out, NewDirSource(filepath.Join(homeDir, ".agents", "skills")))
	}
	return out
}
