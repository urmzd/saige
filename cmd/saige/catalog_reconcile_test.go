package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/provider/catalog"
)

// fakeLister serves a fixed listing.
type fakeLister struct {
	models []catalog.RemoteModel
	err    error
}

func (f fakeLister) ListModels(context.Context) ([]catalog.RemoteModel, error) {
	return f.models, f.err
}

func remote(ids ...string) []catalog.RemoteModel {
	out := make([]catalog.RemoteModel, len(ids))
	for i, id := range ids {
		out[i] = catalog.RemoteModel{ID: id}
	}
	return out
}

// fakeListers answers newLister from a table; a provider absent from it fails.
func fakeListers(by map[string]fakeLister) func(context.Context, string) (catalog.ModelLister, error) {
	return func(_ context.Context, name string) (catalog.ModelLister, error) {
		l, ok := by[name]
		if !ok {
			return nil, errors.New("no fake for " + name)
		}
		return l, nil
	}
}

func envMap(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// servedRows lists every row prefix of a provider, so a listing of them
// reconciles with no drift.
func servedRows(provider string) []catalog.RemoteModel {
	var ids []string
	for _, m := range catalog.Default().Models {
		if m.Provider == provider {
			ids = append(ids, m.Prefix)
		}
	}
	return remote(ids...)
}

func TestStubRowUsesTheBaselineOnly(t *testing.T) {
	base := catalog.Default()
	stub := stubRow("anthropic", "claude-new-1", base.Baselines["anthropic"])
	if stub.Pricing != nil || stub.Tier != "" || stub.SupersededBy != "" || stub.Defaults != nil || stub.ServerToolFees != nil {
		t.Fatalf("a stub must not assert price, tier, successor, defaults or fees: %+v", stub)
	}
	if !slices.Equal(stub.Notes, []string{stubNote}) {
		t.Fatalf("notes %v", stub.Notes)
	}
	raw, _, err := appendStubRows(nil, []catalog.ModelSpec{stub})
	if err != nil {
		t.Fatal(err)
	}
	overlay, err := catalog.Load(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("stub does not load: %v\n%s", err, raw)
	}
	cat, err := catalog.Merge(base, overlay)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := cat.Lookup("anthropic", "claude-new-1")
	want, known := cat.Lookup("anthropic", "zz-not-a-model")
	if known || !slices.Equal(got.List(), want.List()) || got.ContextWindow != want.ContextWindow {
		t.Fatalf("stub capabilities %v, baseline %v", got.List(), want.List())
	}
	if !got.Pricing.IsZero() {
		t.Fatalf("stub is priced: %s", got.Pricing.Describe())
	}
}

func TestCollapsePrefixes(t *testing.T) {
	got := collapsePrefixes([]string{"gpt-x-2026-01-01", "gpt-x", "a", "gpt-x", "b-1", "b"})
	if want := []string{"a", "b", "gpt-x"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := stubPrefix("ollama", "qwen9:4b"); got != "qwen9" {
		t.Fatalf("ollama tag kept: %s", got)
	}
}

func TestAppendStubRowsKeepsExistingBytes(t *testing.T) {
	stubs := []catalog.ModelSpec{
		{Provider: "openai", Prefix: "gpt-new", Notes: []string{stubNote}},
		{Provider: "openai", Prefix: "gpt-4o", Notes: []string{stubNote}},
	}
	raw := catalog.DefaultJSON()
	out, added, err := appendStubRows(raw, stubs)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(added, []string{"openai/gpt-new"}) {
		t.Fatalf("added %v: an existing row must be skipped", added)
	}
	// Removing the inserted text gives back the file byte for byte.
	ins := len(out) - len(raw)
	i := bytes.Index(out, []byte(`,
    {
      "provider": "openai",
      "prefix": "gpt-new"`))
	if i < 0 || !bytes.Equal(append(slices.Clone(out[:i]), out[i+ins:]...), raw) {
		t.Fatalf("existing bytes changed")
	}
	cat, err := catalog.Load(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.Validate(); err != nil {
		t.Fatal(err)
	}
	if e, ok := cat.Describe("openai", "gpt-new"); !ok || e.Prefix != "gpt-new" {
		t.Fatal("stub row not loaded")
	}

	again, readded, err := appendStubRows(out, stubs)
	if err != nil || len(readded) != 0 || !bytes.Equal(again, out) {
		t.Fatalf("a second run must add nothing: %v %v", readded, err)
	}

	empty, _, err := appendStubRows([]byte("{\n  \"version\": 1,\n  \"models\": []\n}\n"), stubs[:1])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Load(bytes.NewReader(empty)); err != nil {
		t.Fatalf("empty array: %v\n%s", err, empty)
	}
	if _, _, err := appendStubRows([]byte(`{"version":1}`), stubs); err == nil {
		t.Fatal("a file without a models array must be refused")
	}
	if _, _, err := appendStubRows([]byte(`[]`), stubs); err == nil {
		t.Fatal("a non-object file must be refused")
	}
}

func TestReconcileWithFakeLister(t *testing.T) {
	catalogSandbox(t)
	now := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	anthropicRows := servedRows("anthropic")
	// claude-haiku-5 has no successor: dropping it is a disappearance.
	// claude-3-opus names one: dropping it is not.
	anthropicRows = slices.DeleteFunc(anthropicRows, func(m catalog.RemoteModel) bool {
		return m.ID == "claude-haiku-5" || m.ID == "claude-3-opus"
	})
	for i := range anthropicRows {
		if anthropicRows[i].ID == "claude-haiku-5-5" {
			anthropicRows[i].ContextWindow = 4096
		}
	}
	opts := reconcileOptions{
		providers: []string{"openai", "anthropic", "google"},
		getenv:    envMap(map[string]string{"OPENAI_API_KEY": "k", "ANTHROPIC_API_KEY": "k"}),
		newLister: fakeListers(map[string]fakeLister{
			"openai": {models: append(servedRows("openai"),
				catalog.RemoteModel{ID: "gpt-9-nova", DisplayName: "GPT-9 Nova", Created: now},
				catalog.RemoteModel{ID: "gpt-9-nova-2026-10-01"},
				catalog.RemoteModel{ID: "gpt-6-luna-2026-09-01"},
				catalog.RemoteModel{ID: "tts-9"})},
			"anthropic": {models: anthropicRows},
		}),
		ignore:     []string{"openai/tts-*"},
		staleAfter: defaultStaleAfter,
		now:        now,
	}
	rep, err := reconcileCatalog(context.Background(), catalog.Default(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Drift {
		t.Fatal("drift not reported")
	}
	byName := map[string]providerReconcile{}
	for _, p := range rep.Providers {
		byName[p.Provider] = p
	}
	oa := byName["openai"]
	if oa.Status != reconcileChecked || oa.Ignored != 1 || oa.Inferred < 1 || len(oa.New) != 2 || oa.New[0].Created != "2026-10-09" {
		t.Fatalf("openai %+v", oa)
	}
	if len(rep.Stubs) != 1 || rep.Stubs[0].Prefix != "gpt-9-nova" || rep.Stubs[0].Extends != "openai.chat" {
		t.Fatalf("stubs %+v", rep.Stubs)
	}
	an := byName["anthropic"]
	if !slices.Equal(an.Disappeared, []string{"claude-haiku-5"}) {
		t.Fatalf("disappeared %v", an.Disappeared)
	}
	if len(an.LimitDrift) != 1 || an.LimitDrift[0].ID != "claude-haiku-5-5" {
		t.Fatalf("limit drift %+v", an.LimitDrift)
	}
	if g := byName["google"]; g.Status != reconcileSkipped || !strings.Contains(g.Reason, "GOOGLE_API_KEY") {
		t.Fatalf("google %+v", g)
	}
}

func TestReconcileListingErrorFails(t *testing.T) {
	catalogSandbox(t)
	opts := reconcileOptions{providers: []string{"openai"}, getenv: envMap(map[string]string{"OPENAI_API_KEY": "k"}),
		newLister: fakeListers(map[string]fakeLister{"openai": {err: errors.New("boom")}}), now: time.Now()}
	rep, err := reconcileCatalog(context.Background(), catalog.Default(), opts)
	if err == nil || rep.Providers[0].Status != reconcileFailed {
		t.Fatalf("err %v, report %+v", err, rep.Providers)
	}
	opts.getenv = envMap(nil)
	if _, err := reconcileCatalog(context.Background(), catalog.Default(), opts); err == nil {
		t.Fatal("a run that checked nothing must fail")
	}
}

func TestPendingReviewAndStalePricing(t *testing.T) {
	overlay, err := catalog.Load(strings.NewReader(`{"version":1,"models":[
		{"provider":"openai","prefix":"gpt-stub","notes":["needs review"]},
		{"provider":"openai","prefix":"gpt-old","pricing":{"input_per_mtok":1,"output_per_mtok":2,"as_of":"2026-01-01"}},
		{"provider":"openai","prefix":"gpt-fresh","pricing":{"input_per_mtok":1,"output_per_mtok":2,"as_of":"2026-10-01"}},
		{"provider":"openai","prefix":"gpt-odd","pricing":{"input_per_mtok":1,"output_per_mtok":2,"as_of":"last spring"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.Merge(catalog.Default(), overlay)
	if err != nil {
		t.Fatal(err)
	}
	if got := pendingReview(cat); !slices.Contains(got, "openai/gpt-stub") {
		t.Fatalf("pending %v", got)
	}
	stale := stalePricing(cat, time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC), defaultStaleAfter)
	var rows []string
	for _, s := range stale {
		rows = append(rows, s.Row)
	}
	if !slices.Contains(rows, "openai/gpt-old") || !slices.Contains(rows, "openai/gpt-odd") || slices.Contains(rows, "openai/gpt-fresh") {
		t.Fatalf("stale %+v", stale)
	}
	for _, s := range stale {
		if strings.HasPrefix(s.Row, "ollama/") {
			t.Fatalf("free local rows are not priced rates: %+v", s)
		}
	}
}

// TestDefaultCatalogHasNoPendingStubs keeps an unfinished stub row out of
// the shipped catalog: its exact ID would resolve as a declared model.
func TestDefaultCatalogHasNoPendingStubs(t *testing.T) {
	if got := pendingReview(catalog.Default()); len(got) > 0 {
		t.Fatalf("complete these stub rows and remove their %q note before merging: %v", stubNote, got)
	}
}

func TestReconcileReportFormat(t *testing.T) {
	rep := reconcileReport{CheckedAt: "2026-10-09", Revision: "r1", Drift: true, StaleAfter: 90,
		Providers: []providerReconcile{
			{Provider: "openai", Status: reconcileChecked, Listed: 3, Declared: 1, Inferred: 1,
				New: []newModel{{ID: "gpt-9", DisplayName: "GPT 9", Created: "2026-10-01"}}},
			{Provider: "anthropic", Status: reconcileChecked, Listed: 2, Declared: 2, Disappeared: []string{"claude-x"},
				LimitDrift: []limitDrift{{ID: "claude-y", Family: "claude-y", Drift: []string{"context window: endpoint 1, catalog 2"}}}},
			{Provider: "google", Status: reconcileSkipped, Reason: "GOOGLE_API_KEY is not set"},
		},
		Stubs:         []catalog.ModelSpec{{Provider: "openai", Prefix: "gpt-9"}},
		Written:       &reconcileWriteState{Path: "c.json", Added: []string{"openai/gpt-9"}},
		PendingReview: []string{"openai/gpt-8"},
		StalePricing:  []stalePrice{{Row: "openai/gpt-4o", Field: "pricing", AsOf: "2026-01-01", AgeDays: 281}},
	}
	var b bytes.Buffer
	if err := writeReconcileReport(&b, rep); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"Checked 2026-10-09 against catalog revision r1.",
		"| google | skipped: GOOGLE_API_KEY is not set | 0 |",
		"## New models", "- `openai/gpt-9` (GPT 9, created 2026-10-01)",
		"## Stub rows", "Added to `c.json`: 1.",
		"## Limit drift", "- `anthropic/claude-y` (row `claude-y`): context window: endpoint 1, catalog 2",
		"## Disappeared upstream", "- `anthropic/claude-x`",
		"## Pending review", "- `openai/gpt-8`",
		"## Stale pricing", "older than 90 days", "- `openai/gpt-4o` pricing as of 2026-01-01 (281 days)",
		"Result: drift found.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}

	b.Reset()
	if err := writeReconcileReport(&b, reconcileReport{CheckedAt: "2026-10-09"}); err != nil {
		t.Fatal(err)
	}
	if out := b.String(); strings.Contains(out, "##") || !strings.Contains(out, "Result: no drift.") {
		t.Fatalf("clean report:\n%s", out)
	}
}

func TestReadIgnoreFile(t *testing.T) {
	dir := t.TempDir()
	name := writeFile(t, filepath.Join(dir, "ignore.txt"), "# comment\n\nopenai/tts-*  # trailing\nwhisper-1\n")
	got, err := readIgnoreFile(name)
	if err != nil || !slices.Equal(got, []string{"openai/tts-*", "whisper-1"}) {
		t.Fatalf("got %v, %v", got, err)
	}
	if !ignored(got, "openai", "tts-1-hd") || !ignored(got, "openai", "whisper-1") || ignored(got, "anthropic", "tts-1") {
		t.Fatal("matching")
	}
	bad := writeFile(t, filepath.Join(dir, "bad.txt"), "gpt-[\n")
	if _, err := readIgnoreFile(bad); err == nil {
		t.Fatal("a malformed glob must be an error")
	}
}

// useFakeListers swaps the lister factory for the test.
func useFakeListers(t *testing.T, by map[string]fakeLister) {
	t.Helper()
	prev := newReconcileLister
	newReconcileLister = fakeListers(by)
	t.Cleanup(func() { newReconcileLister = prev })
}

func TestCatalogReconcileExitCodes(t *testing.T) {
	home, _ := catalogSandbox(t)
	clearProviderEnv(t)
	t.Setenv("OPENAI_API_KEY", "k")

	useFakeListers(t, map[string]fakeLister{"openai": {models: servedRows("openai")}})
	if code, out := runCLI(t, "catalog", "reconcile", "--providers", "openai"); code != 0 || !strings.Contains(out, "Result: no drift.") {
		t.Fatalf("no drift: %d %s", code, out)
	}

	useFakeListers(t, map[string]fakeLister{"openai": {models: append(servedRows("openai"), remote("gpt-9-nova")...)}})
	target := filepath.Join(home, "stubs.json")
	code, out := runCLI(t, "--format", "json", "catalog", "reconcile", "--providers", "openai", "--write", target)
	if code != reconcileExitDrift {
		t.Fatalf("drift: %d %s", code, out)
	}
	var rep reconcileReport
	if err := json.NewDecoder(strings.NewReader(out)).Decode(&rep); err != nil {
		t.Fatalf("json report: %v\n%s", err, out)
	}
	if !rep.Drift || rep.Written == nil || !slices.Equal(rep.Written.Added, []string{"openai/gpt-9-nova"}) {
		t.Fatalf("report %+v", rep)
	}
	written, err := os.ReadFile(target)
	if err != nil || !bytes.Contains(written, []byte(`"needs review"`)) {
		t.Fatalf("stub file: %v\n%s", err, written)
	}
	// With the stub file as a layer, the model is declared, but the row
	// still waits for review, which keeps the run in drift.
	code, out = runCLI(t, "--catalog", target, "catalog", "reconcile", "--providers", "openai")
	if code != reconcileExitDrift || !strings.Contains(out, "## Pending review") || strings.Contains(out, "## New models") {
		t.Fatalf("pending review: %d %s", code, out)
	}

	useFakeListers(t, map[string]fakeLister{"openai": {err: errors.New("upstream down")}})
	if code, out := runCLI(t, "catalog", "reconcile", "--providers", "openai"); code != reconcileExitError || !strings.Contains(out, "upstream down") {
		t.Fatalf("listing error: %d %s", code, out)
	}
	if code, _ := runCLI(t, "catalog", "reconcile", "--providers", "anthropic"); code != reconcileExitError {
		t.Fatal("no configured provider must exit 2")
	}
	if code, _ := runCLI(t, "catalog", "reconcile", "--no-such-flag"); code != reconcileExitError {
		t.Fatal("a bad flag must exit 2")
	}
	if code, _ := runCLI(t, "catalog", "reconcile", "--providers", "openai", "--ignore-file", filepath.Join(home, "missing")); code != reconcileExitError {
		t.Fatal("a missing ignore file must exit 2")
	}
}
