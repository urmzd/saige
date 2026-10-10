package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/urmzd/saige/agent/provider"
	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/provider/wrapper"
	"github.com/urmzd/saige/agent/types"
)

// Exit codes of saige catalog reconcile.
const (
	reconcileExitDrift = 1
	reconcileExitError = 2
)

// stubNote marks a row written by reconcile that a human has not filled in.
const stubNote = "needs review"

// defaultStaleAfter is how old a price's as_of date may be before the report
// lists it. List endpoints do not report prices, so age is the only signal.
const defaultStaleAfter = 90 * 24 * time.Hour

// reconcileDefaultProviders are checked when --providers is not given. Ollama
// is checked only when named, since it lists the weights pulled on this
// machine rather than what a vendor serves.
var reconcileDefaultProviders = []string{providerAnthropic, providerOpenAI, providerGoogle}

// newReconcileLister builds the lister for one provider. Tests replace it.
var newReconcileLister = buildReconcileLister

// reconcileOptions configures one reconcile run.
type reconcileOptions struct {
	providers  []string
	getenv     func(string) string
	newLister  func(ctx context.Context, name string) (catalog.ModelLister, error)
	ignore     []string
	staleAfter time.Duration
	now        time.Time
}

// reconcileReport is what reconcile found. Drift is true when the catalog
// needs a change: a new model, a limit that disagrees, a row no listed model
// matches, or a stub row still waiting for review. Stale prices are a
// reminder and do not count as drift.
type reconcileReport struct {
	CheckedAt     string               `json:"checked_at"`
	Revision      string               `json:"catalog_revision,omitempty"`
	Drift         bool                 `json:"drift"`
	Providers     []providerReconcile  `json:"providers"`
	PendingReview []string             `json:"pending_review,omitempty"`
	StalePricing  []stalePrice         `json:"stale_pricing,omitempty"`
	StaleAfter    int                  `json:"stale_after_days"`
	Stubs         []stubRow            `json:"stubs,omitempty"`
	Written       *reconcileWriteState `json:"written,omitempty"`
}

// reconcileWriteState records what --write did.
type reconcileWriteState struct {
	Path  string   `json:"path"`
	Added []string `json:"added"`
}

// providerReconcile is one provider's part of the report.
type providerReconcile struct {
	Provider string `json:"provider"`
	// Status is "checked", "skipped" (no credentials) or "error".
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
	Listed   int    `json:"listed"`
	Declared int    `json:"declared"`
	Inferred int    `json:"inferred"`
	Ignored  int    `json:"ignored"`
	// New lists models only the provider baseline covers.
	New []newModel `json:"new,omitempty"`
	// LimitDrift lists models whose endpoint limits disagree with the row.
	LimitDrift []limitDrift `json:"limit_drift,omitempty"`
	// Disappeared lists row prefixes no listed model matches and that name
	// no successor yet: candidates for superseded_by or removal.
	Disappeared []string `json:"disappeared,omitempty"`
}

type newModel struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name,omitempty"`
	Created     string `json:"created,omitempty"`
}

type limitDrift struct {
	ID     string   `json:"id"`
	Family string   `json:"family"`
	Drift  []string `json:"drift"`
}

type stalePrice struct {
	Row     string `json:"row"`
	Field   string `json:"field"`
	AsOf    string `json:"as_of"`
	AgeDays int    `json:"age_days,omitempty"`
}

const (
	reconcileChecked = "checked"
	reconcileSkipped = "skipped"
	reconcileFailed  = "error"
)

func newCatalogReconcileCmd() *cobra.Command {
	var providers, ignore []string
	var ignoreFile, write string
	var staleDays int
	cmd := &cobra.Command{
		Use:   "reconcile",
		Short: "Compare what each provider lists with the active catalog",
		Long: "List the models each configured provider serves and reconcile them against\n" +
			"the active catalog. The report names new models, limits that disagree with\n" +
			"their row, rows no listed model matches, stub rows still waiting for review,\n" +
			"and prices whose as_of date is older than --stale-days.\n\n" +
			"With --write, add a stub for each new model to a version 2 catalog file: the\n" +
			"model and an offering extending the endpoint's baseline template, no price,\n" +
			"and the note \"needs review\".\n" +
			"Existing rows are never changed.\n\n" +
			"Exit status: 0 no drift, 1 drift, 2 error.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cf := persistentFlagVars
			cat, err := cf.catalog()
			if err != nil {
				return exitError{code: reconcileExitError, err: err}
			}
			patterns := slices.Clone(ignore)
			if ignoreFile != "" {
				more, err := readIgnoreFile(ignoreFile)
				if err != nil {
					return exitError{code: reconcileExitError, err: err}
				}
				patterns = append(patterns, more...)
			}
			if len(providers) == 0 {
				providers = reconcileDefaultProviders
			}
			opts := reconcileOptions{providers: providers, getenv: os.Getenv, newLister: newReconcileLister,
				ignore: patterns, staleAfter: time.Duration(staleDays) * 24 * time.Hour, now: time.Now().UTC()}
			rep, runErr := reconcileCatalog(cmd.Context(), cat, opts)
			if runErr == nil && write != "" && len(rep.Stubs) > 0 {
				added, err := writeStubRows(write, rep.Stubs)
				if err != nil {
					runErr = err
				} else {
					rep.Written = &reconcileWriteState{Path: write, Added: added}
				}
			}
			w := cmd.OutOrStdout()
			if cf.isJSON() {
				enc := json.NewEncoder(w)
				enc.SetIndent("", "  ")
				err = enc.Encode(rep)
			} else {
				err = writeReconcileReport(w, rep)
			}
			switch {
			case runErr != nil:
				return exitError{code: reconcileExitError, err: runErr}
			case err != nil:
				return exitError{code: reconcileExitError, err: err}
			case rep.Drift:
				return reportedError{exitError{code: reconcileExitDrift, err: errors.New("catalog drift found")}}
			}
			return nil
		},
	}
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return exitError{code: reconcileExitError, err: err}
	})
	f := cmd.Flags()
	f.StringSliceVar(&providers, "providers", nil, "Providers to check (anthropic,openai,google,ollama); default anthropic,openai,google")
	f.StringArrayVar(&ignore, "ignore", nil, "Glob over model IDs and row prefixes to leave out, bare or as provider/glob (repeatable)")
	f.StringVar(&ignoreFile, "ignore-file", "", "File of --ignore globs, one per line; # starts a comment")
	f.StringVar(&write, "write", "", "Append stub rows for new models to this catalog file (created when missing)")
	f.IntVar(&staleDays, "stale-days", int(defaultStaleAfter/(24*time.Hour)), "List prices whose as_of date is older than this many days")
	return cmd
}

// reconcileCatalog lists each provider's models and reconciles them against
// the installed catalog, which cat is. A provider without credentials is
// skipped; a provider whose listing fails is reported and makes the error
// non-nil, since a partial check cannot say there is no drift.
func reconcileCatalog(ctx context.Context, cat *catalog.Catalog, opts reconcileOptions) (reconcileReport, error) {
	rep := reconcileReport{CheckedAt: opts.now.Format(time.DateOnly), Revision: cat.Revision,
		StaleAfter: int(opts.staleAfter / (24 * time.Hour))}
	var errs []error
	checked := 0
	for _, name := range opts.providers {
		name = strings.ToLower(strings.TrimSpace(name))
		pr := providerReconcile{Provider: name}
		if reason, ok := reconcileCredentials(name, opts.getenv); !ok {
			pr.Status, pr.Reason = reconcileSkipped, reason
			rep.Providers = append(rep.Providers, pr)
			continue
		}
		remote, err := listRemote(ctx, opts, name)
		if err != nil {
			pr.Status, pr.Reason = reconcileFailed, err.Error()
			rep.Providers = append(rep.Providers, pr)
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		checked++
		pr.Status = reconcileChecked
		stubs := classify(&pr, catalogProvider(name), remote, opts.ignore)
		for _, id := range stubs {
			rep.Stubs = append(rep.Stubs, newStubRow(cat, catalogProvider(name), id))
		}
		rep.Providers = append(rep.Providers, pr)
	}
	if checked == 0 && len(errs) == 0 {
		errs = append(errs, errors.New("no provider is configured: set ANTHROPIC_API_KEY, OPENAI_API_KEY, GOOGLE_API_KEY or GOOGLE_GENAI_USE_VERTEXAI, or name ollama with --providers"))
	}
	rep.PendingReview = pendingReview(cat)
	rep.StalePricing = stalePricing(cat, opts.now, opts.staleAfter)
	for _, pr := range rep.Providers {
		rep.Drift = rep.Drift || len(pr.New) > 0 || len(pr.LimitDrift) > 0 || len(pr.Disappeared) > 0
	}
	rep.Drift = rep.Drift || len(rep.PendingReview) > 0
	return rep, errors.Join(errs...)
}

func listRemote(ctx context.Context, opts reconcileOptions, name string) ([]catalog.RemoteModel, error) {
	l, err := opts.newLister(ctx, name)
	if err != nil {
		return nil, err
	}
	if c, ok := l.(types.Provider); ok {
		defer func() { _ = types.CloseProvider(ctx, c) }()
	}
	return l.ListModels(ctx)
}

// classify fills the provider report from one listing and returns the IDs
// that need a stub row.
func classify(pr *providerReconcile, name types.ProviderName, remote []catalog.RemoteModel, ignore []string) []types.ModelID {
	var kept []catalog.RemoteModel
	for _, rm := range remote {
		if ignored(ignore, string(name), rm.ID) {
			pr.Ignored++
			continue
		}
		kept = append(kept, rm)
	}
	pr.Listed = len(remote)
	rec := catalog.Reconcile(name, kept)
	var newIDs []types.ModelID
	for _, m := range rec.Models {
		switch m.Status {
		case catalog.StatusDeclared:
			pr.Declared++
		case catalog.StatusInferred:
			pr.Inferred++
		case catalog.StatusUndeclared:
			nm := newModel{ID: m.Remote.ID, DisplayName: m.Remote.DisplayName}
			if !m.Remote.Created.IsZero() {
				nm.Created = m.Remote.Created.UTC().Format(time.DateOnly)
			}
			pr.New = append(pr.New, nm)
			newIDs = append(newIDs, stubPrefix(name, m.Remote.ID))
		}
		if len(m.Drift) > 0 {
			pr.LimitDrift = append(pr.LimitDrift, limitDrift{ID: m.Remote.ID, Family: string(m.Family), Drift: m.Drift})
		}
	}
	// A local runtime lists what is pulled, so an unmatched row there is not
	// a retirement.
	if name != providerOllama {
		for _, prefix := range rec.Unserved {
			if ignored(ignore, string(name), string(prefix)) {
				continue
			}
			if e, ok := catalog.Describe(name, prefix); ok && e.SupersededBy != "" {
				continue
			}
			pr.Disappeared = append(pr.Disappeared, string(prefix))
		}
	}
	return collapsePrefixes(newIDs)
}

// stubPrefix is the row prefix for a new model. An Ollama tag names one
// build of the weights, so the row is for the model without it.
func stubPrefix(provider types.ProviderName, id string) types.ModelID {
	if provider == providerOllama {
		if i := strings.LastIndex(id, ":"); i > 0 {
			return types.ModelID(id[:i])
		}
	}
	return types.ModelID(id)
}

// collapsePrefixes sorts and deduplicates IDs and drops each ID another ID
// is a prefix of, since that shorter row already serves it by family match:
// "gpt-x" covers "gpt-x-2026-01-01".
func collapsePrefixes(ids []types.ModelID) []types.ModelID {
	slices.Sort(ids)
	ids = slices.Compact(ids)
	var out []types.ModelID
	for _, id := range ids {
		if len(out) > 0 && strings.HasPrefix(string(id), string(out[len(out)-1])) {
			continue
		}
		out = append(out, id)
	}
	return out
}

// stubRow is what reconcile proposes for a new model: the model, with no
// facts, and an offering on the vendor's primary endpoint that extends the
// endpoint's baseline template. Tier, limits, successor, defaults, fees and
// prices are left for the reviewer, so nothing is asserted that the
// listing did not report.
type stubRow struct {
	Key      string               `json:"model"`
	Model    catalog.ModelSpec    `json:"model_spec"`
	Offering catalog.OfferingSpec `json:"offering"`
}

func newStubRow(cat *catalog.Catalog, vendor types.ProviderName, prefix types.ModelID) stubRow {
	key := string(vendor) + "/" + string(prefix)
	endpoint, _ := cat.PrimaryEndpoint(vendor)
	tmpl := ""
	if ep, ok := cat.Endpoints[endpoint]; ok {
		tmpl = ep.DefaultOfferingTemplate
	}
	return stubRow{Key: key, Model: catalog.ModelSpec{Notes: []string{stubNote}},
		Offering: catalog.OfferingSpec{Model: key, Endpoint: endpoint, Extends: tmpl, Notes: []string{stubNote}}}
}

// pendingReview lists models that still carry the stub note.
func pendingReview(cat *catalog.Catalog) []string {
	var out []string
	for k, m := range cat.Models {
		if slices.Contains(m.Notes, stubNote) {
			out = append(out, k)
		}
	}
	for _, o := range cat.Offerings {
		if slices.Contains(o.Notes, stubNote) && !slices.Contains(out, o.Model) {
			out = append(out, o.Model)
		}
	}
	sort.Strings(out)
	return out
}

// stalePricing lists every priced rate, token prices and server tool fees,
// whose as_of date is older than maxAge, or is not a date at all.
func stalePricing(cat *catalog.Catalog, now time.Time, maxAge time.Duration) []stalePrice {
	var out []stalePrice
	check := func(row, field, asOf string) {
		if asOf == "" {
			return
		}
		d, err := time.Parse(time.DateOnly, asOf)
		if err != nil {
			out = append(out, stalePrice{Row: row, Field: field, AsOf: asOf})
			return
		}
		if age := now.Sub(d); age > maxAge {
			out = append(out, stalePrice{Row: row, Field: field, AsOf: asOf, AgeDays: int(age / (24 * time.Hour))})
		}
	}
	for _, row := range slices.Sorted(maps.Keys(cat.Models)) {
		vendor, prefix, _ := strings.Cut(row, "/")
		e, ok := cat.Describe(types.ProviderName(vendor), types.ModelID(prefix))
		if !ok || string(e.Prefix) != prefix {
			continue
		}
		if !e.Caps.Pricing.Free {
			check(row, "pricing", e.Caps.Pricing.AsOf)
		}
		kinds := make([]string, 0, len(e.ServerToolFees))
		for k := range e.ServerToolFees {
			kinds = append(kinds, string(k))
		}
		sort.Strings(kinds)
		for _, k := range kinds {
			check(row, "server_tool_fees."+k, e.ServerToolFees[types.ServerToolKind(k)].AsOf)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Row < out[j].Row })
	return out
}

// ignored reports whether a pattern matches id or provider/id.
func ignored(patterns []string, provider, id string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, id); ok {
			return true
		}
		if ok, _ := path.Match(p, provider+"/"+id); ok {
			return true
		}
	}
	return false
}

// readIgnoreFile reads one glob per line. Blank lines and text after # are
// skipped. A malformed glob is an error rather than a pattern that never
// matches.
func readIgnoreFile(name string) ([]string, error) {
	f, err := os.Open(name) //nolint:gosec // path is from a CLI flag, not untrusted input
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []string
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line, _, _ := strings.Cut(sc.Text(), "#")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if _, err := path.Match(line, ""); err != nil {
			return nil, fmt.Errorf("%s:%d: %q: %w", name, n, line, err)
		}
		out = append(out, line)
	}
	return out, sc.Err()
}

// catalogProvider maps a CLI provider name to the catalog's: Vertex serves
// Google rows.
func catalogProvider(name string) types.ProviderName {
	if name == providerVertex {
		return providerGoogle
	}
	return types.ProviderName(name)
}

// reconcileCredentials reports whether a provider can be listed, or why not.
func reconcileCredentials(name string, getenv func(string) string) (string, bool) {
	switch name {
	case providerAnthropic, providerOpenAI:
		if env := provider.APIKeyEnv[types.ProviderName(name)][0]; getenv(env) == "" {
			return env + " is not set", false
		}
	case providerGoogle:
		if provider.VertexEnabled(getenv) {
			if getenv(provider.EnvCloudProject) == "" {
				return provider.EnvUseVertex + " is set but " + provider.EnvCloudProject + " is not", false
			}
			return "", true
		}
		if getenv("GOOGLE_API_KEY") == "" && getenv("GEMINI_API_KEY") == "" {
			return "GOOGLE_API_KEY is not set", false
		}
	case providerVertex:
		if getenv(provider.EnvCloudProject) == "" {
			return provider.EnvCloudProject + " is not set", false
		}
	case providerOllama:
	default:
		return "unknown provider", false
	}
	return "", true
}

// buildReconcileLister builds the provider's adapter with the model of its
// catalog preset, which only satisfies the constructor: listing does not
// depend on the model.
func buildReconcileLister(ctx context.Context, name string) (catalog.ModelLister, error) {
	cf := persistentFlagVars
	cfg := provider.Config{Provider: catalogProvider(name), Model: types.ModelID(name)}
	if cat, err := cf.catalog(); err == nil {
		if p, ok := cat.Presets[types.PresetName(name)]; ok && len(p.Chain) > 0 {
			cfg.Model = p.Chain[0].Model
		}
	}
	switch name {
	case providerVertex:
		cfg.Vertex = &provider.Vertex{}
	case providerOllama:
		cfg.BaseURL = *cf.ollamaHost
	}
	p, err := provider.Build(ctx, cfg)
	if err != nil {
		return nil, err
	}
	l, ok := wrapper.As[catalog.ModelLister](p)
	if !ok {
		_ = types.CloseProvider(ctx, p)
		return nil, fmt.Errorf("the %s adapter cannot list models", name)
	}
	return l, nil
}

// writeStubRows adds the stub models and offerings the file does not
// already declare, and returns the key of each model added. The file's
// existing bytes are kept as written: the stubs are inserted before the
// closing brace of its models object and the closing bracket of its
// offerings array, so nothing existing is reformatted or changed. A
// missing file is created as an overlay layer holding only the stubs. A
// version 1 file is refused: migrate it first.
func writeStubRows(name string, stubs []stubRow) ([]string, error) {
	raw, err := os.ReadFile(name) //nolint:gosec // path is from a CLI flag, not untrusted input
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	out, added, err := appendStubRows(raw, stubs)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if len(added) == 0 {
		return nil, nil
	}
	// The result must still load, or the file is left alone.
	if _, err := catalog.Load(bytes.NewReader(out)); err != nil {
		return nil, fmt.Errorf("%s: stub rows would not load: %w", name, err)
	}
	// A catalog is checked into a repository and read by others: keep an
	// existing file's mode, and create a new one world-readable.
	mode := os.FileMode(0o644)
	if info, err := os.Stat(name); err == nil {
		mode = info.Mode().Perm()
	}
	return added, os.WriteFile(name, out, mode) //nolint:gosec // a catalog holds no secrets; see above
}

// appendStubRows inserts stubs into a catalog file. See writeStubRows.
func appendStubRows(raw []byte, stubs []stubRow) ([]byte, []string, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		models := map[string]catalog.ModelSpec{}
		var offerings []catalog.OfferingSpec
		var added []string
		for _, s := range stubs {
			models[s.Key], offerings = s.Model, append(offerings, s.Offering)
			added = append(added, s.Key)
		}
		out, err := json.MarshalIndent(map[string]any{"version": catalog.SchemaVersion, "models": models, "offerings": offerings}, "", "  ")
		return append(out, '\n'), added, err
	}
	var version struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(raw, &version); err != nil {
		return nil, nil, errors.New("a catalog file must be a JSON object")
	}
	if version.Version == catalog.SchemaVersionV1 {
		return nil, nil, errors.New("the file is a version 1 catalog: run saige catalog migrate --write on it first")
	}
	var head struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, nil, fmt.Errorf("models must be an object keyed by vendor/prefix: %w", err)
	}
	have := map[string]bool{}
	for k := range head.Models {
		have[strings.ToLower(k)] = true
	}
	var todo []stubRow
	for _, s := range stubs {
		if !have[strings.ToLower(s.Key)] {
			todo = append(todo, s)
		}
	}
	if len(todo) == 0 {
		return raw, nil, nil
	}
	modelsEnd, modelCount, err := closingOffset(raw, "models", '{')
	if err != nil {
		return nil, nil, err
	}
	offEnd, offCount, err := closingOffset(raw, "offerings", '[')
	if err != nil {
		return nil, nil, err
	}
	var mins, oins bytes.Buffer
	var added []string
	for i, s := range todo {
		m, err := json.MarshalIndent(s.Model, "    ", "  ")
		if err != nil {
			return nil, nil, err
		}
		o, err := json.MarshalIndent(s.Offering, "    ", "  ")
		if err != nil {
			return nil, nil, err
		}
		if modelCount > 0 || i > 0 {
			mins.WriteByte(',')
		}
		key, _ := json.Marshal(s.Key)
		fmt.Fprintf(&mins, "\n    %s: %s", key, m)
		if offCount > 0 || i > 0 {
			oins.WriteByte(',')
		}
		oins.WriteString("\n    ")
		oins.Write(o)
		added = append(added, s.Key)
	}
	if modelCount == 0 {
		mins.WriteString("\n  ")
	}
	if offCount == 0 {
		oins.WriteString("\n  ")
	}
	// Insert the later position first so the earlier offset stays valid.
	first, second := insertion{modelsEnd, mins.Bytes()}, insertion{offEnd, oins.Bytes()}
	if first.at > second.at {
		first, second = second, first
	}
	out := slices.Concat(raw[:first.at], first.data, raw[first.at:second.at], second.data, raw[second.at:])
	return out, added, nil
}

type insertion struct {
	at   int
	data []byte
}

// closingOffset finds a top-level key's container and returns the offset
// just after its last member, and the member count.
func closingOffset(raw []byte, key string, open json.Delim) (int, int, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return 0, 0, errors.New("a catalog file must be a JSON object")
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return 0, 0, err
		}
		if tok != key {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return 0, 0, err
			}
			continue
		}
		if tok, err := dec.Token(); err != nil || tok != open {
			return 0, 0, fmt.Errorf("%s has the wrong type", key)
		}
		end, n := int(dec.InputOffset()), 0
		for dec.More() {
			if open == '{' {
				if _, err := dec.Token(); err != nil {
					return 0, 0, err
				}
			}
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return 0, 0, err
			}
			end, n = int(dec.InputOffset()), n+1
		}
		return end, n, nil
	}
	return 0, 0, fmt.Errorf("the file has no %s to add stubs to", key)
}

// writeReconcileReport renders the report as Markdown, which reads in a
// terminal and is the body of the pull request the scheduled check opens.
func writeReconcileReport(w io.Writer, rep reconcileReport) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# Model catalog reconcile\n\nChecked %s against catalog revision %s.\n\n", rep.CheckedAt, dash(rep.Revision))
	b.WriteString("| Provider | Status | Listed | Declared | Inferred | Ignored | New | Limit drift | Disappeared |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- | --- | --- | --- |\n")
	for _, p := range rep.Providers {
		status := p.Status
		if p.Reason != "" {
			status += ": " + p.Reason
		}
		fmt.Fprintf(&b, "| %s | %s | %d | %d | %d | %d | %d | %d | %d |\n", p.Provider, mdCell(status),
			p.Listed, p.Declared, p.Inferred, p.Ignored, len(p.New), len(p.LimitDrift), len(p.Disappeared))
	}

	var news, drifts, gone []string
	for _, p := range rep.Providers {
		for _, m := range p.New {
			line := fmt.Sprintf("- `%s/%s`", p.Provider, m.ID)
			var extra []string
			if m.DisplayName != "" && m.DisplayName != m.ID {
				extra = append(extra, m.DisplayName)
			}
			if m.Created != "" {
				extra = append(extra, "created "+m.Created)
			}
			if len(extra) > 0 {
				line += " (" + strings.Join(extra, ", ") + ")"
			}
			news = append(news, line)
		}
		for _, d := range p.LimitDrift {
			drifts = append(drifts, fmt.Sprintf("- `%s/%s` (row `%s`): %s", p.Provider, d.ID, d.Family, strings.Join(d.Drift, "; ")))
		}
		for _, prefix := range p.Disappeared {
			gone = append(gone, fmt.Sprintf("- `%s/%s`", p.Provider, prefix))
		}
	}
	section := func(title, intro string, lines []string) {
		if len(lines) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n## %s\n\n%s\n\n%s\n", title, intro, strings.Join(lines, "\n"))
	}
	section("New models", "Only the provider baseline covers these models.", news)
	if len(rep.Stubs) > 0 {
		var stubs []string
		for _, s := range rep.Stubs {
			stubs = append(stubs, fmt.Sprintf("- `%s` on `%s`", s.Key, s.Offering.Endpoint))
		}
		intro := "Stub rows: a model with no facts and an offering on the vendor's primary endpoint that extends its baseline template, no price, and the note `needs review`. " +
			"A stub's prefix also covers the dated IDs that start with it."
		if rep.Written != nil {
			intro += fmt.Sprintf(" Added to `%s`: %d.", rep.Written.Path, len(rep.Written.Added))
		}
		section("Stub rows", intro, stubs)
	}
	section("Limit drift", "The endpoint reports limits that disagree with the catalog row.", drifts)
	section("Disappeared upstream", "No listed model matches these rows and they name no successor: "+
		"candidates for `superseded_by`, or for removal. A model the key cannot access is also missing from the listing.", gone)
	var pending []string
	for _, r := range rep.PendingReview {
		pending = append(pending, fmt.Sprintf("- `%s`", r))
	}
	section("Pending review", "These rows still carry the `needs review` note.", pending)
	var stale []string
	for _, s := range rep.StalePricing {
		age := "not a date"
		if s.AgeDays > 0 {
			age = fmt.Sprintf("%d days", s.AgeDays)
		}
		stale = append(stale, fmt.Sprintf("- `%s` %s as of %s (%s)", s.Row, s.Field, s.AsOf, age))
	}
	section("Stale pricing", fmt.Sprintf("List endpoints do not report prices. These rates are older than %d days; "+
		"check them against the vendor's price list and update `as_of`.", rep.StaleAfter), stale)

	if rep.Drift {
		b.WriteString("\nResult: drift found.\n")
	} else {
		b.WriteString("\nResult: no drift.\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// mdCell keeps a table cell on one line.
func mdCell(s string) string {
	return strings.NewReplacer("|", "\\|", "\n", " ").Replace(s)
}
