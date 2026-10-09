package eval

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"time"
)

// Provenance records what produced a run, so two runs can be told apart
// after the fact: the code revision, whether it had uncommitted changes, the
// models it called, and the toolchain. It is a snapshot taken when the run
// starts; later edits to the working tree do not change it.
type Provenance struct {
	// GitCommit is the full commit hash of the checkout, empty when unknown.
	GitCommit string `json:"git_commit,omitempty"`
	// GitDirty reports uncommitted changes, untracked files included, at
	// capture time. It is meaningful only when GitCommit is set.
	GitDirty bool `json:"git_dirty,omitempty"`
	// Models lists the models the run called, sorted and deduplicated.
	Models []string `json:"models,omitempty"`
	// Module and ModuleVersion identify the main module of the binary.
	Module        string    `json:"module,omitempty"`
	ModuleVersion string    `json:"module_version,omitempty"`
	GoVersion     string    `json:"go_version,omitempty"`
	CapturedAt    time.Time `json:"captured_at,omitzero"`
	// Extra holds caller-defined facts, such as a dataset revision.
	Extra map[string]string `json:"extra,omitempty"`
	// Catalog records the declared configurations that served the run, when
	// its providers were built from a model catalog.
	Catalog *CatalogProvenance `json:"catalog,omitempty"`
}

// CatalogProvenance identifies the catalog configurations behind a run.
type CatalogProvenance struct {
	// Revision is the catalog revision. Several are joined with ", ".
	Revision string `json:"revision,omitempty"`
	// Presets lists the presets that served, sorted.
	Presets []string `json:"presets,omitempty"`
	// Configs maps each serving profile ID to its configuration hash.
	Configs map[string]string `json:"configs,omitempty"`
}

// AddRoute records that a catalog profile served part of the run. Calls with
// an empty profile or hash are ignored.
func (p *Provenance) AddRoute(profile, preset, configHash, revision string) {
	if profile == "" || configHash == "" {
		return
	}
	if p.Catalog == nil {
		p.Catalog = &CatalogProvenance{}
	}
	c := p.Catalog
	if c.Configs == nil {
		c.Configs = map[string]string{}
	}
	c.Configs[profile] = configHash
	if preset != "" && !slices.Contains(c.Presets, preset) {
		c.Presets = append(c.Presets, preset)
		slices.Sort(c.Presets)
	}
	if revision != "" && !slices.Contains(strings.Split(c.Revision, ", "), revision) {
		if c.Revision != "" {
			c.Revision += ", "
		}
		c.Revision += revision
	}
}

// ConfigDrift lists the differences between two runs' catalog
// configurations that make their scores incomparable: the same profile ID
// served with a different configuration hash, or a different catalog
// revision. It is empty when either run has no catalog provenance.
func ConfigDrift(base, exp Provenance) []string {
	if base.Catalog == nil || exp.Catalog == nil {
		return nil
	}
	var out []string
	if base.Catalog.Revision != exp.Catalog.Revision {
		out = append(out, fmt.Sprintf("catalog revision differs: %q vs %q", base.Catalog.Revision, exp.Catalog.Revision))
	}
	ids := make([]string, 0, len(base.Catalog.Configs))
	for id := range base.Catalog.Configs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		if h, ok := exp.Catalog.Configs[id]; ok && h != base.Catalog.Configs[id] {
			out = append(out, fmt.Sprintf("profile %s: configuration %s vs %s", id, base.Catalog.Configs[id], h))
		}
	}
	return out
}

// CaptureProvenance snapshots the provenance of a run started from dir (the
// current directory when empty). The commit and dirty flag come from git when
// it is installed and dir is inside a repository, and otherwise from the VCS
// stamp of the running binary, if it has one. Failure to find either leaves
// GitCommit empty; it is not an error.
func CaptureProvenance(ctx context.Context, dir string, models ...string) Provenance {
	p := Provenance{
		GoVersion:  runtime.Version(),
		CapturedAt: time.Now().UTC(),
	}
	p.AddModels(models...)

	info, hasInfo := debug.ReadBuildInfo()
	if hasInfo {
		p.Module = info.Main.Path
		p.ModuleVersion = info.Main.Version
	}

	if commit, dirty, ok := gitState(ctx, dir); ok {
		p.GitCommit, p.GitDirty = commit, dirty
	} else if hasInfo {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				p.GitCommit = s.Value
			case "vcs.modified":
				p.GitDirty = s.Value == "true"
			}
		}
	}
	return p
}

// AddModels records models the run called, keeping Models sorted and free of
// duplicates and empty names.
func (p *Provenance) AddModels(models ...string) {
	for _, m := range models {
		if m != "" && !slices.Contains(p.Models, m) {
			p.Models = append(p.Models, m)
		}
	}
	slices.Sort(p.Models)
}

// gitTimeout bounds each git call so a hung repository cannot stall a run.
const gitTimeout = 5 * time.Second

func gitState(ctx context.Context, dir string) (commit string, dirty bool, ok bool) {
	if _, err := exec.LookPath("git"); err != nil {
		return "", false, false
	}
	run := func(args ...string) (string, error) {
		cctx, cancel := context.WithTimeout(ctx, gitTimeout)
		defer cancel()
		cmd := exec.CommandContext(cctx, "git", args...) //nolint:gosec // fixed binary; the arguments are constants from this file
		if dir != "" {
			cmd.Dir = dir
		}
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Run(); err != nil {
			return "", err
		}
		return out.String(), nil
	}
	head, err := run("rev-parse", "HEAD")
	if err != nil {
		return "", false, false
	}
	status, err := run("status", "--porcelain")
	if err != nil {
		return "", false, false
	}
	return strings.TrimSpace(head), strings.TrimSpace(status) != "", true
}
