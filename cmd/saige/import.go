package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/urmzd/saige/agent/skills"
)

// Formats saige import reads.
const (
	importClaude = "claude"
	importCodex  = "codex"
	importCursor = "cursor"
)

// importFormats is every format, in the order import reads them: a name
// defined by an earlier format wins.
var importFormats = []string{importClaude, importCodex, importCursor}

// importedByValue marks skills saige import wrote, so a later import may
// replace them and never a skill someone else wrote.
const importedByValue = "saige import"

// importTarget is where imported skills go, relative to the import base.
const importTarget = ".agents/skills"

type importFlags struct {
	dir    string
	user   bool
	dryRun bool
	diff   bool
	force  bool
}

func newImportCmd(ctx context.Context) *cobra.Command {
	var f importFlags
	cmd := &cobra.Command{
		Use:       "import [FORMAT...]",
		Short:     "Convert Claude Code skills, Codex skills and Cursor rules into .agents/skills",
		ValidArgs: importFormats,
		Long: `Import converts skills and rules written for other tools into the canonical
layout, .agents/skills/NAME/SKILL.md, the inverse of saige export skills.
FORMAT is any of claude, codex and cursor; with none, import reads all three:

  claude  .claude/skills/NAME/SKILL.md, with its resource files
  codex   .codex/skills/NAME/SKILL.md, with its resource files
  cursor  .cursor/rules/**/*.mdc and .cursorrules, one skill per rule

A Cursor rule is named after its path under .cursor/rules, with directories
joined by hyphens, and keeps its globs and alwaysApply in the skill's
metadata. Only the agentskills.io frontmatter fields are written; keys
another harness added are dropped. The originals are left in place.

Import marks what it writes (metadata generated-by: saige import). Running
it again changes nothing unless a source changed, and a skill in
.agents/skills that import did not write is kept unless --force is given.
--user reads ~/.claude/skills and $CODEX_HOME/skills (default
~/.codex/skills) into ~/.agents/skills. --dry-run prints what would change
and writes nothing.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			plan, base, err := planImport(ctx, args, f)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			plan.report(w, base, f.dryRun, f.dryRun || f.diff)
			if f.dryRun {
				return nil
			}
			return plan.apply()
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.dir, "dir", ".", "Project directory to read from and write .agents/skills in")
	fl.BoolVar(&f.user, "user", false, "Import the user-level skills into ~/.agents/skills instead")
	fl.BoolVar(&f.dryRun, "dry-run", false, "Print what would change, with diffs, and write nothing")
	fl.BoolVar(&f.diff, "diff", false, "Print the diff of every changed file")
	fl.BoolVar(&f.force, "force", false, "Replace a skill of the same name that saige import did not write")
	return cmd
}

// importSource is one location import reads.
type importSource struct {
	format string
	src    skills.SkillSource
	// dir holds one package directory per skill; "" for Cursor rules.
	dir string
}

// importSources returns the locations to read for formats, in
// importFormats order.
func importSources(plan *exportPlan, base string, user bool, formats []string) []importSource {
	codexHome := filepath.Join(base, ".codex")
	if user {
		if env := os.Getenv("CODEX_HOME"); env != "" {
			codexHome = env
		}
	}
	var out []importSource
	for _, format := range importFormats {
		if !slices.Contains(formats, format) {
			continue
		}
		switch format {
		case importClaude:
			dir := filepath.Join(base, ".claude", "skills")
			out = append(out, importSource{format, skills.NewDirSource(dir), dir})
		case importCodex:
			dir := filepath.Join(codexHome, "skills")
			out = append(out, importSource{format, skills.NewDirSource(dir), dir})
		case importCursor:
			if user {
				plan.note("Cursor keeps user rules in its settings, not in files: nothing to import for cursor with --user")
				continue
			}
			out = append(out, importSource{format, skills.NewCursorRulesSource(base), ""})
		}
	}
	return out
}

// planImport builds the plan for formats (every format when empty).
func planImport(ctx context.Context, formats []string, f importFlags) (*exportPlan, string, error) {
	for _, format := range formats {
		if !slices.Contains(importFormats, format) {
			return nil, "", invalidInput(fmt.Errorf("unknown format %q: use one of %s", format, strings.Join(importFormats, ", ")))
		}
	}
	if len(formats) == 0 {
		formats = importFormats
	}
	base, err := filepath.Abs(f.dir)
	if err != nil {
		return nil, "", err
	}
	if f.user {
		if base, err = os.UserHomeDir(); err != nil {
			return nil, "", err
		}
	}
	target := filepath.Join(base, filepath.FromSlash(importTarget))
	plan := &exportPlan{}
	origin := map[string]string{} // skill name to where it came from
	found := 0
	for _, is := range importSources(plan, base, f.user, formats) {
		list, err := is.src.List(ctx)
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		for _, e := range unjoin(err) {
			plan.note("skipped: %v", e)
		}
		for _, sk := range list {
			found++
			from := displayPath(base, filepath.Join(is.dir, sk.Name))
			if is.dir == "" {
				from = sk.Metadata[skills.MetaCursorRule]
			}
			if first, ok := origin[sk.Name]; ok {
				plan.note("%s: %s is shadowed by %s", sk.Name, from, first)
				continue
			}
			origin[sk.Name] = from
			dest := filepath.Join(target, sk.Name)
			if is.dir != "" && sameDir(filepath.Join(is.dir, sk.Name), dest) {
				continue // the package already lives in .agents/skills
			}
			existing, err := plan.current(filepath.Join(dest, skills.SkillFile))
			if err != nil {
				return nil, "", err
			}
			if existing != nil && !f.force && skillWriter(existing) != importedByValue {
				plan.note("kept %s: saige import did not write it; pass --force to replace it with %s",
					displayPath(base, dest), from)
				continue
			}
			if err := planSkillPackage(plan, dest, sk, importedByValue); err != nil {
				return nil, "", err
			}
		}
	}
	if found == 0 {
		plan.note("no skills or rules found for %s", strings.Join(formats, ", "))
	}
	return plan, base, nil
}

// unjoin splits an errors.Join result into its errors.
func unjoin(err error) []error {
	if err == nil {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		return joined.Unwrap()
	}
	return []error{err}
}

// sameDir reports whether a and b resolve to the same existing directory,
// as when .claude/skills links to .agents/skills.
func sameDir(a, b string) bool {
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		return false
	}
	rb, err := filepath.EvalSymlinks(b)
	return err == nil && ra == rb
}
