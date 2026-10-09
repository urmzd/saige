package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/eval"
	"github.com/urmzd/saige/eval/store"
	"github.com/urmzd/saige/eval/store/filestore"
)

// captureStdout runs fn with os.Stdout redirected and returns what it wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	os.Stdout = orig
	_ = w.Close()
	return <-done
}

func seedResults(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	s, err := filestore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cost := 0.0125
	suite := &eval.SuiteResult{
		Name:      "smoke",
		CreatedAt: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
		Results: []eval.ObservationResult{
			{Observation: eval.Observation{ID: "q1", Timing: eval.ObservationTiming{CostUSD: &cost}}, Scores: []eval.Score{{Name: "exact_match", Value: 1}}},
			{Observation: eval.Observation{ID: "q2"}, Scores: []eval.Score{{Name: "exact_match", Value: 0}}},
		},
	}
	suite.Aggregate = eval.Aggregate(suite.Results)
	suite.Gate(eval.Assertion{Metric: "exact_match", Op: eval.GTE, Threshold: 1})
	prov := eval.Provenance{GitCommit: "0123456789abcdef0123", GitDirty: true, Models: []string{"m-1"}}
	if _, err := store.SaveSuite(context.Background(), s, "run-1", suite, nil, prov); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestEvalResultCommands(t *testing.T) {
	dir := seedResults(t)
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"runs table", []string{"eval", "runs", "--store", dir}, []string{"RUN", "run-1", "smoke", "succeeded", "failed", "$0.0125", "0123456789ab+dirty"}},
		{"runs filtered out", []string{"eval", "runs", "--store", dir, "--suite", "other"}, []string{"RUN"}},
		{"show", []string{"eval", "show", "run-1", "--store", dir}, []string{"suite", "smoke", "exact_match", "0.5", "m-1", "violations:", "case q2"}},
		{"scorers", []string{"eval", "scorers"}, []string{"json_schema", "regex_count", "calls_in_order", "tool_responds_within"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var errBuf bytes.Buffer
			var code int
			got := captureStdout(t, func() { code = run(context.Background(), tt.args, &errBuf) })
			if code != 0 {
				t.Fatalf("exit %d: %s", code, errBuf.String())
			}
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("output missing %q:\n%s", w, got)
				}
			}
		})
	}
}

func TestEvalShowJSONAndErrors(t *testing.T) {
	dir := seedResults(t)
	var errBuf bytes.Buffer
	var code int
	got := captureStdout(t, func() {
		code = run(context.Background(), []string{"--format", "json", "eval", "show", "run-1", "--store", dir}, &errBuf)
	})
	*persistentFlagVars.format = "human"
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errBuf.String())
	}
	var decoded struct {
		Run   eval.RunRecord `json:"run"`
		Units []eval.Unit    `json:"units"`
	}
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("show --format json is not JSON: %v\n%s", err, got)
	}
	if decoded.Run.ID != "run-1" || len(decoded.Units) != 2 || decoded.Units[0].Attempt != 1 {
		t.Fatalf("decoded = %+v", decoded)
	}

	errBuf.Reset()
	_ = captureStdout(t, func() {
		code = run(context.Background(), []string{"eval", "show", "missing", "--store", dir}, &errBuf)
	})
	if code == 0 || !strings.Contains(errBuf.String(), "not found") {
		t.Fatalf("missing run: exit %d, stderr %q", code, errBuf.String())
	}
	errBuf.Reset()
	_ = captureStdout(t, func() {
		code = run(context.Background(), []string{"eval", "runs", "--store", dir + "/nope"}, &errBuf)
	})
	if code == 0 {
		t.Fatal("a missing store directory was accepted")
	}
}
