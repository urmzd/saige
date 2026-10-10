package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/urmzd/saige/agent/types"
)

// OutputMode selects how a response schema reaches the model.
type OutputMode string

const (
	// OutputAuto picks a mode. For a schema set with WithResponseSchema it
	// means OutputNative, or OutputTool when the provider reports tool
	// calling but cannot constrain output now (for example a model that
	// rejects the forced tool its schema path needs). For Structured it
	// picks OutputTool when the agent
	// has tools, OutputNative when the provider can constrain output, then
	// OutputTool, and OutputPrompt last.
	OutputAuto OutputMode = ""
	// OutputNative sends the schema through the provider's structured output
	// support. With tools present it applies to a final tool-free turn.
	OutputNative OutputMode = "native"
	// OutputTool offers a final_answer tool whose parameters are the schema.
	// The run ends when the model calls it with valid arguments, so the
	// schema and the agent's tools travel in the same request.
	OutputTool OutputMode = "tool"
	// OutputPrompt describes the schema in the system prompt and relies on
	// extraction, validation, and repair. Use it for models with neither
	// structured output nor tool calling.
	OutputPrompt OutputMode = "prompt"
)

// FinalAnswerToolName is the tool OutputTool adds to every turn.
const FinalAnswerToolName = "final_answer"

// ErrNoJSON means ExtractJSON found no JSON value in the text.
var ErrNoJSON = errors.New("no JSON value found")

// WithOutputMode sets how WithResponseSchema's schema reaches the model.
func WithOutputMode(mode OutputMode) AgentOption {
	return func(c *AgentConfig) { c.OutputMode = mode }
}

// runOutput is the response schema in force for one run.
type runOutput struct {
	agent  *Agent
	schema *types.ParameterSchema
	mode   OutputMode // never OutputAuto once resolved
	// check adds the caller's typed validation to the final_answer tool, so
	// a semantic error goes back to the model inside the run.
	check func(json.RawMessage) error
	// scoped marks a schema set by Structured, which validates and repairs
	// the answer itself after the run.
	scoped bool
}

type runOutputKey struct{}

// withRunOutput scopes a schema to runs of agent a started with ctx. Child
// agents receive the same context through delegation, so the value names
// its agent and other agents ignore it.
func withRunOutput(ctx context.Context, out *runOutput) context.Context {
	return context.WithValue(ctx, runOutputKey{}, out)
}

// output returns the schema in force for a run of a under ctx: a scoped one
// set by Structured, or the configured ResponseSchema.
func (a *Agent) output(ctx context.Context) runOutput {
	if out, ok := ctx.Value(runOutputKey{}).(*runOutput); ok && out.agent == a {
		return *out
	}
	if a.cfg.ResponseSchema == nil {
		return runOutput{}
	}
	mode := a.cfg.OutputMode
	if mode == OutputAuto {
		mode = a.autoResponseMode(a.cfg.Provider, a.cfg.ResponseSchema)
	}
	return runOutput{agent: a, schema: a.cfg.ResponseSchema, mode: mode}
}

func (o runOutput) native() bool { return o.schema != nil && o.mode == OutputNative }
func (o runOutput) tool() bool   { return o.schema != nil && o.mode == OutputTool }
func (o runOutput) prompt() bool { return o.schema != nil && o.mode == OutputPrompt }

// maxOutputRepairs bounds how many times one user turn re-asks the model for
// a final text answer that does not match a configured schema.
const maxOutputRepairs = 2

// textAnswerError checks the text of a final turn that called no tools
// against a configured schema in tool or prompt mode. Native mode applies the
// schema in the request, and Structured checks its own answers, so both
// return nil. A text answer in tool mode is accepted when it already matches,
// as if final_answer had been called with it.
func (o runOutput) textAnswerError(msg *types.AssistantMessage) error {
	if o.scoped || (!o.tool() && !o.prompt()) {
		return nil
	}
	var text strings.Builder
	for _, block := range msg.Parts {
		if tc, ok := block.(types.TextPart); ok {
			text.WriteString(tc.Text)
		}
	}
	extracted, err := ExtractJSON(text.String())
	if err != nil {
		return err
	}
	var value any
	if err := json.Unmarshal([]byte(extracted), &value); err != nil {
		return fmt.Errorf("%w: %w", ErrNoJSON, err)
	}
	return types.ValidateJSON(*o.schema, value)
}

// checkOutput rejects an output configuration the run cannot honor, before
// any provider call.
func (a *Agent) checkOutput(provider types.Provider, out runOutput) error {
	if out.schema == nil {
		return nil
	}
	switch out.mode {
	case OutputNative:
		return checkStructuredOutput(provider)
	case OutputTool:
		return a.checkToolOutput(provider, out.schema)
	case OutputPrompt:
		return nil
	default:
		return fmt.Errorf("%w: unknown output mode %q", types.ErrInvalidModelConfig, out.mode)
	}
}

// checkToolOutput reports whether the final_answer tool can carry schema.
func (a *Agent) checkToolOutput(provider types.Provider, schema *types.ParameterSchema) error {
	if schema.Type != "" && schema.Type != "object" {
		return fmt.Errorf("%w: output mode %q needs an object schema, got %q", types.ErrInvalidModelConfig, OutputTool, schema.Type)
	}
	if mc, ok := types.ProviderCapabilities(provider); ok && mc.Known && !mc.Supports(types.CapTools) {
		return fmt.Errorf("%w: output mode %q: model %q declares no tool calling",
			types.ErrInvalidModelConfig, OutputTool, types.ProviderModel(provider))
	}
	if _, taken := a.tools.Get(FinalAnswerToolName); taken {
		return fmt.Errorf("%w: output mode %q: a tool named %q is already registered",
			types.ErrInvalidModelConfig, OutputTool, FinalAnswerToolName)
	}
	return nil
}

// resolveOutputMode turns OutputAuto into a concrete mode for Structured and
// checks an explicit one.
func (a *Agent) resolveOutputMode(mode OutputMode, schema *types.ParameterSchema) (OutputMode, error) {
	provider := a.cfg.Provider
	if mode != OutputAuto {
		return mode, a.checkOutput(provider, runOutput{schema: schema, mode: mode})
	}
	toolErr := a.checkToolOutput(provider, schema)
	if toolErr == nil && (len(a.tools.Definitions()) > 0 || a.handoffs != nil) {
		return OutputTool, nil
	}
	if nativeOutputUsable(provider) {
		return OutputNative, nil
	}
	if toolErr == nil {
		return OutputTool, nil
	}
	return OutputPrompt, nil
}

// autoResponseMode resolves OutputAuto for WithResponseSchema: native when
// the provider can constrain output, otherwise the final_answer tool when
// the provider reports tool calling and the schema fits a tool. The tool
// mode never forces a call, so it works where a forced tool choice is
// rejected. Anything else stays native, which fails before the first call.
func (a *Agent) autoResponseMode(provider types.Provider, schema *types.ParameterSchema) OutputMode {
	if nativeOutputUsable(provider) {
		return OutputNative
	}
	mc, ok := types.ProviderCapabilities(provider)
	if ok && mc.Supports(types.CapTools) && a.checkToolOutput(provider, schema) == nil {
		return OutputTool
	}
	return OutputNative
}

// nativeOutputUsable reports whether the provider, as configured now, can
// constrain output to a schema. Unlike checkStructuredOutput it trusts the
// reported capabilities even when the model is only inferred from a family
// prefix: an adapter reports what it will actually accept (an Anthropic
// adapter whose model thinks by default drops structured output), and auto
// mode must not pick a path the adapter will refuse at call time.
func nativeOutputUsable(provider types.Provider) bool {
	if checkStructuredOutput(provider) != nil {
		return false
	}
	mc, ok := types.ProviderCapabilities(provider)
	if !ok {
		return true
	}
	return mc.Supports(types.CapStructuredOutput) && mc.StructuredOutput != types.StructuredOutputNone
}

// withOutputTool adds the final_answer tool to one turn's tools.
func withOutputTool(active activeContext, out runOutput) activeContext {
	if !out.tool() {
		return active
	}
	reg := types.NewToolRegistry(active.tools.All()...)
	reg.Register(&finalAnswerTool{schema: *out.schema, check: out.check})
	active.tools, active.toolDefs = reg, reg.Definitions()
	return active
}

// withSchemaInstruction appends the schema to the system prompt for one
// request. The tree is not changed.
func withSchemaInstruction(messages []types.Message, out runOutput) []types.Message {
	if !out.prompt() {
		return messages
	}
	return overlaySystem(messages, schemaInstruction(out.schema))
}

func schemaInstruction(schema *types.ParameterSchema) string {
	raw, _ := json.Marshal(schema)
	return "Answer with only a JSON value that matches this JSON Schema. Do not add prose or code fences.\n" + string(raw)
}

// finalAnswerTool carries a structured answer as tool arguments. Its result
// is the canonical JSON of a valid answer; an invalid one is returned to the
// model as a tool error so it can correct the call.
type finalAnswerTool struct {
	schema types.ParameterSchema
	check  func(json.RawMessage) error
}

func (t *finalAnswerTool) Definition() types.ToolDef {
	return types.ToolDef{
		Name:        FinalAnswerToolName,
		Description: "Submit the final answer. Call this once, when the task is complete, with arguments that match the schema. Do not answer in plain text.",
		Parameters:  t.schema,
		Capability:  types.ToolCapabilityRead,
	}
}

func (t *finalAnswerTool) Execute(_ context.Context, args map[string]any) (string, error) {
	if err := types.ValidateJSON(t.schema, args); err != nil {
		return "", err
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return "", err
	}
	if t.check != nil {
		if err := t.check(raw); err != nil {
			return "", err
		}
	}
	return string(raw), nil
}

// ── Tolerant extraction ──────────────────────────────────────────────

var (
	thinkBlock = regexp.MustCompile(`(?is)<think(?:ing)?>.*?</think(?:ing)?>`)
	thinkClose = regexp.MustCompile(`(?is)^.*</think(?:ing)?>`)
	codeFence  = regexp.MustCompile("(?s)```[A-Za-z0-9_-]*[ \t]*\r?\n(.*?)```")
)

// ExtractJSON returns the JSON value inside a model's text answer. Models
// asked for JSON often wrap it: in <think> reasoning, in a Markdown code
// fence, or in prose before and after. ExtractJSON removes reasoning blocks
// (and anything before a lone closing tag), then tries, in order, the whole
// text, each fenced block, and each balanced object or array in the text.
// The first candidate that is valid JSON wins. It returns ErrNoJSON when none
// is.
func ExtractJSON(text string) (string, error) {
	text = thinkBlock.ReplaceAllString(text, "")
	text = thinkClose.ReplaceAllString(text, "")
	text = strings.TrimSpace(text)
	if text != "" && json.Valid([]byte(text)) {
		return text, nil
	}
	for _, m := range codeFence.FindAllStringSubmatch(text, -1) {
		if inner := strings.TrimSpace(m[1]); inner != "" && json.Valid([]byte(inner)) {
			return inner, nil
		}
	}
	for i := 0; i < len(text); i++ {
		if text[i] != '{' && text[i] != '[' {
			continue
		}
		end := balancedEnd(text, i)
		if end < 0 {
			continue
		}
		if candidate := text[i:end]; json.Valid([]byte(candidate)) {
			return candidate, nil
		}
	}
	return "", ErrNoJSON
}

// balancedEnd returns the index just past the bracket that closes the one at
// start, skipping brackets inside strings, or -1 when it is never closed.
func balancedEnd(s string, start int) int {
	depth, inString, escaped := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			escaped = false
		case inString:
			switch c {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
		case c == '"':
			inString = true
		case c == '{' || c == '[':
			depth++
		case c == '}' || c == ']':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}

// ── Partial JSON ─────────────────────────────────────────────────────

// completeJSON closes a JSON prefix so it parses: an open string is closed,
// then every open array and object. When the prefix ends inside a key or
// mid-literal, it falls back to the last point where a value was complete.
// ok is false when no prefix of s parses.
func completeJSON(s string) (string, bool) {
	var (
		stack    []byte
		inString bool
		escaped  bool
		// safe holds cut points where everything before them is a complete
		// sequence of values, with the closers open at that point.
		safe []safeCut
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			escaped = false
		case inString:
			switch c {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
		case c == '"':
			inString = true
		case c == '{':
			stack = append(stack, '}')
			safe = append(safe, safeCut{i + 1, closers(stack)})
		case c == '[':
			stack = append(stack, ']')
			safe = append(safe, safeCut{i + 1, closers(stack)})
		case c == '}' || c == ']':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			safe = append(safe, safeCut{i + 1, closers(stack)})
		case c == ',':
			safe = append(safe, safeCut{i, closers(stack)})
		}
	}
	full := s
	if inString {
		if escaped {
			full = full[:len(full)-1]
		}
		full += `"`
	}
	full += closers(stack)
	if json.Valid([]byte(full)) {
		return full, true
	}
	for i := len(safe) - 1; i >= 0; i-- {
		candidate := s[:safe[i].at] + safe[i].closers
		if json.Valid([]byte(candidate)) {
			return candidate, true
		}
	}
	return "", false
}

type safeCut struct {
	at      int
	closers string
}

func closers(stack []byte) string {
	out := make([]byte, len(stack))
	for i, c := range stack {
		out[len(stack)-1-i] = c
	}
	return string(out)
}

// partialJSON turns one turn's streamed structured output into
// PartialJSONDeltas: the text of a schema-constrained or prompted turn, or
// the arguments of a final_answer call.
type partialJSON struct {
	text  bool // track text deltas
	tool  bool // track final_answer argument deltas
	index int  // the part index of the final_answer call being streamed
	open  bool // a final_answer call is being streamed
	buf   strings.Builder
	last  string
}

func newPartialJSON(out runOutput, tools []types.ToolDef) *partialJSON {
	switch {
	case out.tool():
		return &partialJSON{tool: true}
	case out.prompt(), out.native() && len(tools) == 0:
		return &partialJSON{text: true}
	}
	return nil
}

// push records d and returns a delta when the parsed prefix changed.
func (p *partialJSON) push(d types.Delta) (types.PartialJSONDelta, bool) {
	if p == nil {
		return types.PartialJSONDelta{}, false
	}
	switch v := d.(type) {
	case types.PartStart:
		if p.tool && v.Kind == types.KindToolCall && v.Name == FinalAnswerToolName {
			p.open, p.index = true, v.Index
			p.buf.Reset()
			p.last = ""
		}
		return types.PartialJSONDelta{}, false
	case types.PartDelta:
		switch {
		case p.text && v.Text != "":
			p.buf.WriteString(v.Text)
		case p.tool && p.open && v.Index == p.index && v.Args != "":
			p.buf.WriteString(v.Args)
		default:
			return types.PartialJSONDelta{}, false
		}
	default:
		return types.PartialJSONDelta{}, false
	}
	return p.emit()
}

func (p *partialJSON) emit() (types.PartialJSONDelta, bool) {
	text := p.buf.String()
	if p.text {
		// Prose or a fence may precede the value, and a closing fence may
		// follow it.
		start := strings.IndexAny(text, "{[")
		if start < 0 {
			return types.PartialJSONDelta{}, false
		}
		text = text[start:]
		if end := strings.Index(text, "```"); end >= 0 {
			text = text[:end]
		}
	}
	done, ok := completeJSON(strings.TrimSpace(text))
	if !ok || done == p.last {
		return types.PartialJSONDelta{}, false
	}
	p.last = done
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(done)); err != nil {
		return types.PartialJSONDelta{}, false
	}
	return types.PartialJSONDelta{JSON: json.RawMessage(compact.Bytes())}, true
}
