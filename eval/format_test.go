package eval

import (
	"encoding/json"
	"math"
	"os"
	"testing"
	"time"
)

func TestWriteReadExperiment(t *testing.T) {
	dir := t.TempDir()

	original := &ExperimentResult{
		Name:      "round-trip",
		CreatedAt: time.Now().Truncate(time.Second),
		BaseResults: []ObservationResult{
			{
				Observation: Observation{
					ID:          "o1",
					Sample:      2,
					Labels:      Labels{"topic": "billing"},
					Input:       json.RawMessage(`"q"`),
					Output:      json.RawMessage(`"base"`),
					Annotations: map[string]json.RawMessage{"k": json.RawMessage(`1`)},
					Timing:      ObservationTiming{TotalMs: 5},
				},
				Scores: []Score{{Name: "accuracy", Value: 0.9}},
			},
		},
		ExpResults: []ObservationResult{
			{
				Observation: Observation{ID: "o1", Sample: 2, Output: json.RawMessage(`"exp"`)},
				Scores:      []Score{{Name: "accuracy", Value: 0.95}},
			},
		},
		BaseAggregate: map[string]float64{"accuracy": 0.9},
		ExpAggregate:  map[string]float64{"accuracy": 0.95},
		Deltas:        map[string]float64{"accuracy": 0.05},
	}

	if err := WriteExperiment(dir, original); err != nil {
		t.Fatal(err)
	}

	// Verify files exist.
	for _, path := range []string{
		dir + "/result.json",
		dir + "/inputs/000.json",
		dir + "/outputs/base/000.json",
		dir + "/outputs/exp/000.json",
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected file %s to exist", path)
		}
	}

	// inputs/ carries only dataset fields, never the base arm's output.
	raw, err := os.ReadFile(dir + "/inputs/000.json")
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"output", "annotations", "timing"} {
		if _, ok := keys[key]; ok {
			t.Errorf("inputs/000.json must not contain %q: %s", key, raw)
		}
	}
	var row Observation
	if err := json.Unmarshal(raw, &row); err != nil || row.ID != "o1" || row.Sample != 2 || row.Labels["topic"] != "billing" {
		t.Errorf("inputs/000.json should load as an Observation, got %+v (%v)", row, err)
	}

	// Read back.
	loaded, err := ReadExperiment(dir)
	if err != nil {
		t.Fatal(err)
	}

	if loaded.Name != original.Name {
		t.Errorf("name: got %q, want %q", loaded.Name, original.Name)
	}
	if len(loaded.BaseResults) != 1 {
		t.Fatalf("expected 1 base result, got %d", len(loaded.BaseResults))
	}
	if math.Abs(loaded.Deltas["accuracy"]-0.05) > 0.001 {
		t.Errorf("delta: got %f, want 0.05", loaded.Deltas["accuracy"])
	}
}

func TestWriteReadSuiteResult(t *testing.T) {
	path := t.TempDir() + "/suite.json"

	original := &SuiteResult{
		Name:      "suite-test",
		CreatedAt: time.Now().Truncate(time.Second),
		Results: []ObservationResult{
			{
				Observation: Observation{ID: "s1"},
				Scores:      []Score{{Name: "f1", Value: 0.88}},
			},
		},
		Aggregate: map[string]float64{"f1": 0.88},
	}

	if err := WriteSuiteResult(path, original); err != nil {
		t.Fatal(err)
	}

	loaded, err := ReadSuiteResult(path)
	if err != nil {
		t.Fatal(err)
	}

	if loaded.Name != original.Name {
		t.Errorf("name: got %q, want %q", loaded.Name, original.Name)
	}
	if math.Abs(loaded.Aggregate["f1"]-0.88) > 0.001 {
		t.Errorf("aggregate f1: got %f, want 0.88", loaded.Aggregate["f1"])
	}
}

func TestWriteExperimentPairsArmsByCase(t *testing.T) {
	res := func(id string, sample int, out string) ObservationResult {
		return ObservationResult{Observation: Observation{
			ID: id, Sample: sample, Input: json.RawMessage(`"in-` + id + `"`), Output: json.RawMessage(`"` + out + `"`),
		}}
	}
	tests := []struct {
		name string
		base []ObservationResult
		exp  []ObservationResult
		// want maps file number to the case ID expected in inputs/ and in
		// each arm's output ("" means no file).
		want map[string][3]string
	}{
		{
			name: "exp missing a middle case",
			base: []ObservationResult{res("a", 0, "b"), res("b", 0, "b"), res("c", 0, "b")},
			exp:  []ObservationResult{res("a", 0, "e"), res("c", 0, "e")},
			want: map[string][3]string{"000": {"a", "a", "a"}, "001": {"b", "b", ""}, "002": {"c", "c", "c"}},
		},
		{
			name: "base missing a case",
			base: []ObservationResult{res("a", 0, "b"), res("c", 0, "b")},
			exp:  []ObservationResult{res("a", 0, "e"), res("b", 0, "e"), res("c", 0, "e")},
			want: map[string][3]string{"000": {"a", "a", "a"}, "001": {"c", "c", "c"}, "002": {"b", "", "b"}},
		},
		{
			name: "samples and repeated keys pair by occurrence",
			base: []ObservationResult{res("a", 0, "b"), res("a", 1, "b"), res("a", 1, "b")},
			exp:  []ObservationResult{res("a", 1, "e"), res("a", 1, "e")},
			want: map[string][3]string{"000": {"a", "a", ""}, "001": {"a", "a", "a"}, "002": {"a", "a", "a"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := WriteExperiment(dir, &ExperimentResult{Name: "cancelled", BaseResults: tc.base, ExpResults: tc.exp}); err != nil {
				t.Fatal(err)
			}
			for num, want := range tc.want {
				for i, sub := range []string{"inputs", "outputs/base", "outputs/exp"} {
					path := dir + "/" + sub + "/" + num + ".json"
					raw, err := os.ReadFile(path)
					if want[i] == "" {
						if err == nil {
							t.Errorf("%s should not exist, got %s", path, raw)
						}
						continue
					}
					if err != nil {
						t.Errorf("%s: %v", path, err)
						continue
					}
					var id string
					if sub == "inputs" {
						var row Observation
						if err := json.Unmarshal(raw, &row); err != nil {
							t.Fatal(err)
						}
						id = row.ID
					} else {
						var r ObservationResult
						if err := json.Unmarshal(raw, &r); err != nil {
							t.Fatal(err)
						}
						id = r.Observation.ID
					}
					if id != want[i] {
						t.Errorf("%s holds case %q, want %q", path, id, want[i])
					}
				}
			}
			// Paired files must hold the same sample too.
			for num := range tc.want {
				var b, e ObservationResult
				braw, berr := os.ReadFile(dir + "/outputs/base/" + num + ".json")
				eraw, eerr := os.ReadFile(dir + "/outputs/exp/" + num + ".json")
				if berr != nil || eerr != nil {
					continue
				}
				_ = json.Unmarshal(braw, &b)
				_ = json.Unmarshal(eraw, &e)
				if b.Observation.Sample != e.Observation.Sample {
					t.Errorf("%s pairs sample %d with %d", num, b.Observation.Sample, e.Observation.Sample)
				}
			}
		})
	}
}
