package otel

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/urmzd/saige/agent/types"
)

// recordEvent captures one Record call on a spy histogram.
type recordEvent struct {
	instrument string
	operation  string // gen_ai.operation.name attribute, if present
	tokenType  string // gen_ai.token.type attribute, if present
	errorType  string // error.type attribute, if present
	value      float64
}

func newRecordEvent(instrument string, value float64, opts []metric.RecordOption) recordEvent {
	attrs := metric.NewRecordConfig(opts).Attributes()
	op, _ := attrs.Value(attribute.Key("gen_ai.operation.name"))
	tt, _ := attrs.Value(attribute.Key("gen_ai.token.type"))
	et, _ := attrs.Value(attribute.Key("error.type"))
	return recordEvent{instrument: instrument, operation: op.AsString(), tokenType: tt.AsString(), errorType: et.AsString(), value: value}
}

// spyHistogram records the operation.name attribute for each Record call onto a
// shared event log, tagged with the instrument name it was created under.
type spyHistogram struct {
	noop.Float64Histogram
	instrument string
	log        *[]recordEvent
}

func (h spyHistogram) Record(_ context.Context, v float64, opts ...metric.RecordOption) {
	*h.log = append(*h.log, newRecordEvent(h.instrument, v, opts))
}

type spyInt64Histogram struct {
	noop.Int64Histogram
	instrument string
	log        *[]recordEvent
}

func (h spyInt64Histogram) Record(_ context.Context, v int64, opts ...metric.RecordOption) {
	*h.log = append(*h.log, newRecordEvent(h.instrument, float64(v), opts))
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

func TestMetrics_OptionalRecorders(t *testing.T) {
	tests := []struct {
		name   string
		record func(m *Metrics)
		want   []recordEvent
	}{
		{
			name:   "agent outcome success has no error type",
			record: func(m *Metrics) { m.RecordAgentOutcome(context.Background(), "a", time.Second, nil) },
			want:   []recordEvent{{instrument: "gen_ai.client.operation.duration", operation: "invoke_agent", value: 1}},
		},
		{
			name: "agent outcome failure carries error type",
			record: func(m *Metrics) {
				m.RecordAgentOutcome(context.Background(), "a", time.Second, &types.ProviderError{Kind: types.ErrorKindRateLimit, Err: errors.New("x")})
			},
			want: []recordEvent{{instrument: "gen_ai.client.operation.duration", operation: "invoke_agent", errorType: "rate_limit", value: 1}},
		},
		{
			name:   "cache tokens skip zero counts",
			record: func(m *Metrics) { m.RecordCacheTokenUsage(context.Background(), "chat", "p", 50, 0) },
			want:   []recordEvent{{instrument: "gen_ai.client.token.usage", operation: "chat", tokenType: "cache_read", value: 50}},
		},
		{
			name:   "time to first chunk",
			record: func(m *Metrics) { m.RecordTimeToFirstChunk(context.Background(), "chat", "p", 250*time.Millisecond) },
			want:   []recordEvent{{instrument: "gen_ai.client.operation.time_to_first_chunk", operation: "chat", value: 0.25}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var log []recordEvent
			m, err := NewMetrics(spyMeter{created: new([]string), log: &log})
			if err != nil {
				t.Fatal(err)
			}
			tt.record(m)
			if fmt.Sprint(log) != fmt.Sprint(tt.want) {
				t.Errorf("records = %+v, want %+v", log, tt.want)
			}
		})
	}
}
