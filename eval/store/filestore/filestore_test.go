package filestore_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/eval"
	"github.com/urmzd/saige/eval/store"
	"github.com/urmzd/saige/eval/store/filestore"
	"github.com/urmzd/saige/eval/store/storetest"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		s, err := filestore.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

func put(t *testing.T, s store.Store, runID, obsID string, value float64) {
	t.Helper()
	_, err := s.PutUnit(context.Background(), eval.Unit{
		RunID:       runID,
		Observation: eval.Observation{ID: obsID},
		Scores:      []eval.Score{{Name: "m", Value: value}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestReopenKeepsAttemptNumbering(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := filestore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRun(ctx, eval.RunRecord{ID: "r", Suite: "s", Status: eval.RunRunning, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	put(t, s, "r", "q", 0.1)
	put(t, s, "r", "q", 0.2)

	reopened, err := filestore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.PutUnit(ctx, eval.Unit{RunID: "r", Observation: eval.Observation{ID: "q"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempt != 3 {
		t.Fatalf("attempt after reopen = %d, want 3", got.Attempt)
	}
	prior, err := reopened.Attempts(ctx, "r", "q/0")
	if err != nil || len(prior) != 2 {
		t.Fatalf("Attempts = %v, %v; want 2", prior, err)
	}
}

func TestPartialLastLine(t *testing.T) {
	tests := []struct {
		name string
		tail string
	}{
		{"truncated record", `{"run_id":"r","key":"q2/0","att`},
		{"no newline after complete record", `{"run_id":"r","key":"q2/0","attempt":1,"observation":{"id":"q2","turn":0,"input":null,"output":null,"timing":{"total_ms":0}},"scores":null}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			s, err := filestore.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.CreateRun(ctx, eval.RunRecord{ID: "r", Status: eval.RunRunning, StartedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			put(t, s, "r", "q1", 1)

			path := filepath.Join(dir, "r", "units.jsonl")
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString(tt.tail); err != nil {
				t.Fatal(err)
			}
			_ = f.Close()

			units, err := s.Units(ctx, "r", store.UnitFilter{})
			if err != nil {
				t.Fatalf("Units with a partial tail: %v", err)
			}
			if len(units) != 1 {
				t.Fatalf("Units = %d, want the 1 complete record", len(units))
			}

			put(t, s, "r", "q3", 1)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
			if len(lines) != 2 {
				t.Fatalf("unit log has %d lines after repair, want 2:\n%s", len(lines), data)
			}
			for i, line := range lines {
				if !json.Valid([]byte(line)) {
					t.Fatalf("line %d is not JSON after repair: %q", i, line)
				}
			}
		})
	}
}

func TestRunRecordIsAtomicFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := filestore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	run := eval.RunRecord{ID: "r", Suite: "s", Status: eval.RunRunning, StartedAt: time.Now()}
	if err := s.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	run.Status = eval.RunSucceeded
	if err := s.UpdateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "r"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "r", "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got eval.RunRecord
	if err := json.Unmarshal(data, &got); err != nil || got.Status != eval.RunSucceeded {
		t.Fatalf("run.json = %s (%v)", data, err)
	}
}

func TestListRunsSkipsIncompleteDirectories(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := filestore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "orphan"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRun(ctx, eval.RunRecord{ID: "r", Status: eval.RunSucceeded, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	runs, err := s.ListRuns(ctx, store.RunFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != "r" {
		t.Fatalf("ListRuns = %+v, want only r", runs)
	}
}
