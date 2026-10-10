package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/urmzd/saige/agent/provider"
	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/registry"
	"github.com/urmzd/saige/agent/types"
)

// Environment variables that configure catalog layers.
const (
	envCatalog      = "SAIGE_CATALOG"
	envTrustProject = "SAIGE_TRUST_PROJECT_CATALOG"
)

// Layer kinds, lowest precedence first.
const (
	layerEmbedded = "embedded"
	layerUser     = "user"
	layerProject  = "project"
	layerEnv      = "env"
	layerFlag     = "flag"
)

// layer is one catalog source the CLI merges.
type layer struct {
	Kind   string `json:"kind"`
	Ref    string `json:"ref"`
	Source catalog.Source
	// Trusted layers may set every field. An untrusted layer is limited to
	// the allowlist in untrustedFields.
	Trusted bool `json:"trusted"`
	// Revision is filled in after loading.
	Revision string `json:"revision,omitempty"`
}

// discoverLayers lists the catalog layers, lowest precedence first: the
// embedded default, the user file, the project file, $SAIGE_CATALOG, and each
// --catalog flag. Missing user and project files are skipped; a missing
// path named explicitly is an error when it is loaded.
//
// A project file can arrive with a cloned repository, so it is untrusted: it
// may set only the fields untrustedFields allows, none of which can send
// the user's key, prompt or tool traffic to another host. It is trusted when
// SAIGE_TRUST_PROJECT_CATALOG=1 or when the same file is named explicitly,
// in which case it is loaded once, as the explicit layer.
func discoverLayers(flags []string, getenv func(string) string) ([]layer, error) {
	layers := []layer{{Kind: layerEmbedded, Ref: "catalog/default.json", Source: catalog.EmbeddedSource(), Trusted: true}}
	var explicit []layer
	for _, ref := range filepath.SplitList(getenv(envCatalog)) {
		if ref == "" {
			continue
		}
		l, err := explicitLayer(layerEnv, ref)
		if err != nil {
			return nil, err
		}
		explicit = append(explicit, l)
	}
	for _, ref := range flags {
		l, err := explicitLayer(layerFlag, ref)
		if err != nil {
			return nil, err
		}
		explicit = append(explicit, l)
	}
	if path := userCatalogPath(getenv); path != "" && fileExists(path) {
		layers = append(layers, layer{Kind: layerUser, Ref: path, Source: catalog.FileSource(path), Trusted: true})
	}
	if path := projectCatalogPath(); path != "" && fileExists(path) {
		layers = append(layers, layer{Kind: layerProject, Ref: path, Source: catalog.FileSource(path), Trusted: getenv(envTrustProject) == "1"})
	}
	return dedupeLayers(append(layers, explicit...)), nil
}

// dedupeLayers keeps one layer per file, compared by cleaned absolute path:
// the occurrence with the highest precedence, trusted when any occurrence
// is. Naming the project file with --catalog therefore loads it once, at
// the flag's place and with the flag's trust.
func dedupeLayers(layers []layer) []layer {
	keys := make([]string, len(layers))
	trusted := map[string]bool{}
	last := map[string]int{}
	for i, l := range layers {
		keys[i] = layerFileKey(l)
		if keys[i] == "" {
			continue
		}
		trusted[keys[i]] = trusted[keys[i]] || l.Trusted
		last[keys[i]] = i
	}
	out := make([]layer, 0, len(layers))
	for i, l := range layers {
		if k := keys[i]; k != "" {
			if last[k] != i {
				continue
			}
			l.Trusted = trusted[k]
		}
		out = append(out, l)
	}
	return out
}

// layerFileKey is the cleaned absolute path of a file layer, with symbolic
// links resolved when the file exists, or "" for any other layer.
func layerFileKey(l layer) string {
	if l.Kind == layerEmbedded {
		return ""
	}
	fs, ok := l.Source.(interface{ Path() string })
	if !ok {
		return ""
	}
	abs, err := filepath.Abs(fs.Path())
	if err != nil {
		return ""
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return filepath.Clean(abs)
}

// explicitLayer builds a layer the user named, which is trusted. A URL's
// credentials (user information, query string) are redacted from Ref, which
// is listed and recorded as the source of installed rows.
func explicitLayer(kind, ref string) (layer, error) {
	src, err := catalog.ParseSource(ref, false)
	if err != nil {
		return layer{}, err
	}
	display := ref
	if strings.Contains(ref, "://") && !strings.HasPrefix(ref, "file://") {
		display = catalog.RedactURL(ref)
	}
	return layer{Kind: kind, Ref: display, Source: src, Trusted: true}, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// userCatalogPath is $XDG_CONFIG_HOME/saige/catalog.json, defaulting to
// ~/.config/saige/catalog.json.
func userCatalogPath(getenv func(string) string) string {
	dir := getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home := getenv("HOME")
		if home == "" {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "saige", "catalog.json")
}

// projectCatalogPath walks up from the working directory to the first
// directory holding .saige/catalog.json, stopping at a repository root (a
// directory with .git) or the filesystem root.
func projectCatalogPath() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		candidate := filepath.Join(dir, ".saige", "catalog.json")
		if fileExists(candidate) {
			return candidate
		}
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// loadLayers loads every layer, applying the trust rule.
func loadLayers(ctx context.Context, layers []layer) ([]*catalog.Catalog, error) {
	out := make([]*catalog.Catalog, 0, len(layers))
	for i := range layers {
		l := &layers[i]
		c, err := l.Source.Load(ctx)
		if err != nil {
			return nil, fmt.Errorf("catalog layer %s %s: %w", l.Kind, l.Ref, err)
		}
		if !l.Trusted {
			if issues := untrustedIssues(c); len(issues) > 0 {
				return nil, &catalog.ValidationError{Source: l.Ref, Issues: issues}
			}
		}
		l.Revision = c.Revision
		out = append(out, c)
	}
	return out, nil
}

// mergeLayers loads and merges the layers into one validated catalog.
func mergeLayers(ctx context.Context, layers []layer) (*catalog.Catalog, error) {
	cats, err := loadLayers(ctx, layers)
	if err != nil {
		return nil, err
	}
	return catalog.Merge(cats[0], cats[1:]...)
}

// layerSource records the layers on installed revisions.
func layerSource(layers []layer) registry.Option {
	refs := make([]string, len(layers))
	for i, l := range layers {
		refs[i] = l.Ref
	}
	return registry.WithSource(strings.Join(refs, " + "))
}

func newCatalogCmd(ctx context.Context) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "catalog",
		Short: "Inspect and validate the model catalog and presets",
		Long: "The catalog declares model capabilities and named presets: ordered provider\n" +
			"chains where every entry carries options resolved for its own model.\n" +
			"Layers merge in order: embedded default, ~/.config/saige/catalog.json,\n" +
			".saige/catalog.json in the project, $SAIGE_CATALOG, then each --catalog.",
	}
	cmd.AddCommand(newCatalogShowCmd(), newCatalogExplainCmd(), newCatalogValidateCmd(ctx), newCatalogLayersCmd(),
		newCatalogSchemaCmd(), newCatalogExportCmd(), newCatalogReconcileCmd(), newCatalogMigrateCmd())
	return cmd
}

// shownEntry is the JSON shape of one resolved chain entry.
type shownEntry struct {
	Profile    types.ProfileID          `json:"profile"`
	Provider   types.ProviderName       `json:"provider"`
	Model      types.ModelID            `json:"model"`
	Endpoint   string                   `json:"endpoint,omitempty"`
	Offering   string                   `json:"offering,omitempty"`
	Modalities []string                 `json:"modalities,omitempty"`
	Tiers      []types.ServiceTier      `json:"service_tiers,omitempty"`
	Known      bool                     `json:"known"`
	ConfigHash string                   `json:"config_hash"`
	Options    map[string]shownOption   `json:"options,omitempty"`
	Retry      *catalog.RetrySpec       `json:"retry,omitempty"`
	Cache      *catalog.PromptCacheSpec `json:"prompt_cache,omitempty"`
	Optional   bool                     `json:"optional,omitempty"`
}

type shownOption struct {
	Value  any           `json:"value"`
	Origin catalog.Layer `json:"origin"`
}

type shownPreset struct {
	Name     types.PresetName `json:"name"`
	Revision string           `json:"catalog_revision,omitempty"`
	Chain    []shownEntry     `json:"chain"`
	Warnings []catalog.Issue  `json:"warnings,omitempty"`
}

func newCatalogShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show [preset|provider/model]",
		Short: "Show the effective options of every chain entry, with where each came from",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cf := persistentFlagVars
			cat, err := cf.catalog()
			if err != nil {
				return err
			}
			var name types.PresetName
			if len(args) == 1 {
				name = types.PresetName(args[0])
			} else if name, cat, _, err = cf.selectPreset(cat); err != nil {
				return err
			}
			rp, err := resolveNamed(cat, name)
			if err != nil {
				return err
			}
			return writeShown(cmd.OutOrStdout(), showPreset(rp), cf.isJSON())
		},
	}
}

func showPreset(rp catalog.ResolvedPreset) shownPreset {
	out := shownPreset{Name: rp.Name, Revision: rp.CatalogRevision, Warnings: rp.Warnings}
	for _, e := range rp.Chain {
		se := shownEntry{Profile: e.ProfileID, Provider: e.Provider, Model: e.Model, Endpoint: e.Endpoint, Offering: e.Offering,
			Known: e.Caps.Known, ConfigHash: e.ConfigHash, Options: map[string]shownOption{}, Retry: e.Retry, Cache: e.PromptCache, Optional: e.Optional}
		if o := e.Caps.Offering; o != nil {
			se.Modalities = modalitySummary(*o)
			se.Tiers = slices.Sorted(maps.Keys(o.Tiers))
		}
		values := optionValues(e)
		for _, n := range e.OptionNames() {
			se.Options[n] = shownOption{Value: values[n], Origin: e.Origin[n]}
		}
		out.Chain = append(out.Chain, se)
	}
	return out
}

// optionValues renders each set option's value.
func optionValues(e catalog.ResolvedEntry) map[string]any {
	o := e.Options
	out := map[string]any{}
	put := func(name string, v any, set bool) {
		if set {
			out[name] = v
		}
	}
	deref := func(p *float64) any {
		if p == nil {
			return nil
		}
		return *p
	}
	put(types.OptionTemperature, deref(o.Temperature), o.Temperature != nil)
	put(types.OptionTopP, deref(o.TopP), o.TopP != nil)
	put(types.OptionTopK, deref(o.TopK), o.TopK != nil)
	put(types.OptionFrequencyPenalty, deref(o.FrequencyPenalty), o.FrequencyPenalty != nil)
	put(types.OptionPresencePenalty, deref(o.PresencePenalty), o.PresencePenalty != nil)
	if o.Seed != nil {
		out[types.OptionSeed] = *o.Seed
	}
	if o.MaxOutputTokens != nil {
		out[types.OptionMaxOutputTokens] = *o.MaxOutputTokens
	} else if e.Origin[types.OptionMaxOutputTokens] == catalog.LayerProvider {
		out[types.OptionMaxOutputTokens] = e.Caps.DefaultMaxOutputTokens
	}
	put(types.OptionStop, o.StopSequences, len(o.StopSequences) > 0)
	if o.ParallelTools != nil {
		out[types.OptionParallelTools] = *o.ParallelTools
	}
	switch {
	case o.ReasoningEffort != nil:
		out[types.OptionReasoning] = "effort " + *o.ReasoningEffort
	case o.ReasoningBudget != nil:
		out[types.OptionReasoning] = fmt.Sprintf("budget %d", *o.ReasoningBudget)
	case o.ReasoningEnabled != nil:
		out[types.OptionReasoning] = fmt.Sprintf("enabled %v", *o.ReasoningEnabled)
	}
	if o.ToolChoice != nil {
		out[types.OptionToolChoice] = string(o.ToolChoice.Mode)
	}
	if e.PromptCache != nil {
		out["prompt_cache"] = e.PromptCache.Mode
	}
	if len(e.ServerTools) > 0 {
		var kinds []string
		for _, st := range e.ServerTools {
			kinds = append(kinds, string(st.Kind))
		}
		out["server_tools"] = strings.Join(kinds, ",")
	}
	return out
}

func writeShown(w io.Writer, sp shownPreset, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(sp)
	}
	fmt.Fprintf(w, "preset %s (catalog %s)\n", sp.Name, dash(sp.Revision))
	for i, e := range sp.Chain {
		fmt.Fprintf(w, "\n%d. %s  %s/%s  hash %s", i+1, e.Profile, e.Provider, e.Model, e.ConfigHash)
		if e.Optional {
			fmt.Fprint(w, "  (optional)")
		}
		fmt.Fprintln(w)
		fmt.Fprintf(w, "   endpoint %s  offering %s\n", dash(e.Endpoint), dash(e.Offering))
		if len(e.Modalities) > 0 || len(e.Tiers) > 0 {
			fmt.Fprintf(w, "   in %s  tiers %s\n", dash(strings.Join(e.Modalities, ", ")), dash(joinNames(e.Tiers)))
		}
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "   OPTION\tVALUE\tORIGIN")
		for _, n := range sortedOptionNames(e.Options) {
			o := e.Options[n]
			fmt.Fprintf(tw, "   %s\t%v\t%s\n", n, o.Value, o.Origin)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	for _, is := range sp.Warnings {
		fmt.Fprintf(w, "\nwarning: %s\n", is)
	}
	return nil
}

func sortedOptionNames(m map[string]shownOption) []string {
	var out []string
	for _, n := range append(types.AllOptionNames(), "prompt_cache", "server_tools") {
		if _, ok := m[n]; ok {
			out = append(out, n)
		}
	}
	return out
}

// exitError carries an exit code for run.
type exitError struct {
	code int
	err  error
}

func (e exitError) Error() string { return e.err.Error() }
func (e exitError) ExitCode() int { return e.code }
func (e exitError) Unwrap() error { return e.err }

func newCatalogValidateCmd(ctx context.Context) *cobra.Command {
	var strict, dryBuild bool
	cmd := &cobra.Command{
		Use:   "validate [files...]",
		Short: "Validate catalog layers and every preset they declare",
		Long: "With no arguments, validate the merged layers. With files, validate the\n" +
			"embedded default with those files merged on top. Exits 1 on any error.",
		RunE: func(cmd *cobra.Command, args []string) error {
			var cat *catalog.Catalog
			var err error
			if len(args) == 0 {
				var layers []layer
				if layers, err = discoverLayers(*persistentFlagVars.catalogs, os.Getenv); err == nil {
					cat, err = mergeLayers(ctx, layers)
				}
			} else {
				layers := []layer{{Kind: layerEmbedded, Ref: "catalog/default.json", Source: catalog.EmbeddedSource(), Trusted: true}}
				for _, f := range args {
					l, lerr := explicitLayer(layerFlag, f)
					if lerr != nil {
						return lerr
					}
					layers = append(layers, l)
				}
				cat, err = mergeLayers(ctx, layers)
			}
			w := cmd.OutOrStdout()
			var issues []catalog.Issue
			var ve *catalog.ValidationError
			switch {
			case errors.As(err, &ve):
				issues = ve.Issues
			case err != nil:
				return err
			default:
				issues = cat.Issues()
			}
			if err == nil && dryBuild {
				issues = append(issues, dryBuildIssues(ctx, cat)...)
			}
			failed := false
			for _, is := range issues {
				fmt.Fprintf(w, "%s: %s [%s]\n", is.Severity, is, is.Code)
				failed = failed || is.Severity == catalog.SeverityError || (strict && is.Severity == catalog.SeverityWarning)
			}
			if failed {
				return reportedError{exitError{code: 1, err: errors.New("catalog validation failed")}}
			}
			fmt.Fprintf(w, "ok: %d presets, revision %s\n", len(cat.Presets), dash(cat.Revision))
			return nil
		},
	}
	cmd.Flags().BoolVar(&strict, "strict", false, "Treat warnings as errors")
	cmd.Flags().BoolVar(&dryBuild, "dry-build", false, "Build every entry's adapter with a placeholder key, without network, to run adapter checks")
	return cmd
}

// dryBuildIssues runs provider.Build on every preset entry with a
// placeholder key. Adapter construction makes no request, so this checks
// adapter-specific rules (Anthropic's budget below max_tokens, its
// temperature range, thinking and sampling conflicts) without credentials.
func dryBuildIssues(ctx context.Context, cat *catalog.Catalog) []catalog.Issue {
	var out []catalog.Issue
	for _, name := range cat.PresetNames() {
		rp, err := cat.Resolve(name)
		if err != nil {
			continue // reported by validation
		}
		for i, e := range rp.Chain {
			cfg := provider.Config{Provider: e.Provider, Model: e.Model, BaseURL: e.BaseURL, Options: e.Options,
				ServerTools: e.ServerTools, Getenv: func(string) string { return "" }}
			if e.Provider != provider.Ollama {
				cfg.APIKey = "dry-build"
			}
			if pc := e.PromptCache; pc != nil && pc.Mode != catalog.PromptCacheOff {
				cfg.PromptCache = &provider.PromptCache{Mode: pc.Mode, TTL: pc.TTL, Tools: pc.Tools, System: pc.System,
					Conversation: pc.Conversation, Retention: pc.Retention, Key: pc.Key}
			}
			p, err := provider.Build(ctx, cfg)
			if err != nil {
				out = append(out, catalog.Issue{Path: fmt.Sprintf("presets.%s.chain[%d]", name, i), Code: "build_failed",
					Message: err.Error(), Severity: catalog.SeverityError})
				continue
			}
			_ = types.CloseProvider(ctx, p)
		}
	}
	return out
}

func newCatalogLayersCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "layers",
		Short: "List the catalog layers in merge order, with trust state and revisions",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cf := persistentFlagVars
			layers, err := discoverLayers(*cf.catalogs, os.Getenv)
			if err != nil {
				return err
			}
			_, loadErr := loadLayers(cmd.Context(), layers)
			w := cmd.OutOrStdout()
			if cf.isJSON() {
				if err := json.NewEncoder(w).Encode(layers); err != nil {
					return err
				}
			} else {
				tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "KIND\tREF\tTRUSTED\tREVISION")
				for _, l := range layers {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", l.Kind, l.Ref, yesNo(l.Trusted), dash(l.Revision))
				}
				if err := tw.Flush(); err != nil {
					return err
				}
			}
			return loadErr
		},
	}
}

func newCatalogSchemaCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "schema",
		Short: "Print the JSON Schema of the catalog file format",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := cmd.OutOrStdout().Write(catalog.Schema())
			return err
		},
	}
}

func newCatalogExportCmd() *cobra.Command {
	var flat bool
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Print the merged catalog as canonical JSON",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cat, err := persistentFlagVars.catalog()
			if err != nil {
				return err
			}
			if flat {
				// Every row written out in full, as Lookup resolves it now.
				cat = catalog.Export()
			}
			out, err := cat.Canonical()
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(out)
			return err
		},
	}
	cmd.Flags().BoolVar(&flat, "flat", false, "Write every row in full, without templates")
	return cmd
}

func newCatalogMigrateCmd() *cobra.Command {
	var write bool
	cmd := &cobra.Command{
		Use:   "migrate FILE",
		Short: "Convert a version 1 catalog file to version 2",
		Long: "Read a catalog layer, convert it to version 2 if it is version 1, and print it\n" +
			"as canonical JSON. Templates split into model and offering templates, rows\n" +
			"into models and offerings on each vendor's primary endpoint, baselines into\n" +
			"endpoint default offering templates, and batch pricing into the batch tier.\n" +
			"With --write, the file is rewritten in place.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := catalog.LoadFile(args[0])
			if err != nil {
				return err
			}
			out, err := c.Canonical()
			if err != nil {
				return err
			}
			if !write {
				_, err = cmd.OutOrStdout().Write(out)
				return err
			}
			info, err := os.Stat(args[0])
			if err != nil {
				return err
			}
			if err := os.WriteFile(args[0], out, info.Mode().Perm()); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "wrote %s (catalog version %d)\n", args[0], catalog.SchemaVersion)
			return nil
		},
	}
	cmd.Flags().BoolVar(&write, "write", false, "Rewrite the file in place")
	return cmd
}

// resolveNamed resolves a preset, or a "provider/model" reference to a
// one-entry preset.
func resolveNamed(cat *catalog.Catalog, name types.PresetName) (catalog.ResolvedPreset, error) {
	if _, ok := cat.Presets[name]; ok || !strings.Contains(string(name), "/") {
		return cat.Resolve(name)
	}
	p, m, _ := strings.Cut(string(name), "/")
	return cat.ResolveModel(types.ProviderName(p), types.ModelID(m))
}

// modalitySummary lists an offering's input modalities with their media
// types, as "image(jpeg,png)".
func modalitySummary(o types.Offering) []string {
	var out []string
	for _, m := range types.KnownModalities() {
		l, ok := o.Modalities.In[m]
		if !ok {
			continue
		}
		var media []string
		for _, mt := range l.Media {
			_, sub, _ := strings.Cut(string(mt), "/")
			media = append(media, sub)
		}
		s := string(m)
		if len(media) > 0 {
			s += "(" + strings.Join(media, ",") + ")"
		}
		out = append(out, s)
	}
	return out
}
