package otel

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// recordEvent captures one Record call on a spy histogram.
type recordEvent struct {
	instrument string
	operation  string // gen_ai.operation.name attribute, if present
}

// spyHistogram records the operation.name attribute for each Record call onto a
// shared event log, tagged with the instrument name it was created under.
type spyHistogram struct {
	noop.Float64Histogram
	instrument string
	log        *[]recordEvent
}

func (h spyHistogram) Record(_ context.Context, _ float64, opts ...metric.RecordOption) {
	attrs := metric.NewRecordConfig(opts).Attributes()
	op, _ := attrs.Value(attribute.Key("gen_ai.operation.name"))
	*h.log = append(*h.log, recordEvent{instrument: h.instrument, operation: op.AsString()})
}

type spyInt64Histogram struct {
	noop.Int64Histogram
	instrument string
	log        *[]recordEvent
}

func (h spyInt64Histogram) Record(_ context.Context, _ int64, opts ...metric.RecordOption) {
	attrs := metric.NewRecordConfig(opts).Attributes()
	op, _ := attrs.Value(attribute.Key("gen_ai.operation.name"))
	*h.log = append(*h.log, recordEvent{instrument: h.instrument, operation: op.AsString()})
}

// spyMeter tracks every instrument name requested and hands back spy
// histograms that funnel Record calls into a shared log.
type spyMeter struct {
	noop.Meter
	created *[]string
	log     *[]recordEvent
}

func (m spyMeter) Float64Histogram(name string, _ ...metric.Float64HistogramOption) (metric.Float64Histogram, error) {
	*m.created = append(*m.created, name)
	return spyHistogram{instrument: name, log: m.log}, nil
}

func (m spyMeter) Int64Histogram(name string, _ ...metric.Int64HistogramOption) (metric.Int64Histogram, error) {
	*m.created = append(*m.created, name)
	return spyInt64Histogram{instrument: name, log: m.log}, nil
}

// TestMetrics_CollapsedDurationInstrument verifies that the three duration
// signals (chat / execute_tool / invoke_agent) collapse to a SINGLE
// gen_ai.client.operation.duration instrument, keyed by operation.name, rather
// than registering the same name multiple times.
func TestMetrics_CollapsedDurationInstrument(t *testing.T) {
	var created []string
	var log []recordEvent
	meter := spyMeter{created: &created, log: &log}

	m, err := NewMetrics(meter)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	// Each duration instrument name should be created exactly once.
	counts := map[string]int{}
	for _, name := range created {
		counts[name]++
	}
	if got := counts["gen_ai.client.operation.duration"]; got != 1 {
		t.Errorf("operation.duration instrument created %d times, want 1 (collapsed)", got)
	}
	if got := counts["gen_ai.client.token.usage"]; got != 1 {
		t.Errorf("token.usage instrument created %d times, want 1", got)
	}

	ctx := context.Background()
	m.RecordProviderCall(ctx, "chat", "ollama", time.Second, nil)
	m.RecordToolCall(ctx, "calc", time.Second, errors.New("boom"))
	m.RecordAgentInvocation(ctx, "agent-1", time.Second)

	// All three duration records must land on the one collapsed instrument,
	// disambiguated by operation.name.
	gotOps := map[string]string{} // operation.name -> instrument
	for _, ev := range log {
		if ev.instrument == "gen_ai.client.operation.duration" {
			gotOps[ev.operation] = ev.instrument
		}
	}
	for _, op := range []string{"chat", "execute_tool", "invoke_agent"} {
		if gotOps[op] != "gen_ai.client.operation.duration" {
			t.Errorf("operation %q did not record on the collapsed duration instrument (got %q)", op, gotOps[op])
		}
	}
}

// TestMetrics_RecordTokenUsage verifies token usage records input and output
// counts on the token.usage instrument under the chat operation.
func TestMetrics_RecordTokenUsage(t *testing.T) {
	var created []string
	var log []recordEvent
	meter := spyMeter{created: &created, log: &log}

	m, err := NewMetrics(meter)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	m.RecordTokenUsage(context.Background(), "chat", "ollama", 100, 42)

	// Two records: one for input tokens, one for output tokens. Both carry the
	// chat operation.name and land on the token.usage instrument.
	var tokenRecords int
	for _, ev := range log {
		if ev.instrument == "gen_ai.client.token.usage" {
			tokenRecords++
			if ev.operation != "chat" {
				t.Errorf("token record operation = %q, want %q", ev.operation, "chat")
			}
		}
	}
	if tokenRecords != 2 {
		t.Errorf("token.usage Record calls = %d, want 2 (input + output)", tokenRecords)
	}
}
