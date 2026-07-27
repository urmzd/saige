package types

import "sort"

// Capability names one thing a model can do, or one request knob it accepts.
//
// The two are deliberately in one namespace because callers ask the same
// question of both: "may I set this / may I rely on this?" A model that emits
// reasoning content declares CapReasoning; whether the caller may size that
// reasoning is a separate declaration (CapReasoningBudget or
// CapReasoningEffort), because the two differ per family: Anthropic takes a
// token budget, OpenAI's reasoning models take an effort enum, Ollama takes a
// bare on/off toggle, and Gemini takes either a budget or a level.
//
// Absence means unsupported. A capability that is merely unknown is still
// absent: see ModelCapabilities.Known for telling the two apart.
type Capability string

const (
	// ── Features ────────────────────────────────────────────────────

	// CapTools: the model can be given tool/function definitions and call them.
	CapTools Capability = "tools"
	// CapParallelTools: the model can emit more than one tool call per turn.
	CapParallelTools Capability = "parallel_tools"
	// CapToolChoice: the caller can force or forbid a specific tool.
	CapToolChoice Capability = "tool_choice"
	// CapStructuredOutput: output can be constrained to a JSON schema. Whether
	// that is native or emulated is StructuredOutputMode.
	CapStructuredOutput Capability = "structured_output"
	// CapStreaming: responses can be streamed incrementally.
	CapStreaming Capability = "streaming"
	// CapSystemPrompt: a system instruction is accepted separately from the turns.
	CapSystemPrompt Capability = "system_prompt"
	// CapPromptCaching: repeated prompt prefixes can be cached provider-side.
	CapPromptCaching Capability = "prompt_caching"
	// CapReasoning: the model produces reasoning/thinking content.
	CapReasoning Capability = "reasoning"
	// CapReasoningSignature: reasoning blocks carry an opaque signature that must
	// be echoed back on the next turn. Anthropic and Gemini 3 both require this;
	// dropping it corrupts multi-turn tool use.
	CapReasoningSignature Capability = "reasoning_signature"
	// CapEmbeddings: the model produces embedding vectors.
	CapEmbeddings Capability = "embeddings"
	// CapParallelToolControl: the caller can turn parallel tool calling on or
	// off. Distinct from CapParallelTools, which says only that the model can
	// emit more than one call: a model may do so without letting you stop it,
	// which matters when the tools are not safe to run concurrently.
	CapParallelToolControl Capability = "parallel_tool_control"

	// ── Server-executed tools ───────────────────────────────────────
	//
	// These run inside the provider, not this process: no local execution, no
	// ToolGate, no durable step. See ServerTool.

	// CapServerTools: the provider executes at least one tool class itself.
	CapServerTools Capability = "server_tools"
	// CapWebSearch: provider-native web search and grounding.
	CapWebSearch Capability = "web_search"
	// CapCodeExecution: the provider runs generated code in its own sandbox.
	CapCodeExecution Capability = "code_execution"
	// CapRemoteMCP: the provider connects to remote MCP servers on the caller's
	// behalf. Supporting this is not the same as this SDK connecting to those
	// servers locally, which every provider supports because it is just tools.
	CapRemoteMCP Capability = "remote_mcp"
	// CapCitations: the provider returns structured citation metadata rather
	// than leaving attribution to the prompt. Without it, "cite your sources"
	// is a request the model may ignore; with it, attribution is data.
	CapCitations Capability = "citations"

	// ── Request knobs ───────────────────────────────────────────────

	// CapReasoningBudget: reasoning depth is set as a token budget.
	CapReasoningBudget Capability = "reasoning_budget"
	// CapReasoningEffort: reasoning depth is set as an enum (see ReasoningEfforts).
	CapReasoningEffort Capability = "reasoning_effort"
	// CapReasoningToggle: reasoning is only switchable on or off.
	CapReasoningToggle Capability = "reasoning_toggle"
	// CapTemperature: sampling temperature is accepted. Notably absent on
	// OpenAI's reasoning models, which reject it outright.
	CapTemperature Capability = "temperature"
	// CapTopP: nucleus sampling is accepted.
	CapTopP Capability = "top_p"
	// CapTopK: top-k sampling is accepted.
	CapTopK Capability = "top_k"
	// CapSeed: a sampling seed is accepted.
	CapSeed Capability = "seed"
	// CapStopSequences: caller-supplied stop sequences are accepted.
	CapStopSequences Capability = "stop_sequences"
	// CapMaxOutputTokens: an output token cap is accepted.
	CapMaxOutputTokens Capability = "max_output_tokens"
	// CapFrequencyPenalty / CapPresencePenalty: OpenAI-style repetition penalties.
	CapFrequencyPenalty Capability = "frequency_penalty"
	CapPresencePenalty  Capability = "presence_penalty"
	// CapSafetySettings: per-request content safety thresholds are accepted.
	CapSafetySettings Capability = "safety_settings"
	// CapContextWindowOverride: the context window is a caller-set request
	// parameter rather than a fixed model property (ollama's num_ctx).
	CapContextWindowOverride Capability = "context_window_override"
)

// StructuredOutputMode records how schema-constrained output is achieved,
// because the two are not interchangeable in failure behaviour: a native mode
// is enforced by the provider, an emulated one is a prompt-and-tool convention
// this SDK layers on top and can still return off-schema text.
type StructuredOutputMode string

const (
	// StructuredOutputNone: no schema constraint available.
	StructuredOutputNone StructuredOutputMode = ""
	// StructuredOutputNative: the provider enforces the schema (OpenAI strict
	// json_schema, Gemini responseSchema, ollama format).
	StructuredOutputNative StructuredOutputMode = "native"
	// StructuredOutputToolCall: emulated by forcing a hidden tool call whose
	// input schema is the response schema (Anthropic).
	StructuredOutputToolCall StructuredOutputMode = "tool_call"
)

// ModelCapabilities is the declared capability surface of one (provider, model)
// pair: the flags it accepts, the features it supports, and its hard limits.
//
// It is the single answer to "can this model do X", replacing the scattered
// optional-interface probes (StructuredOutputProvider, ContentNegotiator) that
// can only say what an *adapter* implements, never what the *model behind it*
// accepts. An adapter can implement ChatStreamWithSchema and still be pointed
// at a model that ignores schemas.
//
// A zero value is the honest "nothing is known and nothing may be assumed":
// Known is false and every Supports call returns false.
type ModelCapabilities struct {
	// Provider is the adapter name ("anthropic", "openai", "google", "ollama").
	Provider string
	// Model is the model identifier the capabilities were resolved for.
	Model string
	// Family is the catalog entry that matched, e.g. "claude-sonnet-4". Empty
	// when the model fell through to a provider baseline.
	Family string

	// Caps holds every supported capability. Absent key == unsupported.
	Caps map[Capability]bool

	// ContextWindow is the input token limit. 0 means undeclared, not unlimited.
	ContextWindow int
	// MaxOutputTokens is the largest output cap the model accepts. 0 means
	// undeclared.
	MaxOutputTokens int
	// DefaultMaxOutputTokens is what this SDK sends when the caller sets no cap
	// and the provider requires one (Anthropic). 0 means the provider default.
	DefaultMaxOutputTokens int

	// ReasoningEfforts enumerates the legal values when CapReasoningEffort is
	// set, e.g. ["low","medium","high"] or Gemini's thinking levels.
	ReasoningEfforts []string
	// MinReasoningBudget is the smallest legal token budget when
	// CapReasoningBudget is set (Anthropic rejects anything below 1024).
	MinReasoningBudget int

	// StructuredOutput records how schema constraint is achieved.
	StructuredOutput StructuredOutputMode

	// Pricing is the model's rate card. The zero value means unpriced, which a
	// Budget treats as unenforceable rather than free.
	Pricing Pricing

	// ServerTools enumerates the server-executed tool kinds the model accepts.
	// Redundant with the Cap* flags by design: the flags answer "may I", this
	// answers "which", and a CLI listing wants the second.
	ServerTools []ServerToolKind

	// Media declares which media types reach the model natively. This is the
	// model-level counterpart of ContentNegotiator, which only describes the
	// adapter.
	Media ContentSupport

	// Known is false when these capabilities are a conservative provider
	// baseline for an unrecognised model rather than a catalog entry. Callers
	// that want to fail closed on unknown models check this.
	Known bool

	// Notes carries caveats worth surfacing in logs, e.g. that a knob is
	// accepted but silently ignored.
	Notes []string
}

// Supports reports whether c is declared supported.
func (mc ModelCapabilities) Supports(c Capability) bool { return mc.Caps[c] }

// SupportsAll reports whether every capability in want is declared.
func (mc ModelCapabilities) SupportsAll(want ...Capability) bool {
	return len(mc.Missing(want...)) == 0
}

// Missing returns the subset of want that is not declared, in the order given.
// It is the shape callers want for an error message: "model X is missing
// [reasoning_budget]".
func (mc ModelCapabilities) Missing(want ...Capability) []Capability {
	var out []Capability
	for _, c := range want {
		if !mc.Caps[c] {
			out = append(out, c)
		}
	}
	return out
}

// List returns every declared capability, sorted, for logging and diffing.
func (mc ModelCapabilities) List() []Capability {
	out := make([]Capability, 0, len(mc.Caps))
	for c, ok := range mc.Caps {
		if ok {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// With returns a copy with the given capabilities added. It never mutates the
// receiver, so catalog entries can be shared safely.
func (mc ModelCapabilities) With(caps ...Capability) ModelCapabilities {
	out := mc.clone()
	for _, c := range caps {
		out.Caps[c] = true
	}
	return out
}

// Without returns a copy with the given capabilities removed.
func (mc ModelCapabilities) Without(caps ...Capability) ModelCapabilities {
	out := mc.clone()
	for _, c := range caps {
		delete(out.Caps, c)
	}
	return out
}

// ForModel returns a copy retargeted at model. Used by adapters whose WithModel
// produces a variant that must report the new model's capabilities.
func (mc ModelCapabilities) ForModel(model string) ModelCapabilities {
	out := mc.clone()
	out.Model = model
	return out
}

func (mc ModelCapabilities) clone() ModelCapabilities {
	out := mc
	out.Caps = make(map[Capability]bool, len(mc.Caps))
	for c, ok := range mc.Caps {
		if ok {
			out.Caps[c] = true
		}
	}
	out.ReasoningEfforts = append([]string(nil), mc.ReasoningEfforts...)
	out.ServerTools = append([]ServerToolKind(nil), mc.ServerTools...)
	out.Notes = append([]string(nil), mc.Notes...)
	out.Media = ContentSupport{NativeTypes: map[MediaType]bool{}}
	for mt, ok := range mc.Media.NativeTypes {
		if ok {
			out.Media.NativeTypes[mt] = true
		}
	}
	return out
}

// Intersect returns the capabilities a caller may rely on when either mc or
// other might serve the request. This is what a fallback chain must report:
// promising a capability only the first provider has is exactly the bug that
// surfaces at 3am, when the first provider is down and the schema silently
// stops being enforced.
//
// Set membership intersects; declared limits take the smaller non-zero value;
// Known is true only if both are known.
func (mc ModelCapabilities) Intersect(other ModelCapabilities) ModelCapabilities {
	out := ModelCapabilities{
		Provider:               mc.Provider,
		Model:                  mc.Model,
		Family:                 mc.Family,
		Caps:                   map[Capability]bool{},
		ContextWindow:          minNonZero(mc.ContextWindow, other.ContextWindow),
		MaxOutputTokens:        minNonZero(mc.MaxOutputTokens, other.MaxOutputTokens),
		DefaultMaxOutputTokens: minNonZero(mc.DefaultMaxOutputTokens, other.DefaultMaxOutputTokens),
		MinReasoningBudget:     maxInt(mc.MinReasoningBudget, other.MinReasoningBudget),
		Known:                  mc.Known && other.Known,
		Media:                  ContentSupport{NativeTypes: map[MediaType]bool{}},
	}
	if mc.Provider != other.Provider {
		out.Provider = mc.Provider + "+" + other.Provider
		out.Family = ""
	}
	for c := range mc.Caps {
		if mc.Caps[c] && other.Caps[c] {
			out.Caps[c] = true
		}
	}
	for mt := range mc.Media.NativeTypes {
		if mc.Media.NativeTypes[mt] && other.Media.NativeTypes[mt] {
			out.Media.NativeTypes[mt] = true
		}
	}
	// A schema is only enforced as weakly as the weakest link: native plus
	// emulated is emulated, and anything plus none is none.
	out.StructuredOutput = weakerStructuredOutput(mc.StructuredOutput, other.StructuredOutput)
	if out.StructuredOutput == StructuredOutputNone {
		delete(out.Caps, CapStructuredOutput)
	}
	// Effort enums only carry over when both sides accept the same vocabulary.
	if equalStrings(mc.ReasoningEfforts, other.ReasoningEfforts) {
		out.ReasoningEfforts = append([]string(nil), mc.ReasoningEfforts...)
	} else {
		delete(out.Caps, CapReasoningEffort)
	}
	// Pricing takes the WORSE of the two, not the intersection. Any member may
	// serve the request, so a budget built on the cheaper member's rates would
	// under-count exactly when the expensive fallback is in use.
	out.Pricing = worsePricing(mc.Pricing, other.Pricing)

	// Server tools intersect for the same reason capabilities do: a chain that
	// advertises web search the secondary cannot run produces an ungrounded
	// answer the moment it falls through.
	for _, k := range mc.ServerTools {
		for _, o := range other.ServerTools {
			if k == o {
				out.ServerTools = append(out.ServerTools, k)
				break
			}
		}
	}
	out.Notes = append(append([]string(nil), mc.Notes...), other.Notes...)
	return out
}

// worsePricing returns the more expensive rate for each line item, so a
// fallback chain is costed against its worst case.
// worsePricing returns the rate card a budget must assume when either a or b
// might serve the request. "Worse" means the one that cannot under-count:
// unpriced beats everything, and among priced members each rate takes the
// higher of the two.
func worsePricing(a, b Pricing) Pricing {
	// Unpriced wins. It is the one state a budget cannot enforce against, so
	// reporting the other member's rates would let a run be costed at rates
	// that do not apply to the member actually serving it. This is the same
	// fail-closed direction as Known, which is only true when both are known.
	if a.IsZero() || b.IsZero() {
		return Pricing{}
	}

	// Two genuinely free members stay free. Falling through would drop the Free
	// flag and produce all-zero rates, which reads back as unpriced and would
	// refuse a run that costs nothing at all.
	if a.Free && b.Free {
		return Pricing{Free: true, Currency: a.Currency, AsOf: worseAsOf(a, b), Source: joinSource(a, b)}
	}

	// Rates in different currencies cannot be compared, let alone maxed, and
	// there is no conversion here. Report unpriced rather than invent a number:
	// a budget that refuses to run is recoverable, one that silently sums EUR
	// into USD is not.
	if a.currency() != b.currency() {
		return Pricing{}
	}

	return Pricing{
		Currency:           a.Currency,
		InputPerMTok:       maxFloat(a.InputPerMTok, b.InputPerMTok),
		OutputPerMTok:      maxFloat(a.OutputPerMTok, b.OutputPerMTok),
		CachedInputPerMTok: maxFloat(a.CachedInputPerMTok, b.CachedInputPerMTok),
		CacheWritePerMTok:  maxFloat(a.CacheWritePerMTok, b.CacheWritePerMTok),
		PerRequest:         maxFloat(a.PerRequest, b.PerRequest),
		AsOf:               worseAsOf(a, b),
		Source:             joinSource(a, b),
	}
}

// worseAsOf returns the less trustworthy of two recording dates: an unknown
// date beats any known one, and otherwise the older wins. Keeping the first
// operand's date would date the merged card by whichever member happened to be
// primary rather than by the staler number in it.
func worseAsOf(a, b Pricing) string {
	if a.AsOf == "" || b.AsOf == "" {
		return ""
	}
	if a.AsOf < b.AsOf { // ISO dates sort lexicographically
		return a.AsOf
	}
	return b.AsOf
}

// joinSource names every provenance that fed the merged card, so a rate that
// came from the secondary is not attributed to the primary's pricing page.
func joinSource(a, b Pricing) string {
	switch {
	case a.Source == b.Source:
		return a.Source
	case a.Source == "":
		return b.Source
	case b.Source == "":
		return a.Source
	default:
		return a.Source + " + " + b.Source
	}
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// SupportsServerTool reports whether the model accepts a server-executed tool
// of the given kind.
func (mc ModelCapabilities) SupportsServerTool(k ServerToolKind) bool {
	for _, have := range mc.ServerTools {
		if have == k {
			return true
		}
	}
	return false
}

func weakerStructuredOutput(a, b StructuredOutputMode) StructuredOutputMode {
	if a == StructuredOutputNone || b == StructuredOutputNone {
		return StructuredOutputNone
	}
	if a == StructuredOutputToolCall || b == StructuredOutputToolCall {
		return StructuredOutputToolCall
	}
	return StructuredOutputNative
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func minNonZero(a, b int) int {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ── Provider integration ────────────────────────────────────────────

// CapabilityReporter is an optional interface providers implement to declare
// what the model they target supports. Decorators (retry, fallback, cache,
// otel) must forward it, or every capability check downgrades to "unknown" the
// moment a provider is wrapped.
type CapabilityReporter interface {
	Provider
	Capabilities() ModelCapabilities
}

// ProviderCapabilities returns p's declared capabilities. The bool is false
// when p does not report any, which is not the same as reporting none: an
// unreporting provider is unknown, and callers that must fail closed should
// treat it as such.
func ProviderCapabilities(p Provider) (ModelCapabilities, bool) {
	if cr, ok := p.(CapabilityReporter); ok {
		return cr.Capabilities(), true
	}
	return ModelCapabilities{}, false
}

// MissingCapabilities returns the subset of want that p does not declare. A
// provider that reports nothing is treated as missing everything, so a caller
// can gate on a non-empty result without a second nil check.
func MissingCapabilities(p Provider, want ...Capability) []Capability {
	mc, ok := ProviderCapabilities(p)
	if !ok {
		return append([]Capability(nil), want...)
	}
	return mc.Missing(want...)
}

// ProviderContentSupport resolves the media types p handles natively,
// preferring the model-level declaration over the adapter-level
// ContentNegotiator. The adapter can only say what it knows how to encode; the
// model decides what it can actually read.
func ProviderContentSupport(p Provider) ContentSupport {
	if mc, ok := ProviderCapabilities(p); ok && len(mc.Media.NativeTypes) > 0 {
		return mc.Media
	}
	if cn, ok := p.(ContentNegotiator); ok {
		return cn.ContentSupport()
	}
	return ContentSupport{}
}
