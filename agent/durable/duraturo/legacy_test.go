package duraturo

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/urmzd/duraturo/pkg/ledger"
	"github.com/urmzd/duraturo/pkg/queue"
	"github.com/urmzd/duraturo/pkg/run"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/durable/internal/legacyrun"
	"github.com/urmzd/saige/agent/types"
)

const legacyDir = "../../internal/testdata/legacy/durable/"

// TestLegacyLedgerResumes loads the ledger of a run the release before
// typed parts recorded and left parked on an approval, and resumes it.
func TestLegacyLedgerResumes(t *testing.T) {
	ctx := testContext(t)
	raw, err := os.ReadFile(legacyDir + "duraturo/v1_ledger.json")
	if err != nil {
		t.Fatal(err)
	}
	var dump struct {
		Run     run.Run      `json:"run"`
		Records []run.Record `json:"records"`
	}
	if err := json.Unmarshal(raw, &dump); err != nil {
		t.Fatal(err)
	}
	lgr := ledger.NewMemory()
	if err := lgr.Accept(ctx, dump.Run); err != nil {
		t.Fatal(err)
	}
	for _, rec := range dump.Records {
		if _, err := lgr.Record(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	e := New(lgr, queue.NewMemory())
	var c legacyrun.Counters
	wf := e.Register("", func(string) *agent.Agent { return legacyrun.Agent(&c) })
	startWorker(t, e)

	state, err := e.Inspect(ctx, legacyrun.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Pending) != 1 || state.Pending[0].ID != legacyrun.Interrupt {
		t.Fatalf("pending = %+v", state.Pending)
	}
	// The recorded input still matches the input this release builds.
	if err := e.Start(ctx, wf, legacyrun.RunID, legacyrun.Input()); err != nil {
		t.Fatal(err)
	}
	if err := e.Decide(ctx, legacyrun.RunID, legacyrun.Interrupt, "key", legacyrun.Approve); err != nil {
		t.Fatal(err)
	}
	final, err := e.Wait(ctx, legacyrun.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range legacyrun.Check(&c, final) {
		t.Error(p)
	}
}

// TestLegacyBatchResults decodes batch results the previous release
// recorded.
func TestLegacyBatchResults(t *testing.T) {
	raw, err := os.ReadFile(legacyDir + "gob/batch_results.gob")
	if err != nil {
		t.Fatal(err)
	}
	var recorded []recordedResult
	if err := decode(raw, &recorded); err != nil {
		t.Fatal(err)
	}
	if len(recorded) != 2 || recorded[1].Err != "bad" || recorded[0].FinishReason != "stop" {
		t.Fatalf("recorded = %+v", recorded)
	}
	msg, err := recorded[0].Message.Message()
	if err != nil {
		t.Fatal(err)
	}
	if types.TextOf(msg) != "Looking." || len(msg.Parts) != 10 {
		t.Fatalf("message = %#v", msg)
	}
	empty, err := recorded[1].Message.Message()
	if err != nil || len(empty.Parts) != 0 {
		t.Fatalf("errored result message = %#v, %v", empty, err)
	}

}
