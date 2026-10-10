package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/urmzd/saige/eval"
)

// Chat message role names.
const (
	roleSystem    = "system"
	roleUser      = "user"
	roleAssistant = "assistant"
)

// Built-in flow names.
const (
	baseFlowName      = "base"
	statelessFlowName = "stateless"
)

// Turn is one prompt in a script. Index 0 is the synthesis turn; later
// indices are edit instructions.
type Turn struct {
	Index  int
	Prompt string
}

// Script is one scripted multi-turn eval case: a fixed sequence of turns
// asked in one session. Systems maps arbitrary system prompt names to their
// content; the built-in flows read Systems["base"]. Dir is the script
// directory where flows write outputs/ and the runner writes the metrics
// document. Each [Flow] is one way of driving a script, so the flows of a
// runner play the role of variants.
type Script struct {
	ID      string
	Format  string
	Dir     string
	Systems map[string]string
	Turns   []Turn
}

// FlowContext carries results across the flows of one script. The runner
// records each flow's final artifact and turn-0 metrics under the flow name,
// so later flows can seed from earlier ones ([StatelessFlow] seeds from
// "base" when present).
type FlowContext struct {
	Artifacts map[string]string
	Turn0     map[string]*TurnMetrics
}

// NewFlowContext returns an empty FlowContext with initialized maps.
func NewFlowContext() *FlowContext {
	return &FlowContext{
		Artifacts: map[string]string{},
		Turn0:     map[string]*TurnMetrics{},
	}
}

// Flow drives one script through the model with a particular strategy.
type Flow interface {
	Name() string
	Run(ctx context.Context, c *Client, script Script, fc *FlowContext) (FlowResult, error)
}

// FlowResult is the outcome of one flow over one script. Turn0 covers
// the synthesis turn; Turns cover the edit turns. Artifact is the final
// artifact, kept for seeding later flows and for inspection. Extra holds
// flow-level custom metrics (for example parse rates) that the default
// metrics assembly flattens into the flow's JSON object.
type FlowResult struct {
	Turn0    TurnMetrics
	Turns    []TurnResult
	Artifact string
	Extra    map[string]any
}

// BaseFlow regenerates the full artifact each turn inside one growing
// conversation: system, turn-0 prompt, then alternating assistant artifacts
// and user edit instructions. Outputs land in <script.Dir>/outputs/base.
//
// A failed request on an edit turn is recorded as a failed [TurnResult]
// (FailureReason "request failed: ...") and the flow continues from the last
// good artifact. A failed turn-0 request fails the flow, since there is no
// artifact to edit; the runner then reports the script as failed.
type BaseFlow struct{}

// Name returns "base".
func (BaseFlow) Name() string { return baseFlowName }

// Run executes the base flow.
func (BaseFlow) Run(ctx context.Context, c *Client, script Script, fc *FlowContext) (FlowResult, error) {
	if len(script.Turns) == 0 {
		return FlowResult{}, fmt.Errorf("script %s has no turns", script.ID)
	}
	outDir := filepath.Join(script.Dir, "outputs", baseFlowName)
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return FlowResult{}, err
	}
	system := script.Systems["base"]
	ext := FormatExt(script.Format)
	start := time.Now()
	result, err := c.Chat(ctx, []Message{
		{Role: roleSystem, Content: system},
		{Role: roleUser, Content: script.Turns[0].Prompt},
	})
	if err != nil {
		return FlowResult{}, err
	}
	artifact := CleanArtifact(result.Text)
	if err := WriteText(filepath.Join(outDir, "turn-0"+ext), artifact); err != nil {
		return FlowResult{}, err
	}
	t0 := NewTurnMetrics(result, start, artifact)

	messages := []Message{
		{Role: roleSystem, Content: system},
		{Role: roleUser, Content: script.Turns[0].Prompt},
		{Role: roleAssistant, Content: artifact},
	}
	var turns []TurnResult
	for _, turn := range script.Turns[1:] {
		start := time.Now()
		result, err := c.Chat(ctx, append(messages, Message{Role: roleUser, Content: turn.Prompt}))
		if err != nil {
			if ctx.Err() != nil {
				return FlowResult{Turn0: t0, Turns: turns, Artifact: artifact}, err
			}
			// The failed exchange is left out of the conversation, so the
			// next turn edits the last good artifact.
			turns = append(turns, NewFailedTurnResult(turn, start, err))
			continue
		}
		messages = append(messages, Message{Role: roleUser, Content: turn.Prompt})
		artifact = CleanArtifact(result.Text)
		if err := WriteText(filepath.Join(outDir, fmt.Sprintf("turn-%d%s", turn.Index, ext)), artifact); err != nil {
			return FlowResult{Turn0: t0, Turns: turns, Artifact: artifact}, err
		}
		messages = append(messages, Message{Role: roleAssistant, Content: artifact})
		turns = append(turns, NewTurnResult(turn, result, start, artifact))
	}
	return FlowResult{Turn0: t0, Turns: turns, Artifact: artifact}, nil
}

// StatelessFlow re-sends only the current artifact plus the edit instruction
// each turn, with no conversation history. When the base flow ran earlier in
// the same script, its artifact and turn-0 metrics seed this flow
// instead of a fresh synthesis call. Outputs land in
// <script.Dir>/outputs/stateless. Request failures are handled as in [BaseFlow].
type StatelessFlow struct{}

// Name returns "stateless".
func (StatelessFlow) Name() string { return statelessFlowName }

// Run executes the stateless flow.
func (StatelessFlow) Run(ctx context.Context, c *Client, script Script, fc *FlowContext) (FlowResult, error) {
	if len(script.Turns) == 0 {
		return FlowResult{}, fmt.Errorf("script %s has no turns", script.ID)
	}
	outDir := filepath.Join(script.Dir, "outputs", statelessFlowName)
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return FlowResult{}, err
	}
	system := script.Systems["base"]
	ext := FormatExt(script.Format)

	var seedArtifact string
	var seedMetrics *TurnMetrics
	if fc != nil {
		seedArtifact = fc.Artifacts[baseFlowName]
		seedMetrics = fc.Turn0[baseFlowName]
	}

	var artifact string
	var t0 TurnMetrics
	if seedArtifact != "" && seedMetrics != nil {
		artifact = seedArtifact
		t0 = *seedMetrics
	} else {
		start := time.Now()
		result, err := c.Chat(ctx, []Message{
			{Role: roleSystem, Content: system},
			{Role: roleUser, Content: script.Turns[0].Prompt},
		})
		if err != nil {
			return FlowResult{}, err
		}
		artifact = CleanArtifact(result.Text)
		t0 = NewTurnMetrics(result, start, artifact)
	}
	if err := WriteText(filepath.Join(outDir, "turn-0"+ext), artifact); err != nil {
		return FlowResult{}, err
	}

	var turns []TurnResult
	for _, turn := range script.Turns[1:] {
		user := fmt.Sprintf("## Current Artifact\n\n```\n%s\n```\n\n## Edit Instruction\n\n%s\n\nReturn the complete updated artifact, raw, with no commentary.", artifact, turn.Prompt)
		start := time.Now()
		result, err := c.Chat(ctx, []Message{
			{Role: roleSystem, Content: system},
			{Role: roleUser, Content: user},
		})
		if err != nil {
			if ctx.Err() != nil {
				return FlowResult{Turn0: t0, Turns: turns, Artifact: artifact}, err
			}
			// Keep the last good artifact for the next edit.
			turns = append(turns, NewFailedTurnResult(turn, start, err))
			continue
		}
		artifact = CleanArtifact(result.Text)
		if err := WriteText(filepath.Join(outDir, fmt.Sprintf("turn-%d%s", turn.Index, ext)), artifact); err != nil {
			return FlowResult{Turn0: t0, Turns: turns, Artifact: artifact}, err
		}
		turns = append(turns, NewTurnResult(turn, result, start, artifact))
	}
	return FlowResult{Turn0: t0, Turns: turns, Artifact: artifact}, nil
}

// NewTurnMetrics builds the TurnMetrics of a synthesis turn from its chat
// result, the time the turn started, and the artifact it produced. Custom
// flows use it so their turn-0 metrics match the built-in flows.
func NewTurnMetrics(result ChatResult, start time.Time, artifact string) TurnMetrics {
	return TurnMetrics{
		InputTokens:       result.InputTokens,
		OutputTokens:      result.OutputTokens,
		CachedInputTokens: result.CachedInputTokens,
		LatencyMS:         elapsedMS(start),
		ArtifactBytes:     len(artifact),
	}
}

// NewTurnResult builds the TurnResult of a successful edit turn from its chat
// result, the time the turn started, and the artifact it produced. Edit is
// the turn prompt cut to 80 bytes. For a turn that took several calls, sum
// them with [ChatResult.Add] first.
func NewTurnResult(turn Turn, result ChatResult, start time.Time, artifact string) TurnResult {
	retried := result.Retried
	return TurnResult{
		Turn:              turn.Index,
		Edit:              Truncate(turn.Prompt, 80),
		InputTokens:       result.InputTokens,
		OutputTokens:      result.OutputTokens,
		CachedInputTokens: result.CachedInputTokens,
		LatencyMS:         elapsedMS(start),
		OutputBytes:       len(artifact),
		Retried:           &retried,
		Failed:            false,
	}
}

// requestFailedPrefix starts the FailureReason of a turn whose chat request
// failed; [ComputeReliability] counts such turns as request failures.
const requestFailedPrefix = "request failed"

// NewFailedTurnResult records an edit turn whose chat request failed. Its
// FailureReason starts with "request failed", which [ComputeReliability]
// counts as a request failure. It is Inconclusive when [eval.IsInfra]
// reports err.
func NewFailedTurnResult(turn Turn, start time.Time, err error) TurnResult {
	reason := requestFailedPrefix + ": " + Truncate(err.Error(), maxErrorBody)
	return TurnResult{
		Turn:          turn.Index,
		Edit:          Truncate(turn.Prompt, 80),
		LatencyMS:     elapsedMS(start),
		Failed:        true,
		FailureReason: &reason,
		Inconclusive:  eval.IsInfra(err),
	}
}

// elapsedMS returns the elapsed wall time since start in milliseconds.
func elapsedMS(start time.Time) uint64 {
	ms := time.Since(start).Milliseconds()
	if ms < 0 {
		return 0
	}
	return uint64(ms)
}
