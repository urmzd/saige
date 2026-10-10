package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/eval"
	"github.com/urmzd/saige/eval/store"
	"github.com/urmzd/saige/eval/store/filestore"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files")

// compareRun stores one run of suite "nightly" with n cases: the first fail
// cases score 0 on the gated metric "correct", the next unmeasured cases
// lost their subject to an outage, and every measured case has a latency.
func compareRun(t *testing.T, s store.Store, id string, started time.Time, n, fail, unmeasured int, latency float64) {
	t.Helper()
	suite := &eval.SuiteResult{Name: "nightly", CreatedAt: started}
	for i := range n {
		obs := eval.Observation{ID: fmt.Sprintf("q%02d", i)}
		if i >= fail && i < fail+unmeasured {
			eval.MarkSubjectError(&obs, eval.Infra(errors.New("provider unavailable")))
			suite.Results = append(suite.Results, eval.ObservationResult{Observation: obs})
			suite.SubjectErrors++
			suite.Inconclusive++
			continue
		}
		v := 1.0
		if i < fail {
			v = 0
		}
		suite.Results = append(suite.Results, eval.ObservationResult{
			Observation: obs,
			Scores:      []eval.Score{{Name: "correct", Value: v}, {Name: "latency_ms", Value: latency + float64(i)}},
		})
	}
	suite.Aggregate = eval.Aggregate(suite.Results)
	suite.Gate(eval.Assertion{Metric: "correct", Op: eval.GTE, Threshold: 1})
	prov := eval.Provenance{GitCommit: fmt.Sprintf("c0ffee%02d", started.Hour()) + strings.Repeat("0", 32)}
	if _, err := store.SaveSuite(context.Background(), s, id, suite, nil, prov); err != nil {
		t.Fatal(err)
	}
}

// seedCompareStore stores a baseline and candidates that regressed, held,
// or were lost to an outage.
func seedCompareStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	s, err := filestore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 6, 1, 1, 0, 0, 0, time.UTC)
	compareRun(t, s, "base", at, 20, 0, 0, 100)
	compareRun(t, s, "regressed", at.Add(time.Hour), 20, 6, 0, 100)
	compareRun(t, s, "held", at.Add(2*time.Hour), 20, 1, 0, 100)
	compareRun(t, s, "outage", at.Add(3*time.Hour), 20, 0, 18, 100)
	compareRun(t, s, "slower", at.Add(4*time.Hour), 20, 0, 0, 400)
	main := eval.RunRecord{ID: "main-1", Suite: "main", Status: eval.RunSucceeded, StartedAt: at}
	if err := s.CreateRun(context.Background(), main); err != nil {
		t.Fatal(err)
	}
	return dir
}

// runEvalCLI runs the CLI and returns its exit code, stdout, and stderr.
func runEvalCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var errBuf bytes.Buffer
	var code int
	out := captureStdout(t, func() { code = run(context.Background(), args, &errBuf) })
	*persistentFlagVars.format = "human"
	return code, out, errBuf.String()
}

func TestEvalCompareExitCodes(t *testing.T) {
	dir := seedCompareStore(t)
	tests := []struct {
		name     string
		args     []string
		wantCode int
		want     []string
	}{
		{"significant regression", []string{"regressed", "--baseline", "base"}, 1, []string{"outcome    failed", "correct", "regressed: dropped by 0.3", "regressed cases (6)"}},
		{"one flipped case is noise", []string{"held", "--baseline", "base"}, 0, []string{"outcome    passed", "dropped by 0.05, not significant"}},
		{"point estimate counts it", []string{"held", "--baseline", "base", "--significance", "1"}, 1, []string{"outcome    failed"}},
		{"tolerated drop", []string{"regressed", "--baseline", "base", "--max-regression", "0.4"}, 0, []string{"outcome    passed"}},
		{"per-metric threshold", []string{"regressed", "--baseline", "base", "--max-regression", "0.4", "--threshold", "correct=0.05"}, 1, []string{"outcome    failed"}},
		{"outage is inconclusive", []string{"outage", "--baseline", "base"}, exitInconclusive, []string{"outcome    inconclusive", "2 paired cases, fewer than the 3 required", "unmeasured 0 baseline and 18 candidate results"}},
		{"latest baseline is the run before", []string{"held"}, 0, []string{"baseline   regressed", "+0.25"}},
		{"latest baseline skips an inconclusive run", []string{"slower", "--metric", "correct"}, 0, []string{"baseline   held"}},
		{"newest run of a suite", []string{"--suite", "nightly", "--baseline", "base", "--metric", "correct"}, 0, []string{"candidate  slower"}},
		{"lower is better", []string{"slower", "--baseline", "base", "--metric", "latency_ms", "--lower-is-better", "latency_ms"}, 1, []string{"mean, lower is better", "regressed"}},
		{"re-gate both runs", []string{"held", "--baseline", "base", "--assert", "correct>=1"}, 1, []string{"candidate gate: failed", "case q00 correct"}},
		{"re-gate tolerating the outage", []string{"outage", "--baseline", "base", "--assert", "aggregate:correct>=1", "--max-inconclusive", "1", "--min-cases", "2"}, 0, []string{"candidate gate: passed"}},
		{"baseline from another suite", []string{"held", "--baseline-suite", "main"}, exitInconclusive, []string{"baseline   main-1 (main"}},
		{"unknown metric", []string{"held", "--baseline", "base", "--metric", "nope"}, exitInconclusive, []string{"no case scored it on both arms"}},
		{"same run", []string{"base", "--baseline", "base"}, exitInvalid, nil},
		{"no candidate", []string{}, exitInvalid, nil},
		{"bad threshold", []string{"held", "--threshold", "correct"}, exitInvalid, nil},
		{"bad significance", []string{"held", "--significance", "0"}, exitInvalid, nil},
		{"bad format", []string{"--format", "yaml", "eval", "compare", "held"}, exitInvalid, nil},
		{"missing baseline", []string{"base"}, 1, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := tt.args
			if len(args) == 0 || args[0] != "--format" {
				args = append([]string{"eval", "compare"}, args...)
			}
			args = append(args, "--store", dir)
			code, out, stderr := runEvalCLI(t, args...)
			if code != tt.wantCode {
				t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, tt.wantCode, out, stderr)
			}
			for _, w := range tt.want {
				if !strings.Contains(out, w) {
					t.Errorf("output missing %q:\n%s", w, out)
				}
			}
		})
	}
}

func TestEvalCompareFormats(t *testing.T) {
	dir := seedCompareStore(t)
	for _, format := range []string{"human", "markdown", "junit", "json"} {
		t.Run(format, func(t *testing.T) {
			code, out, stderr := runEvalCLI(t, "--format", format, "eval", "compare", "regressed", "--baseline", "base",
				"--metric", "correct", "--metric", "latency_ms", "--lower-is-better", "latency_ms", "--store", dir)
			if code != 1 {
				t.Fatalf("exit %d, want 1: %s", code, stderr)
			}
			switch format {
			case "junit":
				var doc junitSuites
				if err := xml.Unmarshal([]byte(out), &doc); err != nil {
					t.Fatalf("junit output does not parse: %v\n%s", err, out)
				}
				if doc.Tests != 2 || doc.Failures != 1 || doc.Skipped != 0 {
					t.Fatalf("junit totals = %+v", doc)
				}
			case "json":
				var rep compareReport
				if err := json.Unmarshal([]byte(out), &rep); err != nil {
					t.Fatalf("json output does not parse: %v\n%s", err, out)
				}
				if rep.Outcome != eval.OutcomeFailed || rep.Baseline.ID != "base" || len(rep.Regressions) != 6 {
					t.Fatalf("json report = %+v", rep)
				}
				if got := compareSummary(rep); got != "failed (1 of 2 metrics regressed), regressed vs baseline base" {
					t.Fatalf("summary = %q", got)
				}
				return // the JSON carries timestamps; it is checked by decoding
			}
			checkGolden(t, filepath.Join("testdata", "compare", format+".golden"), out)
		})
	}
}

func TestEvalCompareInconclusiveFormats(t *testing.T) {
	dir := seedCompareStore(t)
	for _, format := range []string{"markdown", "junit"} {
		t.Run(format, func(t *testing.T) {
			code, out, _ := runEvalCLI(t, "--format", format, "eval", "compare", "outage", "--baseline", "base", "--store", dir)
			if code != exitInconclusive {
				t.Fatalf("exit %d, want %d", code, exitInconclusive)
			}
			checkGolden(t, filepath.Join("testdata", "compare", "inconclusive-"+format+".golden"), out)
		})
	}
}

func TestEvalCompareOutputFile(t *testing.T) {
	dir := seedCompareStore(t)
	path := filepath.Join(t.TempDir(), "report.xml")
	code, out, _ := runEvalCLI(t, "--format", "junit", "eval", "compare", "held", "--baseline", "base", "--store", dir, "--output", path)
	if code != 0 || out != "" {
		t.Fatalf("exit %d, stdout %q; want 0 and nothing on stdout", code, out)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), `<testsuites name="saige eval compare" tests="1" failures="0" skipped="0">`) {
		t.Fatalf("report file = %s, %v", data, err)
	}
}

// checkGolden compares got with the golden file, rewriting it with -update.
func checkGolden(t *testing.T, path, got string) {
	t.Helper()
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -run %s -update to create it)", err, t.Name())
	}
	if got != string(want) {
		t.Fatalf("output differs from %s (run with -update to accept):\n--- got\n%s\n--- want\n%s", path, got, want)
	}
}

func TestEvalCompareWithPostgresStore(t *testing.T) {
	dsn := freshEvalDSN(t)
	ctx := context.Background()
	s, closeStore, err := openEvalStore(ctx, dsn, "acme", true)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 6, 1, 1, 0, 0, 0, time.UTC)
	compareRun(t, s, "base", at, 20, 0, 0, 100)
	compareRun(t, s, "regressed", at.Add(time.Hour), 20, 6, 0, 100)
	compareRun(t, s, "outage", at.Add(2*time.Hour), 20, 0, 18, 100)
	closeStore()

	for _, tt := range []struct {
		run      string
		wantCode int
	}{
		{"regressed", 1},
		{"outage", exitInconclusive},
	} {
		code, out, stderr := runEvalCLI(t, "eval", "compare", tt.run, "--baseline", "base", "--store", dsn, "--tenant", "acme")
		if code != tt.wantCode {
			t.Fatalf("%s: exit %d, want %d\n%s\n%s", tt.run, code, tt.wantCode, out, stderr)
		}
	}
	// The latest baseline of the outage run skips nothing: regressed is
	// the newest succeeded run before it.
	code, out, _ := runEvalCLI(t, "eval", "compare", "outage", "--store", dsn, "--tenant", "acme")
	if code != exitInconclusive || !strings.Contains(out, "baseline   regressed") {
		t.Fatalf("latest baseline: exit %d\n%s", code, out)
	}
}
