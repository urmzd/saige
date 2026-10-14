package local

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/durable/internal/legacyrun"
)

// TestLegacyJournalResumes resumes a run the release before typed parts
// recorded and left suspended on an approval.
func TestLegacyJournalResumes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := "../../internal/testdata/legacy/durable/local"
	if err := os.CopyFS(dir, os.DirFS(src)); err != nil {
		t.Fatal(err)
	}
	// Keep the historical wire payload intact, but give its approval a fresh
	// deadline: this test exercises migration, not expiry of the captured run.
	files, err := filepath.Glob(filepath.Join(dir, "*", "state.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("fixture state files = %v, %v", files, err)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Interrupts map[string]struct {
			ExpiresAt string `json:"expires_at"`
		} `json:"interrupts"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	expires := fixture.Interrupts[legacyrun.Interrupt].ExpiresAt
	if expires == "" {
		t.Fatal("fixture approval has no expiry")
	}
	old, err := json.Marshal(expires)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := json.Marshal(time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(files[0], bytes.ReplaceAll(raw, old, fresh), 0600); err != nil {
		t.Fatal(err)
	}
	engine := New(dir)
	state, err := engine.Inspect(legacyrun.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != statusSuspended || len(state.Steps) != 4 {
		t.Fatalf("recorded state = %s with %d steps", state.Status, len(state.Steps))
	}
	if err := engine.Decide(legacyrun.RunID, "v1", legacyrun.Interrupt, "key", legacyrun.Approve); err != nil {
		t.Fatal(err)
	}
	var c legacyrun.Counters
	factory := func() *agent.Agent { return legacyrun.Agent(&c) }
	final, err := engine.Run(ctx, legacyrun.RunID, "v1", factory, legacyrun.Input())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range legacyrun.Check(&c, final) {
		t.Error(p)
	}
	// The steps the resumed run recorded are in the current format, and the
	// recorded run replays from the mixed journal without new work.
	matches, _ := filepath.Glob(filepath.Join(dir, "*", "state.json"))
	if len(matches) != 1 {
		t.Fatalf("state files = %v", matches)
	}
	var again legacyrun.Counters
	if _, err := engine.Run(ctx, legacyrun.RunID, "v1", func() *agent.Agent { return legacyrun.Agent(&again) }, nil); err != nil {
		t.Fatal(err)
	}
	if again.ModelCalls.Load() != 0 || again.Writes.Load() != 0 || again.Snapshots.Load() != 0 {
		t.Fatal("replaying the finished run repeated work")
	}
}
