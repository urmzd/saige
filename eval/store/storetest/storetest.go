// Package storetest is the conformance suite every eval/store.Store
// implementation runs, so stores behave the same behind the interface.
package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/eval"
	"github.com/urmzd/saige/eval/store"
)

// Run exercises a store implementation. newStore must return an empty store
// for each call.
func Run(t *testing.T, newStore func(t *testing.T) store.Store) {
	t.Helper()
	tests := []struct {
		name string
		fn   func(t *testing.T, s store.Store)
	}{
		{"CreateGetUpdate", testCreateGetUpdate},
		{"InvalidIDs", testInvalidIDs},
		{"ListRunsOrderAndFilter", testListRuns},
		{"PutUnitAssignsKeyAndAttempt", testPutUnit},
		{"ReplaceArchivesPriorAttempt", testReplace},
		{"UnitsFilter", testUnitsFilter},
		{"MissingRun", testMissingRun},
		{"CopiesOnPutAndGet", testCopies},
		{"SaveAndLoadSuite", testSaveAndLoadSuite},
		{"SaveSuiteRejectsDuplicateKeys", testSaveSuiteDuplicates},
		{"InconclusiveRoundTrip", testInconclusiveRoundTrip},
		{"LatestSucceeded", testLatestSucceeded},
		{"ConcurrentPuts", testConcurrentPuts},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.fn(t, newStore(t))
		})
	}
}

var base = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func newRun(id, suite string, status eval.RunStatus, started time.Time) eval.RunRecord {
	return eval.RunRecord{ID: id, Suite: suite, Status: status, StartedAt: started}
}

func unit(runID, obsID string, value float64) eval.Unit {
	return eval.Unit{
		RunID: runID,
		Observation: eval.Observation{
			ID:     obsID,
			Labels: eval.Labels{"topic": "t-" + obsID},
			Input:  json.RawMessage(`"in"`),
			Output: json.RawMessage(`"out"`),
		},
		Scores: []eval.Score{{Name: "m", Value: value}},
	}
}

func testCreateGetUpdate(t *testing.T, s store.Store) {
	ctx := context.Background()
	run := newRun("r1", "suite", eval.RunRunning, base)
	run.Provenance = eval.Provenance{GitCommit: "abc", GitDirty: true, Models: []string{"m1"}}
	if err := s.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := s.CreateRun(ctx, run); !errors.Is(err, store.ErrRunExists) {
		t.Fatalf("second CreateRun = %v, want ErrRunExists", err)
	}
	got, err := s.GetRun(ctx, "r1")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Status != eval.RunRunning || got.Provenance.GitCommit != "abc" || !got.Provenance.GitDirty || !got.StartedAt.Equal(base) {
		t.Fatalf("GetRun = %+v", got)
	}
	run.Status = eval.RunSucceeded
	run.FinishedAt = base.Add(time.Minute)
	run.Aggregate = map[string]float64{"m": 0.5}
	if err := s.UpdateRun(ctx, run); err != nil {
		t.Fatalf("UpdateRun: %v", err)
	}
	got, _ = s.GetRun(ctx, "r1")
	if got.Status != eval.RunSucceeded || got.Aggregate["m"] != 0.5 || !got.FinishedAt.Equal(run.FinishedAt) {
		t.Fatalf("after update = %+v", got)
	}
}

func testInvalidIDs(t *testing.T, s store.Store) {
	ctx := context.Background()
	for _, id := range []string{"", ".", "..", "a/b", `a\b`, "a\x00b"} {
		if err := s.CreateRun(ctx, newRun(id, "s", eval.RunRunning, base)); !errors.Is(err, store.ErrInvalidID) {
			t.Errorf("CreateRun(%q) = %v, want ErrInvalidID", id, err)
		}
	}
}

func testListRuns(t *testing.T, s store.Store) {
	ctx := context.Background()
	runs := []eval.RunRecord{
		newRun("a", "s1", eval.RunSucceeded, base),
		newRun("b", "s1", eval.RunErrored, base.Add(2*time.Hour)),
		newRun("c", "s2", eval.RunSucceeded, base.Add(time.Hour)),
		newRun("d", "s1", eval.RunSucceeded, base.Add(time.Hour)),
	}
	for _, r := range runs {
		if err := s.CreateRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name   string
		filter store.RunFilter
		want   []string
	}{
		{"all newest first", store.RunFilter{}, []string{"b", "d", "c", "a"}},
		{"suite", store.RunFilter{Suite: "s1"}, []string{"b", "d", "a"}},
		{"status", store.RunFilter{Status: []eval.RunStatus{eval.RunSucceeded}}, []string{"d", "c", "a"}},
		{"limit", store.RunFilter{Suite: "s1", Limit: 2}, []string{"b", "d"}},
		{"no match", store.RunFilter{Suite: "nope"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.ListRuns(ctx, tt.filter)
			if err != nil {
				t.Fatal(err)
			}
			if ids := runIDs(got); fmt.Sprint(ids) != fmt.Sprint(tt.want) {
				t.Fatalf("ListRuns = %v, want %v", ids, tt.want)
			}
		})
	}
}

func runIDs(runs []eval.RunRecord) []string {
	var ids []string
	for _, r := range runs {
		ids = append(ids, r.ID)
	}
	return ids
}

func testPutUnit(t *testing.T, s store.Store) {
	ctx := context.Background()
	if err := s.CreateRun(ctx, newRun("r", "s", eval.RunRunning, base)); err != nil {
		t.Fatal(err)
	}
	got, err := s.PutUnit(ctx, unit("r", "q1", 1))
	if err != nil {
		t.Fatalf("PutUnit: %v", err)
	}
	if got.Key != "q1/0" || got.Attempt != 1 || got.RecordedAt.IsZero() {
		t.Fatalf("PutUnit = key %q attempt %d recorded %v", got.Key, got.Attempt, got.RecordedAt)
	}
	units, err := s.Units(ctx, "r", store.UnitFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 1 || units[0].Scores[0].Value != 1 || string(units[0].Observation.Output) != `"out"` {
		t.Fatalf("Units = %+v", units)
	}
}

func testReplace(t *testing.T, s store.Store) {
	ctx := context.Background()
	if err := s.CreateRun(ctx, newRun("r", "s", eval.RunRunning, base)); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"q1", "q2"} {
		if _, err := s.PutUnit(ctx, unit("r", id, 0.1)); err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range []float64{0.2, 0.3} {
		if _, err := s.PutUnit(ctx, unit("r", "q1", v)); err != nil {
			t.Fatal(err)
		}
	}
	units, err := s.Units(ctx, "r", store.UnitFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 2 || units[0].Key != "q1/0" || units[1].Key != "q2/0" {
		t.Fatalf("Units keys = %v, want first-put order [q1/0 q2/0]", unitKeys(units))
	}
	if units[0].Attempt != 3 || units[0].Scores[0].Value != 0.3 {
		t.Fatalf("current q1 = attempt %d value %v, want 3 and 0.3", units[0].Attempt, units[0].Scores[0].Value)
	}
	prior, err := s.Attempts(ctx, "r", "q1/0")
	if err != nil {
		t.Fatal(err)
	}
	if len(prior) != 2 || prior[0].Attempt != 1 || prior[0].Scores[0].Value != 0.1 || prior[1].Attempt != 2 || prior[1].Scores[0].Value != 0.2 {
		t.Fatalf("Attempts = %+v", prior)
	}
	none, err := s.Attempts(ctx, "r", "q2/0")
	if err != nil || len(none) != 0 {
		t.Fatalf("Attempts(q2) = %v, %v; want empty", none, err)
	}
}

func unitKeys(units []eval.Unit) []string {
	var keys []string
	for _, u := range units {
		keys = append(keys, u.Key)
	}
	return keys
}

func testUnitsFilter(t *testing.T, s store.Store) {
	ctx := context.Background()
	if err := s.CreateRun(ctx, newRun("r", "s", eval.RunRunning, base)); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"q1", "q2", "q3"} {
		if _, err := s.PutUnit(ctx, unit("r", id, 1)); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name   string
		filter store.UnitFilter
		want   []string
	}{
		{"all", store.UnitFilter{}, []string{"q1/0", "q2/0", "q3/0"}},
		{"where", store.UnitFilter{Where: eval.Where{"topic": "t-q2"}}, []string{"q2/0"}},
		{"observation", store.UnitFilter{ObservationID: "q3"}, []string{"q3/0"}},
		{"none", store.UnitFilter{Where: eval.Where{"topic": "x"}}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.Units(ctx, "r", tt.filter)
			if err != nil {
				t.Fatal(err)
			}
			if keys := unitKeys(got); fmt.Sprint(keys) != fmt.Sprint(tt.want) {
				t.Fatalf("Units = %v, want %v", keys, tt.want)
			}
		})
	}
}

func testMissingRun(t *testing.T, s store.Store) {
	ctx := context.Background()
	if _, err := s.GetRun(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetRun = %v, want ErrNotFound", err)
	}
	if err := s.UpdateRun(ctx, newRun("nope", "s", eval.RunSucceeded, base)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UpdateRun = %v, want ErrNotFound", err)
	}
	if _, err := s.PutUnit(ctx, unit("nope", "q", 1)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("PutUnit = %v, want ErrNotFound", err)
	}
	if _, err := s.Units(ctx, "nope", store.UnitFilter{}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Units = %v, want ErrNotFound", err)
	}
	if _, err := s.Attempts(ctx, "nope", "q/0"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Attempts = %v, want ErrNotFound", err)
	}
	if _, err := store.LatestSucceeded(ctx, s, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("LatestSucceeded = %v, want ErrNotFound", err)
	}
}

func testCopies(t *testing.T, s store.Store) {
	ctx := context.Background()
	run := newRun("r", "s", eval.RunRunning, base)
	run.Aggregate = map[string]float64{"m": 1}
	if err := s.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	run.Aggregate["m"] = 99

	u := unit("r", "q", 1)
	if _, err := s.PutUnit(ctx, u); err != nil {
		t.Fatal(err)
	}
	u.Scores[0].Value = 99
	u.Observation.Labels["topic"] = "changed"
	u.Observation.Output[1] = 'X'

	gotRun, _ := s.GetRun(ctx, "r")
	if gotRun.Aggregate["m"] != 1 {
		t.Fatalf("stored run changed through caller map: %v", gotRun.Aggregate)
	}
	units, _ := s.Units(ctx, "r", store.UnitFilter{})
	if units[0].Scores[0].Value != 1 || units[0].Observation.Labels["topic"] != "t-q" || string(units[0].Observation.Output) != `"out"` {
		t.Fatalf("stored unit changed through caller data: %+v", units[0])
	}
	units[0].Scores[0].Value = 42
	again, _ := s.Units(ctx, "r", store.UnitFilter{})
	if again[0].Scores[0].Value != 1 {
		t.Fatal("stored unit changed through returned data")
	}
}

func testSaveAndLoadSuite(t *testing.T, s store.Store) {
	ctx := context.Background()
	cost := 0.25
	suite := &eval.SuiteResult{
		Name:      "suite",
		CreatedAt: base,
		Results: []eval.ObservationResult{
			{Observation: eval.Observation{ID: "q1", Output: json.RawMessage(`"a"`), Timing: eval.ObservationTiming{CostUSD: &cost}},
				Scores: []eval.Score{{Name: "m", Value: 1}}},
			{Observation: eval.Observation{ID: "q2", Output: json.RawMessage(`"b"`)},
				Scores: []eval.Score{{Name: "m", Value: 0}}},
		},
	}
	suite.Aggregate = eval.Aggregate(suite.Results)
	suite.Gate(eval.Assertion{Metric: "m", Op: eval.GTE, Threshold: 0.5})

	run, err := store.SaveSuite(ctx, s, "r1", suite, nil, eval.Provenance{GitCommit: "abc"})
	if err != nil {
		t.Fatalf("SaveSuite: %v", err)
	}
	if run.Status != eval.RunSucceeded || run.Outcome != eval.OutcomeFailed || run.Units != 2 || run.CostUSD == nil || *run.CostUSD != 0.25 {
		t.Fatalf("SaveSuite run = %+v", run)
	}
	loaded, err := store.LoadSuite(ctx, s, "r1")
	if err != nil {
		t.Fatalf("LoadSuite: %v", err)
	}
	if loaded.Name != "suite" || len(loaded.Results) != 2 || loaded.Aggregate["m"] != 0.5 || loaded.Outcome != eval.OutcomeFailed {
		t.Fatalf("LoadSuite = %+v", loaded)
	}
	if v := loaded.Check(eval.Assertion{Metric: "m", Op: eval.GTE, Threshold: 0.5}); len(v) != 1 || v[0].CaseID != "q2" {
		t.Fatalf("Check on loaded suite = %v, want one violation on q2", v)
	}
}

// testInconclusiveRoundTrip stores a suite with results that could not be
// measured and checks that the classification, the counts, and the gate
// outcome come back.
func testInconclusiveRoundTrip(t *testing.T, s store.Store) {
	ctx := context.Background()
	unmeasured := eval.Observation{ID: "q2"}
	eval.MarkSubjectError(&unmeasured, eval.Infra(errors.New("provider down")))
	suite := &eval.SuiteResult{
		Name:      "suite",
		CreatedAt: base,
		Results: []eval.ObservationResult{
			{Observation: eval.Observation{ID: "q1"}, Scores: []eval.Score{{Name: "m", Value: 1}, {Name: "judge", Error: "rate limited", Inconclusive: true}}},
			{Observation: unmeasured},
		},
		SubjectErrors: 1,
		Inconclusive:  2,
	}
	suite.Aggregate = eval.Aggregate(suite.Results)
	if got := suite.Gate(eval.Assertion{Metric: "m", Op: eval.GTE, Threshold: 1}); got != eval.OutcomeInconclusive {
		t.Fatalf("Gate = %s, want inconclusive", got)
	}
	run, err := store.SaveSuite(ctx, s, "r1", suite, nil, eval.Provenance{})
	if err != nil {
		t.Fatalf("SaveSuite: %v", err)
	}
	stored, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Outcome != eval.OutcomeInconclusive || stored.Inconclusive != 2 || len(stored.Violations) == 0 || !stored.Violations[0].Inconclusive() {
		t.Fatalf("stored run = %+v", stored)
	}
	loaded, err := store.LoadSuite(ctx, s, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Inconclusive != 2 || !eval.SubjectInconclusive(loaded.Results[1].Observation) || !loaded.Results[0].Scores[1].Inconclusive {
		t.Fatalf("LoadSuite = %+v", loaded)
	}
	if got := loaded.Gate(eval.Assertion{Metric: "m", Op: eval.GTE, Threshold: 1}); got != eval.OutcomeInconclusive {
		t.Fatalf("Gate on the loaded suite = %s, want inconclusive", got)
	}
}

func testSaveSuiteDuplicates(t *testing.T, s store.Store) {
	ctx := context.Background()
	result := func(id string, labels eval.Labels) eval.ObservationResult {
		return eval.ObservationResult{
			Observation: eval.Observation{ID: id, Labels: labels},
			Scores:      []eval.Score{{Name: "m", Value: 1}},
		}
	}
	tests := []struct {
		name    string
		results []eval.ObservationResult
		dupKey  string
	}{
		{"repeated case ID", []eval.ObservationResult{result("q1", nil), result("q1", nil)}, "q1/0"},
		{"labels outside variant", []eval.ObservationResult{
			result("q1", eval.Labels{"model": "a"}),
			result("q1", eval.Labels{"model": "b"}),
		}, "q1/0"},
		{"distinct variants", []eval.ObservationResult{
			result("q1", eval.Labels{eval.LabelVariant: "a"}),
			result("q1", eval.Labels{eval.LabelVariant: "b"}),
		}, ""},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := fmt.Sprintf("dup-%d", i)
			suite := &eval.SuiteResult{Name: "suite", CreatedAt: base, Results: tt.results}
			_, err := store.SaveSuite(ctx, s, id, suite, nil, eval.Provenance{})
			if tt.dupKey == "" {
				if err != nil {
					t.Fatalf("SaveSuite: %v", err)
				}
				units, err := s.Units(ctx, id, store.UnitFilter{})
				if err != nil || len(units) != len(tt.results) {
					t.Fatalf("Units = %d, %v; want %d", len(units), err, len(tt.results))
				}
				return
			}
			if !errors.Is(err, store.ErrDuplicateUnit) {
				t.Fatalf("SaveSuite err = %v, want ErrDuplicateUnit", err)
			}
			if want := fmt.Sprintf("%q", tt.dupKey); !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name key %s", err, want)
			}
			if _, err := s.GetRun(ctx, id); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("GetRun after rejected save = %v, want ErrNotFound", err)
			}
		})
	}
}

func testLatestSucceeded(t *testing.T, s store.Store) {
	ctx := context.Background()
	for _, r := range []eval.RunRecord{
		newRun("old", "s", eval.RunSucceeded, base),
		newRun("mid", "s", eval.RunSucceeded, base.Add(time.Hour)),
		newRun("new-failed", "s", eval.RunErrored, base.Add(2*time.Hour)),
		newRun("new-canceled", "s", eval.RunCanceled, base.Add(3*time.Hour)),
		newRun("other", "t", eval.RunSucceeded, base.Add(4*time.Hour)),
	} {
		if err := s.CreateRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.LatestSucceeded(ctx, s, "s")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "mid" {
		t.Fatalf("LatestSucceeded = %q, want mid", got.ID)
	}
}

func testConcurrentPuts(t *testing.T, s store.Store) {
	ctx := context.Background()
	if err := s.CreateRun(ctx, newRun("r", "s", eval.RunRunning, base)); err != nil {
		t.Fatal(err)
	}
	const workers, perWorker = 8, 5
	var wg sync.WaitGroup
	errs := make(chan error, workers*perWorker)
	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range perWorker {
				// Every worker also rewrites the shared key, so attempt
				// numbering is exercised under contention.
				if _, err := s.PutUnit(ctx, unit("r", fmt.Sprintf("w%d-%d", w, i), 1)); err != nil {
					errs <- err
				}
				if _, err := s.PutUnit(ctx, unit("r", "shared", float64(w))); err != nil {
					errs <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	units, err := s.Units(ctx, "r", store.UnitFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != workers*perWorker+1 {
		t.Fatalf("Units = %d, want %d", len(units), workers*perWorker+1)
	}
	prior, err := s.Attempts(ctx, "r", "shared/0")
	if err != nil {
		t.Fatal(err)
	}
	if len(prior) != workers*perWorker-1 {
		t.Fatalf("Attempts(shared) = %d, want %d", len(prior), workers*perWorker-1)
	}
	for i, p := range prior {
		if p.Attempt != i+1 {
			t.Fatalf("attempt %d numbered %d", i, p.Attempt)
		}
	}
}
