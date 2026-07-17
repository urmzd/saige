package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
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

// Turn is one prompt in an experiment. Index 0 is the synthesis turn; later
// indices are edit instructions.
type Turn struct {
	Index  int
	Prompt string
}

// Experiment is one multi-turn eval case. Systems maps arbitrary system
// prompt names to their content; the built-in flows read Systems["base"].
// Dir is the experiment directory where flows write outputs/ and the runner
// writes the metrics document.
type Experiment struct {
	ID      string
	Format  string
	Dir     string
	Systems map[string]string
	Turns   []Turn
}

// FlowContext carries results across the flows of one experiment. The runner
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

// Flow drives one experiment through the model with a particular strategy.
type Flow interface {
	Name() string
	Run(ctx context.Context, c *Client, exp Experiment, fc *FlowContext) (FlowResult, error)
}

// FlowResult is the outcome of one flow over one experiment. Turn0 covers
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
// and user edit instructions. Outputs land in <exp.Dir>/outputs/base.
type BaseFlow struct{}

// Name returns "base".
func (BaseFlow) Name() string { return baseFlowName }

// Run executes the base flow.
func (BaseFlow) Run(ctx context.Context, c *Client, exp Experiment, fc *FlowContext) (FlowResult, error) {
	if len(exp.Turns) == 0 {
		return FlowResult{}, fmt.Errorf("experiment %s has no turns", exp.ID)
	}
	outDir := filepath.Join(exp.Dir, "outputs", baseFlowName)
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return FlowResult{}, err
	}
	system := exp.Systems["base"]
	ext := FormatExt(exp.Format)
	start := time.Now()
	result, err := c.Chat(ctx, []Message{
		{Role: roleSystem, Content: system},
		{Role: roleUser, Content: exp.Turns[0].Prompt},
	})
	if err != nil {
		return FlowResult{}, err
	}
	artifact := CleanArtifact(result.Text)
	if err := WriteText(filepath.Join(outDir, "turn-0"+ext), artifact); err != nil {
		return FlowResult{}, err
	}
	t0 := newTurnMetrics(result, start, artifact)

	messages := []Message{
		{Role: roleSystem, Content: system},
		{Role: roleUser, Content: exp.Turns[0].Prompt},
		{Role: roleAssistant, Content: artifact},
	}
	var turns []TurnResult
	for _, turn := range exp.Turns[1:] {
		messages = append(messages, Message{Role: roleUser, Content: turn.Prompt})
		start := time.Now()
		result, err := c.Chat(ctx, messages)
		if err != nil {
			return FlowResult{Turn0: t0, Turns: turns, Artifact: artifact}, err
		}
		artifact = CleanArtifact(result.Text)
		if err := WriteText(filepath.Join(outDir, fmt.Sprintf("turn-%d%s", turn.Index, ext)), artifact); err != nil {
			return FlowResult{Turn0: t0, Turns: turns, Artifact: artifact}, err
		}
		messages = append(messages, Message{Role: roleAssistant, Content: artifact})
		turns = append(turns, newTurnResult(turn, result, start, artifact))
	}
	return FlowResult{Turn0: t0, Turns: turns, Artifact: artifact}, nil
}

// StatelessFlow re-sends only the current artifact plus the edit instruction
// each turn, with no conversation history. When the base flow ran earlier in
// the same experiment, its artifact and turn-0 metrics seed this flow
// instead of a fresh synthesis call. Outputs land in
// <exp.Dir>/outputs/stateless.
type StatelessFlow struct{}

// Name returns "stateless".
func (StatelessFlow) Name() string { return statelessFlowName }

// Run executes the stateless flow.
func (StatelessFlow) Run(ctx context.Context, c *Client, exp Experiment, fc *FlowContext) (FlowResult, error) {
	if len(exp.Turns) == 0 {
		return FlowResult{}, fmt.Errorf("experiment %s has no turns", exp.ID)
	}
	outDir := filepath.Join(exp.Dir, "outputs", statelessFlowName)
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return FlowResult{}, err
	}
	system := exp.Systems["base"]
	ext := FormatExt(exp.Format)

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
			{Role: roleUser, Content: exp.Turns[0].Prompt},
		})
		if err != nil {
			return FlowResult{}, err
		}
		artifact = CleanArtifact(result.Text)
		t0 = newTurnMetrics(result, start, artifact)
	}
	if err := WriteText(filepath.Join(outDir, "turn-0"+ext), artifact); err != nil {
		return FlowResult{}, err
	}

	var turns []TurnResult
	for _, turn := range exp.Turns[1:] {
		user := fmt.Sprintf("## Current Artifact\n\n```\n%s\n```\n\n## Edit Instruction\n\n%s\n\nReturn the complete updated artifact, raw, with no commentary.", artifact, turn.Prompt)
		start := time.Now()
		result, err := c.Chat(ctx, []Message{
			{Role: roleSystem, Content: system},
			{Role: roleUser, Content: user},
		})
		if err != nil {
			return FlowResult{Turn0: t0, Turns: turns, Artifact: artifact}, err
		}
		artifact = CleanArtifact(result.Text)
		if err := WriteText(filepath.Join(outDir, fmt.Sprintf("turn-%d%s", turn.Index, ext)), artifact); err != nil {
			return FlowResult{Turn0: t0, Turns: turns, Artifact: artifact}, err
		}
		turns = append(turns, newTurnResult(turn, result, start, artifact))
	}
	return FlowResult{Turn0: t0, Turns: turns, Artifact: artifact}, nil
}

// newTurnMetrics builds TurnMetrics from a chat result.
func newTurnMetrics(result ChatResult, start time.Time, artifact string) TurnMetrics {
	return TurnMetrics{
		InputTokens:       result.InputTokens,
		OutputTokens:      result.OutputTokens,
		CachedInputTokens: result.CachedInputTokens,
		LatencyMS:         elapsedMS(start),
		ArtifactBytes:     len(artifact),
	}
}

// newTurnResult builds a TurnResult for one edit turn.
func newTurnResult(turn Turn, result ChatResult, start time.Time, artifact string) TurnResult {
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

// elapsedMS returns the elapsed wall time since start in milliseconds.
func elapsedMS(start time.Time) uint64 {
	ms := time.Since(start).Milliseconds()
	if ms < 0 {
		return 0
	}
	return uint64(ms)
}
