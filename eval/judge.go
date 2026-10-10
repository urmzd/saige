package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/urmzd/saige/agent/types"
)

// Generator is the minimal LLM interface for evaluation prompts.
// It is intentionally identical to rag/types.LLM so any provider satisfies both.
type Generator interface {
	Generate(ctx context.Context, prompt string) (string, error)
}

// StructuredGenerator is a [Generator] that can constrain its reply to a JSON
// Schema, such as a provider's structured output mode. Judge scorers use it
// when the generator implements it, passing [JudgeSchema], so the verdict is
// a JSON object instead of free text.
type StructuredGenerator interface {
	Generator
	GenerateStructured(ctx context.Context, prompt string, schema json.RawMessage) (string, error)
}

// JudgeSchema is the JSON Schema of a judge verdict: a reasoning string and
// a score from 0 to 1.
var JudgeSchema = json.RawMessage(`{"type":"object","properties":{"reasoning":{"type":"string"},"score":{"type":"number","minimum":0,"maximum":1}},"required":["reasoning","score"],"additionalProperties":false}`)

// PartsGenerator is a [Generator] that can send media with a prompt. A
// judge scorer whose observation input carries media (see [DecodeInput])
// needs one: the prompt is sent as the first text part and the input's media
// parts follow it. The generator decides how media the judge model cannot
// take natively is handled, through its conversion policy; agent/eval's
// Generator rejects such media unless its policy permits a conversion.
type PartsGenerator interface {
	Generator
	// GenerateParts sends parts as one user message. A non-nil schema
	// constrains the reply as [StructuredGenerator.GenerateStructured]
	// does, when the generator supports it.
	GenerateParts(ctx context.Context, parts []types.UserPart, schema json.RawMessage) (string, error)
}

// ErrJudgeMedia reports an observation whose input carries media for a
// judge whose generator cannot send it. The judge declines to score the
// text alone, which would grade an answer about a picture it never saw.
var ErrJudgeMedia = errors.New("judge generator cannot send the input's media")

// generateVerdict asks gen for a judge verdict, constrained to [JudgeSchema]
// when gen supports structured output. With media, the prompt goes first
// and the media follow it in the same message.
func generateVerdict(ctx context.Context, gen Generator, prompt string, media []types.UserPart) (string, error) {
	if len(media) > 0 {
		pg, ok := gen.(PartsGenerator)
		if !ok {
			return "", fmt.Errorf("%w: %T", ErrJudgeMedia, gen)
		}
		parts := append([]types.UserPart{types.Text(prompt)}, media...)
		return pg.GenerateParts(ctx, parts, JudgeSchema)
	}
	if sg, ok := gen.(StructuredGenerator); ok {
		return sg.GenerateStructured(ctx, prompt, JudgeSchema)
	}
	return gen.Generate(ctx, prompt)
}

// judgeInput is what a judge prompt shows of an observation's input, and
// the media sent with it. A text or subject-defined input is shown as its
// raw JSON, as it always was. A parts input is shown as its text, with one
// line per media part naming it, and the media parts, resolved through
// resolvers, follow the prompt.
func judgeInput(ctx context.Context, raw json.RawMessage, resolvers map[string]types.Resolver) (string, []types.UserPart, error) {
	in, err := DecodeInput(raw)
	if err != nil {
		return "", nil, err
	}
	if in.Kind != InputParts {
		return string(raw), nil, nil
	}
	media := in.Media()
	text := in.Text()
	if len(media) == 0 {
		return text, nil, nil
	}
	var b strings.Builder
	b.WriteString(text)
	for i, m := range media {
		src, _ := types.SourceOf(m)
		fmt.Fprintf(&b, "\n[attachment %d: %s %s, sent after these instructions]", i+1, m.Kind(), src.MediaType)
	}
	if media, err = ResolveInput(ctx, media, resolvers); err != nil {
		return "", nil, err
	}
	return b.String(), media, nil
}

// ErrNoJudgeScore is returned when a judge reply has no parseable SCORE line,
// for example a refusal, a truncated reply, or a score that is not a number.
// The scorer reports it as an errored [Score], which [Aggregate] excludes,
// instead of counting the reply as 0.0.
var ErrNoJudgeScore = errors.New("judge reply has no parseable SCORE")

// JudgeConfig configures a judge scorer.
type JudgeConfig struct {
	Name   string
	Rubric string
	// PositionSwap asks the pairwise judge twice, once with the responses
	// in each order, and reports the mean. It is on by default for
	// [NewPairwiseJudgeScorer] and ignored by [NewJudgeScorer].
	PositionSwap bool
	// Resolvers fetch the bytes of input media referenced by uri, keyed by
	// scheme, before they are sent to the judge. See [WithJudgeResolvers].
	Resolvers map[string]types.Resolver
}

// JudgeOption configures a judge scorer.
type JudgeOption func(*JudgeConfig)

// WithJudgeName sets the metric name (default: "judge_score").
func WithJudgeName(name string) JudgeOption {
	return func(c *JudgeConfig) { c.Name = name }
}

// WithJudgeRubric sets the evaluation criteria rubric.
func WithJudgeRubric(rubric string) JudgeOption {
	return func(c *JudgeConfig) { c.Rubric = rubric }
}

// WithPositionSwap turns the pairwise judge's order swap on or off. LLM
// judges tend to favor one position regardless of content; asking twice with
// the responses swapped cancels that offset at the cost of a second judge
// call per observation. Pass false to make one call per observation.
func WithPositionSwap(on bool) JudgeOption {
	return func(c *JudgeConfig) { c.PositionSwap = on }
}

// WithJudgeResolvers sets the resolvers that fetch input media referenced
// by uri, keyed by scheme, such as {"file": DirResolver(datasetDir)}. Media
// with a scheme that has no resolver is sent with its uri, for the judge's
// provider to fetch or its conversion policy to handle.
func WithJudgeResolvers(resolvers map[string]types.Resolver) JudgeOption {
	return func(c *JudgeConfig) { c.Resolvers = resolvers }
}

// NewJudgeScorer creates a [Scorer] that uses an LLM to evaluate output quality.
// It reads Input, Output, and optionally a context annotation from the Observation.
// When the input is in the parts form and carries media, the media are sent
// to the judge with the prompt; that needs a [PartsGenerator], and any
// other generator fails the score with [ErrJudgeMedia].
//
// A reply without a parseable SCORE line is an error wrapping
// [ErrNoJudgeScore], so it is excluded from aggregates instead of scored 0.
func NewJudgeScorer(gen Generator, opts ...JudgeOption) Scorer {
	cfg := &JudgeConfig{
		Name:   "judge_score",
		Rubric: "Evaluate the response for correctness, completeness, and relevance.",
	}
	for _, o := range opts {
		o(cfg)
	}

	return NewScorerFunc(cfg.Name, func(ctx context.Context, obs Observation) (Score, error) {
		contextText := ""
		if raw, ok := obs.Annotations["context"]; ok {
			contextText = string(raw)
		}

		input, media, err := judgeInput(ctx, obs.Input, cfg.Resolvers)
		if err != nil {
			return Score{}, err
		}
		prompt, err := renderPrompt(judgeTmpl, map[string]string{
			"Input":    input,
			"Context":  contextText,
			"Response": string(obs.Output),
			"Rubric":   cfg.Rubric,
		})
		if err != nil {
			return Score{}, err
		}

		result, err := generateVerdict(ctx, gen, prompt, media)
		if err != nil {
			return Score{}, fmt.Errorf("judge generate: %w", err)
		}

		score, reason, err := parseJudgeOutput(result)
		if err != nil {
			return Score{}, err
		}
		return Score{Name: cfg.Name, Value: score, Reason: reason}, nil
	})
}

// NewPairwiseJudgeScorer creates a [Scorer] for comparing two outputs.
// It expects the base output in GroundTruth and the experimental output in
// Output. The score is how much better the experimental output is: 0.0 means
// the base is clearly better, 0.5 equal, 1.0 the experimental output.
//
// By default the judge is asked twice, with the base shown first and then
// second, which doubles judge calls. The second verdict is mapped to 1-s and
// the two are averaged. Both ordered verdicts are kept in [Score.Samples];
// when they fall on opposite sides of 0.5 the judge contradicted itself,
// Samples.Stable is false, and the reason is marked position-inconsistent.
// Under a [Sampler] each sample makes both calls, and a score with any
// position-inconsistent sample is reported unstable.
// Pass WithPositionSwap(false) for a single call in the fixed order.
func NewPairwiseJudgeScorer(gen Generator, opts ...JudgeOption) Scorer {
	cfg := &JudgeConfig{
		Name:         "pairwise_judge",
		Rubric:       "Compare the two responses for correctness, completeness, and relevance.",
		PositionSwap: true,
	}
	for _, o := range opts {
		o(cfg)
	}

	judge := func(ctx context.Context, input string, media []types.UserPart, a, b string) (float64, string, error) {
		prompt, err := renderPrompt(judgePairwiseTmpl, map[string]string{
			"Input":     input,
			"ResponseA": a,
			"ResponseB": b,
			"Rubric":    cfg.Rubric,
		})
		if err != nil {
			return 0, "", err
		}
		result, err := generateVerdict(ctx, gen, prompt, media)
		if err != nil {
			return 0, "", fmt.Errorf("pairwise judge generate: %w", err)
		}
		return parseJudgeOutput(result)
	}

	return NewScorerFunc(cfg.Name, func(ctx context.Context, obs Observation) (Score, error) {
		input, media, err := judgeInput(ctx, obs.Input, cfg.Resolvers)
		if err != nil {
			return Score{}, err
		}
		base := string(obs.GroundTruth)
		exp := string(obs.Output)

		forward, forwardReason, err := judge(ctx, input, media, base, exp)
		if err != nil {
			return Score{}, err
		}
		if !cfg.PositionSwap {
			return Score{Name: cfg.Name, Value: forward, Reason: forwardReason}, nil
		}

		swapped, swappedReason, err := judge(ctx, input, media, exp, base)
		if err != nil {
			return Score{}, fmt.Errorf("swapped order: %w", err)
		}
		mirrored := 1 - swapped

		stats := &SampleStats{
			Values:  []float64{forward, mirrored},
			Reasons: []string{forwardReason, swappedReason},
		}
		stats.summarize(math.Inf(1))
		stats.Stable = (forward-0.5)*(mirrored-0.5) >= 0

		reason := forwardReason
		if !stats.Stable {
			reason = fmt.Sprintf("position-inconsistent: %.3g with base first, %.3g with base second", forward, mirrored)
		}
		return Score{Name: cfg.Name, Value: stats.Mean, Reason: reason, Samples: stats}, nil
	})
}

// parseJudgeOutput extracts the score and reasoning from a judge reply. The
// verdict is read first as a JSON object with "score" and "reasoning" keys
// (see [JudgeSchema]), which may be wrapped in a code fence or surrounded by
// prose. Replies without such an object fall back to "SCORE:" and
// "REASONING:" lines. A reply with no parseable score, or a non-finite one,
// returns an error wrapping [ErrNoJudgeScore]. Scores are clamped to [0, 1].
func parseJudgeOutput(output string) (float64, string, error) {
	if score, reason, ok := parseJudgeJSON(output); ok {
		return score, reason, nil
	}
	return parseJudgeLines(output)
}

// maxJudgeJSONCandidates bounds how many "{" positions are tried as the start
// of a verdict object, so a long reply full of braces stays cheap.
const maxJudgeJSONCandidates = 32

// parseJudgeJSON reads the first JSON object in output that carries a
// parseable score. The score may be a number or a string such as "0.8" or
// "8/10".
func parseJudgeJSON(output string) (float64, string, bool) {
	text := stripCodeFence(output)
	tried := 0
	for i := 0; i < len(text) && tried < maxJudgeJSONCandidates; i++ {
		if text[i] != '{' {
			continue
		}
		tried++
		var obj map[string]any
		if err := json.NewDecoder(strings.NewReader(text[i:])).Decode(&obj); err != nil {
			continue
		}
		var (
			score  float64
			found  bool
			reason string
		)
		for k, v := range obj {
			switch strings.ToLower(k) {
			case "score":
				switch val := v.(type) {
				case float64:
					score, found = val, true
				case string:
					score, found = parseJudgeScore(val)
				}
			case "reasoning", "reason":
				if s, ok := v.(string); ok {
					reason = s
				}
			}
		}
		if found {
			return clamp(score, 0, 1), reason, true
		}
	}
	return 0, "", false
}

// parseJudgeLines extracts SCORE and REASONING lines. It accepts markdown
// emphasis around the label ("**SCORE:** 0.8"), "=" as the separator, and a
// fraction ("8/10"). When several SCORE lines parse, the last one wins.
func parseJudgeLines(output string) (float64, string, error) {
	var (
		score  float64
		found  bool
		reason string
	)
	for _, line := range strings.Split(output, "\n") {
		label, value, ok := splitJudgeLine(line)
		if !ok {
			continue
		}
		switch label {
		case "SCORE":
			if v, ok := parseJudgeScore(value); ok {
				score, found = clamp(v, 0, 1), true
			}
		case "REASONING", "REASON":
			reason = value
		}
	}
	if !found {
		return 0, reason, fmt.Errorf("%w: %q", ErrNoJudgeScore, excerpt(output, 200))
	}
	return score, reason, nil
}

// splitJudgeLine splits "LABEL: value" or "LABEL = value", ignoring list
// markers, headings, and markdown emphasis around the label. The label is
// returned upper-cased.
func splitJudgeLine(line string) (label, value string, ok bool) {
	line = strings.TrimLeft(strings.TrimSpace(line), "#->`*_ \t")
	idx := strings.IndexAny(line, ":=")
	if idx <= 0 {
		return "", "", false
	}
	label = strings.ToUpper(strings.Trim(line[:idx], "*_` \t"))
	value = strings.Trim(line[idx+1:], "*_` \t")
	return label, value, true
}

// parseJudgeScore parses a score value such as "0.8", "0.8.", "8/10", or
// "0.8 (good)". It rejects NaN and infinities.
func parseJudgeScore(value string) (float64, bool) {
	if fields := strings.Fields(value); len(fields) > 0 {
		value = fields[0]
	}
	value = strings.TrimRight(value, ".,;*_`")
	var v float64
	if num, den, isFrac := strings.Cut(value, "/"); isFrac {
		n, err1 := strconv.ParseFloat(num, 64)
		d, err2 := strconv.ParseFloat(den, 64)
		if err1 != nil || err2 != nil || d <= 0 {
			return 0, false
		}
		v = n / d
	} else {
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return 0, false
		}
		v = parsed
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

// excerpt shortens s to at most n bytes on a rune boundary for error text.
func excerpt(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
