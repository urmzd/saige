package pgqueue

import (
	"strings"
	"testing"

	"github.com/urmzd/saige/durable/adapters/postgres/pgkit"
	"github.com/urmzd/saige/durable/queue"
)

func TestNewDeltaQueueValidatesDeltaMapping(t *testing.T) {
	pool := lazyPool(t)

	tests := []struct {
		name    string
		mutate  func(dm *DeltaMapping)
		wantSub string
	}{
		{"missing table", func(dm *DeltaMapping) { dm.Table = "" }, "DeltaMapping.Table"},
		{"missing seq", func(dm *DeltaMapping) { dm.Seq = "" }, "DeltaMapping.Seq"},
		{"missing run id", func(dm *DeltaMapping) { dm.RunID = "" }, "DeltaMapping.RunID"},
		{"missing record key", func(dm *DeltaMapping) { dm.RecordKey = "" }, "DeltaMapping.RecordKey"},
		{"missing attempt", func(dm *DeltaMapping) { dm.Attempt = "" }, "DeltaMapping.Attempt"},
		{"missing kind", func(dm *DeltaMapping) { dm.Kind = "" }, "DeltaMapping.Kind"},
		{"missing payload", func(dm *DeltaMapping) { dm.Payload = "" }, "DeltaMapping.Payload"},
		{"missing channel", func(dm *DeltaMapping) { dm.Channel = "" }, "DeltaMapping.Channel"},
		{"unsafe channel", func(dm *DeltaMapping) { dm.Channel = `c"; NOTIFY x; --` }, "unsafe identifier"},
		{"long channel", func(dm *DeltaMapping) { dm.Channel = strings.Repeat("c", 64) }, "63-byte channel limit"},
		{"unsafe table", func(dm *DeltaMapping) { dm.Table = "Deltas" }, "lower_snake_case"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dm := DefaultDeltaMapping()
			tt.mutate(&dm)
			_, err := NewDeltaQueue(pool, DefaultMapping(), dm)
			if err == nil {
				t.Fatal("NewDeltaQueue = nil error, want error")
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Fatalf("NewDeltaQueue error = %q, want it to contain %q", err, tt.wantSub)
			}
		})
	}

	if _, err := NewDeltaQueue(pool, Mapping{}, DefaultDeltaMapping()); err == nil {
		t.Fatal("NewDeltaQueue with invalid queue mapping = nil error, want error")
	}
}

func TestWithListener(t *testing.T) {
	pool := lazyPool(t)
	if _, err := NewDeltaQueue(pool, DefaultMapping(), DefaultDeltaMapping(), WithListener(nil)); err == nil {
		t.Fatal("WithListener(nil) = nil error, want error")
	}
	l, err := pgkit.NewListener(pool)
	if err != nil {
		t.Fatal(err)
	}
	dq, err := NewDeltaQueue(pool, DefaultMapping(), DefaultDeltaMapping(), WithListener(l))
	if err != nil {
		t.Fatalf("NewDeltaQueue with a shared listener: %v", err)
	}
	if dq.hub.listener != l {
		t.Fatal("DeltaQueue did not use the listener it was given")
	}
	own, err := NewDeltaQueue(pool, DefaultMapping(), DefaultDeltaMapping())
	if err != nil {
		t.Fatal(err)
	}
	if own.hub.listener == nil || own.hub.listener == l {
		t.Fatal("a DeltaQueue without WithListener must build its own")
	}
}

func TestDeltaQueueCapability(t *testing.T) {
	pool := lazyPool(t)
	q, err := New(pool, DefaultMapping())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := any(q).(queue.DeltaLog); ok {
		t.Fatal("plain Queue advertises queue.DeltaLog; only DeltaQueue should")
	}
	dq, err := NewDeltaQueue(pool, DefaultMapping(), DefaultDeltaMapping())
	if err != nil {
		t.Fatalf("NewDeltaQueue: %v", err)
	}
	if _, ok := any(dq).(queue.DeltaLog); !ok {
		t.Fatal("DeltaQueue does not implement queue.DeltaLog")
	}
	if _, ok := any(dq).(queue.Queue); !ok {
		t.Fatal("DeltaQueue does not implement queue.Queue")
	}
}

func TestRecommendedDeltaDDLGolden(t *testing.T) {
	const want = `-- Delta log table for pgqueue. Suggested only: pgqueue never executes DDL.
CREATE TABLE IF NOT EXISTS "durable_delta" (
    "seq" bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    "run_id" text COLLATE "C" NOT NULL,
    "record_key" text NOT NULL,
    "attempt" bigint NOT NULL,
    "kind" text NOT NULL,
    "payload" bytea NOT NULL
);
CREATE INDEX IF NOT EXISTS "durable_delta_run_id_seq_idx" ON "durable_delta" ("run_id", "seq");
`
	if got := RecommendedDeltaDDL(DefaultDeltaMapping()); got != want {
		t.Fatalf("RecommendedDeltaDDL(DefaultDeltaMapping()) mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestCursorRoundTrip(t *testing.T) {
	if n, err := parseCursor(""); err != nil || n != 0 {
		t.Fatalf(`parseCursor("") = %d, %v; want 0, nil`, n, err)
	}
	if n, err := parseCursor(formatCursor(42)); err != nil || n != 42 {
		t.Fatalf("parseCursor(formatCursor(42)) = %d, %v; want 42, nil", n, err)
	}
	for _, bad := range []queue.Cursor{"abc", "-1", "1-0"} {
		if _, err := parseCursor(bad); err == nil {
			t.Errorf("parseCursor(%q) = nil error, want error", bad)
		}
	}
}
