package convert

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/urmzd/saige/agent/types"
)

// Default prompts of the model converters. They are part of each
// converter's version, so changing one never serves a stale result.
const (
	TranscribePrompt = "Transcribe the speech in this audio verbatim. Reply with the transcript only. If there is no speech, reply with [no speech]."
	DescribePrompt   = "Describe this image for someone who cannot see it: the subject, any text it contains (verbatim), and the details a question about it might need. Reply with the description only."
)

// Default output caps of the model converters.
const (
	DefaultTranscribeTokens = 2048
	DefaultDescribeTokens   = 512
)

// ModelOption configures Transcribe or Describe.
type ModelOption func(*modelConverter)

// WithPrompt replaces the converter's instruction.
func WithPrompt(prompt string) ModelOption {
	return func(c *modelConverter) { c.prompt = prompt }
}

// WithMaxOutputTokens caps the converter's reply. It bounds the estimate.
func WithMaxOutputTokens(n int64) ModelOption {
	return func(c *modelConverter) { c.maxOut = n }
}

// modelConverter fits a part to a target by asking another model, one that
// takes the part natively, to turn it into text.
type modelConverter struct {
	action   types.ModalityAction
	name     string
	provider types.Provider
	prompt   string
	maxOut   int64
	accepts  []types.Modality
	label    string
}

// Transcribe returns a converter that sends audio to p, which must take it
// natively (for example a Gemini model), and replaces the part with the
// transcript. The call runs with temperature 0 where the model accepts one,
// and its result is memoized, so a clip is transcribed once however often
// the history is sent again.
func Transcribe(p types.Provider, opts ...ModelOption) types.Converter {
	c := &modelConverter{action: types.ActTranscribe, name: "transcribe", provider: p, prompt: TranscribePrompt,
		maxOut: DefaultTranscribeTokens, accepts: []types.Modality{types.ModalityAudio}, label: "transcript"}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Describe returns a converter that sends an image (or a video) to p, a
// model that takes it natively, and replaces the part with the description.
// Like Transcribe, it runs at temperature 0 and is memoized.
func Describe(p types.Provider, opts ...ModelOption) types.Converter {
	c := &modelConverter{action: types.ActDescribe, name: "describe", provider: p, prompt: DescribePrompt,
		maxOut: DefaultDescribeTokens, accepts: []types.Modality{types.ModalityImage, types.ModalityVideo}, label: "description"}
	for _, o := range opts {
		o(c)
	}
	return c
}

func (c *modelConverter) Name() string                 { return c.name }
func (c *modelConverter) Action() types.ModalityAction { return c.action }
func (c *modelConverter) Produces(types.Part) []types.Modality {
	return []types.Modality{types.ModalityText}
}

// Version names the model and a digest of the prompt and output cap, so a
// change to any of them is a different converter.
func (c *modelConverter) Version() string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%d", c.prompt, c.maxOut))
	return "1." + types.ProviderModel(c.provider) + "." + hex.EncodeToString(sum[:4])
}

// Pricing implements Priced with the converter model's rate card.
func (c *modelConverter) Pricing() types.Pricing {
	caps, _ := types.ProviderCapabilities(c.provider)
	return caps.Pricing
}

// Accepts reports whether the part has a modality the converter handles and
// the converter's model takes it natively.
func (c *modelConverter) Accepts(p types.Part) bool {
	m, ok := types.PartModality(p)
	if !ok || !containsModality(c.accepts, m) {
		return false
	}
	if _, user := p.(types.UserPart); !user {
		return false
	}
	off, ok := Target(c.provider)
	if !ok {
		return false
	}
	src, _ := types.SourceOf(p)
	pr := planner{target: off, counts: map[types.Modality]int{}}
	reason, _, _ := pr.native(p, src, m, false)
	return reason == ""
}

func containsModality(ms []types.Modality, m types.Modality) bool {
	for _, x := range ms {
		if x == m {
			return true
		}
	}
	return false
}

// Estimate bounds the call: the media's input tokens by the converter
// model's token rule (or a conservative default), the prompt, and the
// output cap, priced at the converter model's rates.
func (c *modelConverter) Estimate(p types.Part, _ types.Offering) (types.ConversionEstimate, error) {
	in := len(c.prompt)/3 + 16 + c.mediaTokens(p)
	out := int(c.maxOut)
	cost := c.Pricing().Cost(types.TokenUsage{InputTokens: in, OutputTokens: out, Requests: 1})
	return types.ConversionEstimate{InputTokens: in, OutputTokens: out, Cost: cost}, nil
}

// Conservative media token counts when the converter model declares none.
const (
	defaultImageTokens  = 1600
	audioTokensPerSec   = 32
	videoTokensPerSec   = 300
	assumedBytesPerSec  = 16000 // 128 kbps: an upper bound on seconds for compressed audio
	minEstimatedSeconds = 1
)

func (c *modelConverter) mediaTokens(p types.Part) int {
	var rule types.TokenRule
	m, _ := types.PartModality(p)
	if off, ok := Target(c.provider); ok {
		rule = off.Modalities.In[m].Tokens
	}
	src, _ := types.SourceOf(p)
	size := src.Size
	if size == 0 {
		size = int64(len(src.Inline))
	}
	seconds := func(d float64) int {
		s := int(d)
		if s <= 0 {
			s = int(size / assumedBytesPerSec)
		}
		return max(s, minEstimatedSeconds) + 1
	}
	switch v := p.(type) {
	case types.AudioPart:
		per := rule.PerSecond
		if per == 0 {
			per = audioTokensPerSec
		}
		return seconds(v.Duration.Seconds()) * per
	case types.VideoPart:
		per := rule.PerSecond
		if per == 0 {
			per = videoTokensPerSec
		}
		return seconds(v.Duration.Seconds()) * per
	default:
		if n := rule.PerImage + rule.Base; n > 0 {
			return n
		}
		return defaultImageTokens
	}
}

// Convert asks the converter model for the text and returns it as one
// text part, labelled so the serving model knows what it stands for.
func (c *modelConverter) Convert(ctx context.Context, p types.Part, _ types.ConvertEnv) ([]types.Part, types.ConversionUsage, error) {
	up, ok := p.(types.UserPart)
	if !ok {
		return nil, types.ConversionUsage{}, fmt.Errorf("%s: %s part cannot be sent to a model", c.name, p.Kind())
	}
	req := types.Request{Messages: []types.Message{types.UserMsg(types.Text(c.prompt), up)}}
	if opts := c.options(); opts != nil && types.AcceptsOptions(c.provider) {
		req.Options = opts
	}
	usage := types.ConversionUsage{Model: types.ProviderModel(c.provider), Pricing: c.Pricing()}
	ch, err := c.provider.Stream(ctx, req)
	if err != nil {
		return nil, usage, err
	}
	asm := types.NewPartAssembler()
	var streamErr error
	for d := range ch {
		switch v := d.(type) {
		case types.UsageDelta:
			usage.Usage = usage.Usage.Merge(v)
		case types.ErrorDelta:
			streamErr = v.Error
			if streamErr == nil {
				streamErr = errors.New("converter model reported an empty error")
			}
		default:
			asm.Push(d)
		}
	}
	if streamErr != nil {
		return nil, usage, streamErr
	}
	parts, _, _ := asm.Flush()
	var text strings.Builder
	for _, part := range parts {
		if t, ok := part.(types.TextPart); ok {
			text.WriteString(t.Text)
		}
	}
	body := strings.TrimSpace(text.String())
	if body == "" {
		return nil, usage, fmt.Errorf("%s: the model returned no text", c.name)
	}
	return []types.Part{types.TextPart{Text: fmt.Sprintf("[%s of %s]\n%s", c.label, describePart(p), body)}}, usage, nil
}

// options asks for deterministic, bounded output where the model takes
// those controls.
func (c *modelConverter) options() *types.RequestOptions {
	caps, ok := types.ProviderCapabilities(c.provider)
	if !ok {
		return nil
	}
	var o types.RequestOptions
	if caps.Supports(types.CapTemperature) {
		zero := 0.0
		o.Temperature = &zero
	}
	if caps.Supports(types.CapMaxOutputTokens) && c.maxOut > 0 {
		n := c.maxOut
		o.MaxOutputTokens = &n
	}
	if o.Temperature != nil && caps.ValidateOptions(o) != nil {
		// Sampling is refused while the model reasons: keep the cap only.
		o.Temperature = nil
	}
	if o.Temperature == nil && o.MaxOutputTokens == nil {
		return nil
	}
	return &o
}
