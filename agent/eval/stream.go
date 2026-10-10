// Package eval provides agent-specific evaluation scorers and utilities.
package eval

import (
	"sort"
	"strings"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// StreamTiming holds latency metrics collected from a delta stream.
type StreamTiming struct {
	TTFTMs     int64   `json:"ttft_ms"`
	TTLTMs     int64   `json:"ttlt_ms"`
	MedianITL  float64 `json:"median_itl_ms"`
	ChunkCount int     `json:"chunk_count"`
	// InputTokens and OutputTokens sum the prompt and completion tokens of
	// each top-level UsageDelta, skipping response-cache replays.
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
	// Errors holds the message of each ErrorDelta seen on the stream. A
	// non-empty list means the run failed and the text may be truncated.
	Errors []string `json:"errors,omitempty"`
}

// Failed reports whether the stream carried an ErrorDelta.
func (st StreamTiming) Failed() bool { return len(st.Errors) > 0 }

// CollectStreamTiming drains a delta channel, collecting timing data and
// concatenating text content. Returns the collected timing, the full text
// output, and all deltas (for further inspection by scorers).
//
// The clock starts when CollectStreamTiming is called. When the stream was
// started earlier, use [CollectStreamTimingFrom] with the start time, or
// buffered deltas make TTFT and TTLT read low.
//
// For an agent run, agent.Collect also gives a per-delta callback, the tool
// calls, and the run's terminal error from Wait.
func CollectStreamTiming(ch <-chan types.Delta) (StreamTiming, string, []types.Delta) {
	return CollectStreamTimingFrom(time.Now(), ch)
}

// CollectStreamTimingFrom is [CollectStreamTiming] with TTFT and TTLT
// measured from start. Capture start immediately before invoking the agent.
func CollectStreamTimingFrom(start time.Time, ch <-chan types.Delta) (StreamTiming, string, []types.Delta) {
	var c streamCollector
	var allDeltas []types.Delta
	for delta := range ch {
		allDeltas = append(allDeltas, delta)
		c.observe(time.Now(), delta)
	}
	return c.timing(start), c.text.String(), allDeltas
}

// streamCollector accumulates text, token usage, inter-token gaps, and
// errors from top-level deltas.
type streamCollector struct {
	st           StreamTiming
	firstTokenAt time.Time
	lastTokenAt  time.Time
	prevChunkAt  time.Time
	itls         []time.Duration
	text         strings.Builder
}

func (c *streamCollector) observe(now time.Time, delta types.Delta) {
	switch v := delta.(type) {
	case types.PartDelta:
		if v.Text == "" {
			return
		}
		if c.firstTokenAt.IsZero() {
			c.firstTokenAt = now
		}
		if !c.prevChunkAt.IsZero() {
			c.itls = append(c.itls, now.Sub(c.prevChunkAt))
		}
		c.prevChunkAt = now
		c.lastTokenAt = now
		c.st.ChunkCount++
		c.text.WriteString(v.Text)

	case types.UsageDelta:
		// A response-cache replay carries the original counts but makes
		// no new request, so it adds no tokens, matching
		// [types.UsageFromDelta].
		if v.CacheHit {
			return
		}
		c.st.InputTokens += v.PromptTokens
		c.st.OutputTokens += v.CompletionTokens

	case types.ErrorDelta:
		msg := "unknown error"
		if v.Error != nil {
			msg = v.Error.Error()
		}
		c.st.Errors = append(c.st.Errors, msg)
	}
}

func (c *streamCollector) timing(start time.Time) StreamTiming {
	timing := c.st
	if !c.firstTokenAt.IsZero() {
		timing.TTFTMs = c.firstTokenAt.Sub(start).Milliseconds()
	}
	if !c.lastTokenAt.IsZero() {
		timing.TTLTMs = c.lastTokenAt.Sub(start).Milliseconds()
	}
	if len(c.itls) > 0 {
		itls := append([]time.Duration(nil), c.itls...)
		sort.Slice(itls, func(i, j int) bool { return itls[i] < itls[j] })
		mid := len(itls) / 2
		if len(itls)%2 == 0 {
			timing.MedianITL = float64(itls[mid-1]+itls[mid]) / 2.0 / float64(time.Millisecond)
		} else {
			timing.MedianITL = float64(itls[mid]) / float64(time.Millisecond)
		}
	}
	return timing
}
