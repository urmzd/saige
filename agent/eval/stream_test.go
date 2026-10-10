package eval

import (
	"errors"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/types"
)

func TestCollectStreamTimingBasic(t *testing.T) {
	ch := make(chan types.Delta, 10)

	// Simulate a stream with text chunks.
	go func() {
		ch <- types.PartStart{Index: 0, Kind: types.KindText}
		ch <- types.PartDelta{Index: 0, Text: "Hello"}
		time.Sleep(5 * time.Millisecond)
		ch <- types.PartDelta{Index: 0, Text: " world"}
		ch <- types.UsageDelta{PromptTokens: 10, CompletionTokens: 5}
		ch <- types.PartEnd{Index: 0}
		ch <- types.DoneDelta{}
		close(ch)
	}()

	timing, text, deltas := CollectStreamTiming(ch)

	if text != "Hello world" {
		t.Errorf("expected 'Hello world', got %q", text)
	}
	if timing.ChunkCount != 2 {
		t.Errorf("expected 2 chunks, got %d", timing.ChunkCount)
	}
	if timing.InputTokens != 10 {
		t.Errorf("expected 10 input tokens, got %d", timing.InputTokens)
	}
	if timing.OutputTokens != 5 {
		t.Errorf("expected 5 output tokens, got %d", timing.OutputTokens)
	}
	if timing.TTFTMs < 0 {
		t.Error("TTFT should be non-negative")
	}
	if timing.TTLTMs < timing.TTFTMs {
		t.Error("TTLT should be >= TTFT")
	}
	if len(deltas) != 6 {
		t.Errorf("expected 6 deltas, got %d", len(deltas))
	}
}

func TestCollectStreamTimingEmpty(t *testing.T) {
	ch := make(chan types.Delta)
	close(ch)

	timing, text, deltas := CollectStreamTiming(ch)

	if text != "" {
		t.Errorf("expected empty text, got %q", text)
	}
	if timing.ChunkCount != 0 {
		t.Errorf("expected 0 chunks, got %d", timing.ChunkCount)
	}
	if timing.TTFTMs != 0 {
		t.Errorf("expected 0 TTFT, got %d", timing.TTFTMs)
	}
	if len(deltas) != 0 {
		t.Errorf("expected 0 deltas, got %d", len(deltas))
	}
}

func TestCollectStreamTimingMultipleUsage(t *testing.T) {
	ch := make(chan types.Delta, 5)

	go func() {
		ch <- types.UsageDelta{PromptTokens: 10, CompletionTokens: 5}
		ch <- types.UsageDelta{PromptTokens: 20, CompletionTokens: 10}
		close(ch)
	}()

	timing, _, _ := CollectStreamTiming(ch)

	// Usage should accumulate.
	if timing.InputTokens != 30 {
		t.Errorf("expected 30 input tokens, got %d", timing.InputTokens)
	}
	if timing.OutputTokens != 15 {
		t.Errorf("expected 15 output tokens, got %d", timing.OutputTokens)
	}
}

func TestCollectStreamTimingFrom(t *testing.T) {
	tests := []struct {
		name    string
		delay   time.Duration
		wantMin int64
		errs    int
	}{
		{name: "clock starts before collection", delay: 50 * time.Millisecond, wantMin: 50},
		{name: "error delta is flagged", errs: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch := make(chan types.Delta, 4)
			start := time.Now()
			time.Sleep(tt.delay)
			ch <- types.PartDelta{Index: 0, Text: "x"}
			for range tt.errs {
				ch <- types.ErrorDelta{Error: errors.New("boom")}
			}
			close(ch)

			timing, _, _ := CollectStreamTimingFrom(start, ch)
			if timing.TTFTMs < tt.wantMin {
				t.Errorf("TTFTMs %d, want >= %d", timing.TTFTMs, tt.wantMin)
			}
			if len(timing.Errors) != tt.errs || timing.Failed() != (tt.errs > 0) {
				t.Errorf("Errors: got %v, want %d", timing.Errors, tt.errs)
			}
		})
	}
}
