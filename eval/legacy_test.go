package eval_test

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/urmzd/saige/eval"
	"github.com/urmzd/saige/eval/online"
	"github.com/urmzd/saige/eval/store"
	"github.com/urmzd/saige/eval/store/filestore"
)

// The files under testdata/legacy-v0.32 were written by the v0.32 release:
// a suite result, a file store run, and promoted cases. They must stay
// readable, and their inputs must keep their meaning.
const legacyDir = "testdata/legacy-v0.32"

func TestLegacySuiteResultReads(t *testing.T) {
	suite, err := eval.ReadSuiteResult(filepath.Join(legacyDir, "suite_result.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(suite.Results) != 1 || suite.Aggregate["judge_score"] != 0.9 {
		t.Fatalf("suite = %+v", suite)
	}
	obs := suite.Results[0].Observation
	in, err := eval.DecodeInput(obs.Input)
	if err != nil {
		t.Fatal(err)
	}
	if in.Kind != eval.InputText || in.Text() != "What is the capital of France?" || in.HasMedia() {
		t.Fatalf("input = %+v", in)
	}
	if _, ok := obs.Annotations["agent.tool_calls"]; !ok {
		t.Fatal("agent annotations lost")
	}
}

func TestLegacyFileStoreReads(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, filepath.Join(legacyDir, "store"), dir)
	st, err := filestore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	run, err := st.GetRun(ctx, "run-legacy")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != eval.RunSucceeded {
		t.Fatalf("run = %+v", run)
	}
	units, err := st.Units(ctx, "run-legacy", store.UnitFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 1 || units[0].Observation.ID != "q1" || len(units[0].Scores) != 2 {
		t.Fatalf("units = %+v", units)
	}
	if in, err := eval.DecodeInput(units[0].Observation.Input); err != nil || in.Kind != eval.InputText {
		t.Fatalf("input = %+v, %v", in, err)
	}
	// The store still appends to an old run.
	if _, err := st.PutUnit(ctx, eval.Unit{RunID: "run-legacy", Observation: units[0].Observation, Scores: units[0].Scores}); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyCasesRead(t *testing.T) {
	f, err := os.Open(filepath.Join(legacyDir, "cases.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var kinds []eval.InputKind
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var c online.Case
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		in, err := eval.DecodeInput(c.Observation().Input)
		if err != nil {
			t.Fatalf("case %s: %v", c.ID, err)
		}
		kinds = append(kinds, in.Kind)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(kinds) != 2 || kinds[0] != eval.InputText || kinds[1] != eval.InputOther {
		t.Fatalf("kinds = %v", kinds)
	}
}

func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, path)
		target := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}
