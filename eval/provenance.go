package eval

import (
	"bytes"
	"context"
	"fmt"
	"maps"
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
	// Tools maps each tool that reported a version, such as an agent.Func
	// schema hash or an agent.AIFunc version, to the versions that ran,
	// joined with ", " when several did.
	Tools map[string]string `json:"tools,omitempty"`
	// Dials records, per dial name, what each requested value compiled to
	// on the run's calls. See AddDial.
	Dials map[string][]DialOutcome `json:"dials,omitempty"`
	// Conversions records how the run's media reached its models: one
	// entry per part kind, executed action and converter (or offering)
	// that ran it, such as "image native" or "document extracted via
	// documents@1". See AddConversion.
	Conversions []ConversionOutcome `json:"conversions,omitempty"`
}

// ConversionOutcome is one way media of one kind reached a model in a run.
type ConversionOutcome struct {
	Kind   string `json:"kind"`
	Action string `json:"action"`
	// Via is the converter, as name@version, or the offering that did it;
	// empty for native media.
	Via string `json:"via,omitempty"`
}

// AddConversion records that a part of kind was handled by action through
// via on one call, such as AddConversion("audio", "transcribed",
// "transcribe@1"). Entries are deduplicated and kept sorted. Calls with an
// empty kind or action are ignored.
func (p *Provenance) AddConversion(kind, action, via string) {
	if kind == "" || action == "" {
		return
	}
	o := ConversionOutcome{Kind: kind, Action: action, Via: via}
	if slices.Contains(p.Conversions, o) {
		return
	}
	p.Conversions = append(p.Conversions, o)
	slices.SortFunc(p.Conversions, func(a, b ConversionOutcome) int {
		return strings.Compare(a.Kind+"\x00"+a.Action+"\x00"+a.Via, b.Kind+"\x00"+b.Action+"\x00"+b.Via)
	})
}

// AddTool records that a version of a tool ran. Calls with an empty name or
// version are ignored.
func (p *Provenance) AddTool(name, version string) {
	if name == "" || version == "" {
		return
	}
	if p.Tools == nil {
		p.Tools = map[string]string{}
	}
	have := p.Tools[name]
	if have == "" {
		p.Tools[name] = version
		return
	}
	if !slices.Contains(strings.Split(have, ", "), version) {
		p.Tools[name] = have + ", " + version
	}
}

// DialOutcome is what one requested dial value compiled to across a run.
type DialOutcome struct {
	Requested string `json:"requested"`
	// Sent lists the distinct raw parameters it was sent as, sorted, with
	// "dropped" or "rejected" when nothing was sent.
	Sent []string `json:"sent"`
}

// AddDial records that a dial requested as requested was sent as sent on
// one call. Empty names are ignored.
func (p *Provenance) AddDial(name, requested, sent string) {
	if name == "" {
		return
	}
	if p.Dials == nil {
		p.Dials = map[string][]DialOutcome{}
	}
	outcomes := p.Dials[name]
	i := slices.IndexFunc(outcomes, func(o DialOutcome) bool { return o.Requested == requested })
	if i < 0 {
		outcomes = append(outcomes, DialOutcome{Requested: requested})
		slices.SortFunc(outcomes, func(a, b DialOutcome) int { return strings.Compare(a.Requested, b.Requested) })
		i = slices.IndexFunc(outcomes, func(o DialOutcome) bool { return o.Requested == requested })
	}
	if !slices.Contains(outcomes[i].Sent, sent) {
		outcomes[i].Sent = append(outcomes[i].Sent, sent)
		slices.Sort(outcomes[i].Sent)
	}
	p.Dials[name] = outcomes
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

// ConfigDrift lists the differences between two runs' configurations that
// make their scores incomparable: the same profile ID served with a
// different configuration hash, a different catalog revision, a dial held at
// the same value in both runs that was sent as different raw parameters,
// the same tool run at a different version, or media of a kind reaching the
// models differently (native in one run, described in the other). The
// catalog checks need catalog provenance on both runs.
func ConfigDrift(base, exp Provenance) []string {
	out := append(dialDrift(base, exp), toolDrift(base, exp)...)
	out = append(out, conversionDrift(base, exp)...)
	if base.Catalog == nil || exp.Catalog == nil {
		return out
	}
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

// dialDrift lists the dials both runs requested with the same value but
// sent differently, such as creativity applied on one model and dropped on
// the other.
func dialDrift(base, exp Provenance) []string {
	var out []string
	for _, name := range slices.Sorted(maps.Keys(base.Dials)) {
		for _, b := range base.Dials[name] {
			i := slices.IndexFunc(exp.Dials[name], func(o DialOutcome) bool { return o.Requested == b.Requested })
			if i < 0 {
				continue
			}
			if e := exp.Dials[name][i]; !slices.Equal(b.Sent, e.Sent) {
				out = append(out, fmt.Sprintf("dial %s %s: sent as %v vs %v", name, b.Requested, b.Sent, e.Sent))
			}
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

// toolDrift lists tools both runs called at different versions.
func toolDrift(base, exp Provenance) []string {
	names := make([]string, 0, len(base.Tools))
	for name := range base.Tools {
		names = append(names, name)
	}
	slices.Sort(names)
	var out []string
	for _, name := range names {
		if v, ok := exp.Tools[name]; ok && v != base.Tools[name] {
			out = append(out, fmt.Sprintf("tool %s: version %s vs %s", name, base.Tools[name], v))
		}
	}
	return out
}

// conversionDrift lists the part kinds both runs converted whose handling
// differs between them.
func conversionDrift(base, exp Provenance) []string {
	if len(base.Conversions) == 0 || len(exp.Conversions) == 0 {
		return nil
	}
	byKind := func(p Provenance) map[string][]string {
		m := map[string][]string{}
		for _, o := range p.Conversions {
			h := o.Action
			if o.Via != "" {
				h += " via " + o.Via
			}
			m[o.Kind] = append(m[o.Kind], h)
		}
		return m
	}
	b, e := byKind(base), byKind(exp)
	var out []string
	for _, kind := range slices.Sorted(maps.Keys(b)) {
		if ev, ok := e[kind]; ok && !slices.Equal(b[kind], ev) {
			out = append(out, fmt.Sprintf("%s media: %s vs %s", kind, strings.Join(b[kind], ", "), strings.Join(ev, ", ")))
		}
	}
	return out
}
