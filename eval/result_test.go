package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

func TestUnitKey(t *testing.T) {
	tests := []struct {
		obs  Observation
		want string
	}{
		{Observation{ID: "q1"}, "q1/0"},
		{Observation{ID: "q1", Turn: 2}, "q1/2"},
		{Observation{ID: "q1", Sample: 3}, "q1/0#3"},
		{Observation{ID: "q1", Sample: 1, Labels: Labels{LabelVariant: "cand", "topic": "x"}}, "q1/0#1@cand"},
	}
	for _, tt := range tests {
		if got := UnitKey(tt.obs); got != tt.want {
			t.Errorf("UnitKey(%+v) = %q, want %q", tt.obs, got, tt.want)
		}
	}
}

func TestStatusOf(t *testing.T) {
	tests := []struct {
		err  error
		want RunStatus
	}{
		{nil, RunSucceeded},
		{fmt.Errorf("suite: %w", context.Canceled), RunCanceled},
		{context.DeadlineExceeded, RunCanceled},
		{errors.New("all scorers errored"), RunErrored},
	}
	for _, tt := range tests {
		if got := StatusOf(tt.err); got != tt.want {
			t.Errorf("StatusOf(%v) = %q, want %q", tt.err, got, tt.want)
		}
		if !tt.want.Finished() {
			t.Errorf("%q is not finished", tt.want)
		}
	}
	if RunRunning.Finished() {
		t.Error("running reported finished")
	}
}

func TestNewRunRecordAndRoundTrip(t *testing.T) {
	a, b := 0.1, 0.2
	suite := &SuiteResult{
		Name:      "s",
		Claim:     "cheaper",
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Results: []ObservationResult{
			{Observation: Observation{ID: "q1", Timing: ObservationTiming{CostUSD: &a}}, Scores: []Score{{Name: "m", Value: 1}}},
			{Observation: Observation{ID: "q2", Timing: ObservationTiming{CostUSD: &b}}, Scores: []Score{{Name: "m", Error: "boom"}}},
			{Observation: Observation{ID: "q3"}, Scores: []Score{{Name: "m", Value: 0}}},
		},
		ErroredCases: 1,
		Incomplete:   2,
	}
	suite.Aggregate = Aggregate(suite.Results)
	suite.Gate(Assertion{Metric: "m", Op: GTE, Threshold: 1})

	prov := Provenance{GitCommit: "abc", Models: []string{"m1"}, Extra: map[string]string{"k": "v"}}
	run := NewRunRecord("r1", suite, fmt.Errorf("stopped: %w", context.Canceled), prov)
	prov.Models[0] = "changed"
	prov.Extra["k"] = "changed"

	if run.Status != RunCanceled || run.Error == "" || run.Suite != "s" || run.Claim != "cheaper" || run.Units != 3 ||
		run.ErroredCases != 1 || run.Incomplete != 2 || run.Outcome != OutcomeFailed || len(run.Violations) == 0 {
		t.Fatalf("run = %+v", run)
	}
	if run.CostUSD == nil || *run.CostUSD < 0.3-1e-9 || *run.CostUSD > 0.3+1e-9 {
		t.Fatalf("cost = %v, want 0.3", run.CostUSD)
	}
	if run.Provenance.Models[0] != "m1" || run.Provenance.Extra["k"] != "v" {
		t.Fatal("provenance shares memory with the caller")
	}

	units := suite.Units("r1")
	if len(units) != 3 || units[0].RunID != "r1" || units[0].Key != "q1/0" || units[0].Attempt != 0 {
		t.Fatalf("units = %+v", units)
	}
	data, err := json.Marshal(units)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []Unit
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	rebuilt := SuiteFromUnits(run, decoded)
	if rebuilt.Name != "s" || rebuilt.Aggregate["m"] != 0.5 || rebuilt.Outcome != OutcomeFailed || rebuilt.Incomplete != 2 {
		t.Fatalf("rebuilt = %+v", rebuilt)
	}
	if v := rebuilt.Check(Assertion{Metric: "m", Op: GTE, Threshold: 1}); len(v) == 0 {
		t.Fatal("rebuilt suite passed a gate the original failed")
	}
}

func TestTotalCostUSDUnrecorded(t *testing.T) {
	if c := TotalCostUSD([]ObservationResult{{}, {}}); c != nil {
		t.Fatalf("cost = %v, want nil when nothing recorded a cost", *c)
	}
}

func TestNewRunID(t *testing.T) {
	re := regexp.MustCompile(`^\d{8}T\d{6}Z-[0-9a-f]{8}$`)
	a, b := NewRunID(), NewRunID()
	if !re.MatchString(a) || a == b {
		t.Fatalf("NewRunID = %q, %q", a, b)
	}
}

func TestProvenanceAddModels(t *testing.T) {
	var p Provenance
	p.AddModels("b", "", "a", "b")
	p.AddModels("a", "c")
	if fmt.Sprint(p.Models) != "[a b c]" {
		t.Fatalf("Models = %v", p.Models)
	}
}

func TestCaptureProvenanceGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "a.txt")
	git("-c", "commit.gpgsign=false", "commit", "-q", "-m", "init")

	ctx := context.Background()
	clean := CaptureProvenance(ctx, dir, "model-b", "model-a")
	if len(clean.GitCommit) != 40 || clean.GitDirty {
		t.Fatalf("clean provenance = %+v", clean)
	}
	if fmt.Sprint(clean.Models) != "[model-a model-b]" || clean.GoVersion == "" || clean.CapturedAt.IsZero() {
		t.Fatalf("provenance fields = %+v", clean)
	}

	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("b"), 0o600); err != nil {
		t.Fatal(err)
	}
	dirty := CaptureProvenance(ctx, dir)
	if dirty.GitCommit != clean.GitCommit || !dirty.GitDirty {
		t.Fatalf("dirty provenance = %+v", dirty)
	}
}

func TestCaptureProvenanceOutsideRepository(t *testing.T) {
	p := CaptureProvenance(context.Background(), t.TempDir())
	if p.GoVersion == "" {
		t.Fatalf("provenance = %+v", p)
	}
}
