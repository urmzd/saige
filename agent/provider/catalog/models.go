package catalog

import (
	"github.com/urmzd/saige/agent/registry"
	"github.com/urmzd/saige/agent/types"
)

// This file is the model registry itself. Rows are grouped by provider and
// declared by family prefix, longest match wins.
//
// Reading a row: a Capability present means the flag is accepted and honoured.
// A knob absent from a reasoning model's row is usually absent because the API
// *rejects* it, not because nobody set it: OpenAI's reasoning models return a
// 400 for temperature and top_p, which is exactly the class of gap this table
// exists to make visible before the request is sent.
//
// # On the numbers
//
// Limits and prices are third-party facts on someone else's release schedule.
// They are declared only where they are solid, zero means undeclared rather
// than unlimited or free, and every priced row carries an AsOf date so
// staleness is visible rather than assumed away. Rows whose pricing this
// registry cannot state confidently are left unpriced on purpose: a Budget
// refuses to enforce against an unpriced model, which is the right failure.
// Correct a row in-process with Register; it appends a revision, so History
// shows what changed and Rollback undoes it.
const asOf = "2026-07-24"

// Shared media sets.
var (
	imagesAndPDF  = media(types.MediaJPEG, types.MediaPNG, types.MediaGIF, types.MediaWebP, types.MediaPDF)
	ollamaImages  = media(types.MediaJPEG, types.MediaPNG)
	geminiNative  = media(types.MediaJPEG, types.MediaPNG, types.MediaGIF, types.MediaWebP, types.MediaPDF, types.MediaMP3, types.MediaWAV, types.MediaMP4)
	noNativeMedia = media()
)

// Capability bundles shared by many rows.
var (
	// toolUse is the baseline modern tool-calling surface.
	toolUse = []types.Capability{
		types.CapTools, types.CapParallelTools, types.CapToolChoice,
		types.CapStreaming, types.CapSystemPrompt,
	}
	// classicSampling is the knob set of a non-reasoning chat model.
	classicSampling = []types.Capability{
		types.CapTemperature, types.CapTopP, types.CapStopSequences, types.CapMaxOutputTokens,
	}
)

func with(base []types.Capability, extra ...types.Capability) types.ModelCapabilities {
	return caps(append(append([]types.Capability{}, base...), extra...)...)
}

// seed is Register with the provenance recorded, so History shows that revision
// 1 of every row came from this table rather than from an operator.
func seed(e Entry) registry.Entry[Entry] {
	return Register(e, registry.WithSource("catalog/models.go"), registry.WithNote("initial table"))
}

func init() {
	registerAnthropic()
	registerOpenAI()
	registerGoogle()
	registerOllama()
}

// ── Anthropic ───────────────────────────────────────────────────────
//
// Extended thinking is sized by token budget (minimum 1024) and returns signed
// blocks that must be echoed back verbatim on the next turn, or multi-turn tool
// use breaks. There is no native response_format: the adapter emulates schema
// output by forcing a hidden tool call, which is enforcement by convention, not
// by the API. Server-side web search is available and billed per search on top
// of tokens, and document citations are returned as structured data.

func registerAnthropic() {
	const p = "anthropic"

	base := func() types.ModelCapabilities {
		c := with(toolUse,
			types.CapTemperature, types.CapTopP, types.CapTopK,
			types.CapStopSequences, types.CapMaxOutputTokens,
			types.CapPromptCaching, types.CapStructuredOutput,
			types.CapParallelToolControl,
		)
		c.ContextWindow = 200_000
		c.DefaultMaxOutputTokens = 4096
		c.StructuredOutput = types.StructuredOutputToolCall
		c.Media = imagesAndPDF
		return c
	}

	thinking := func(pricing types.Pricing) types.ModelCapabilities {
		c := base().With(
			types.CapReasoning, types.CapReasoningBudget, types.CapReasoningSignature,
			types.CapServerTools, types.CapWebSearch, types.CapCitations, types.CapRemoteMCP,
		)
		c.MinReasoningBudget = 1024
		c.ServerTools = []types.ServerToolKind{types.ServerToolWebSearch, types.ServerToolRemoteMCP}
		c.Pricing = pricing
		c.Notes = []string{
			"max_tokens is required on every request; the adapter defaults it to 4096",
			"thinking blocks carry a signature that must round-trip or multi-turn tool use breaks",
			"temperature and top_p may not be combined with thinking on all versions",
			"server-side web search bills per search on top of tokens",
		}
		return c
	}

	nonThinking := func(maxOut int, pricing types.Pricing) types.ModelCapabilities {
		c := base().With(types.CapSeed)
		c = c.Without(types.CapSeed) // Anthropic has no seed; keep the row honest
		c.MaxOutputTokens = maxOut
		c.Pricing = pricing
		c.Notes = []string{"no extended thinking: reasoning knobs are rejected"}
		return c
	}

	price := func(in, out, cachedIn float64) types.Pricing {
		return types.Pricing{
			Currency: "USD", InputPerMTok: in, OutputPerMTok: out,
			CachedInputPerMTok: cachedIn, AsOf: asOf, Source: "vendor list price",
		}
	}

	// Claude 5: capabilities are known, list pricing is not stated here.
	for _, prefix := range []string{"claude-opus-5", "claude-sonnet-5", "claude-fable-5"} {
		c := thinking(types.Pricing{})
		c.Notes = append(c.Notes, "unpriced in this table: set Pricing via Register before enforcing a budget")
		seed(Entry{Provider: p, Prefix: prefix, Caps: c})
	}

	seed(Entry{Provider: p, Prefix: "claude-opus-4", Caps: thinking(price(15, 75, 1.50))})
	seed(Entry{Provider: p, Prefix: "claude-sonnet-4", Caps: thinking(price(3, 15, 0.30))})
	seed(Entry{Provider: p, Prefix: "claude-haiku-4", Caps: thinking(price(1, 5, 0.10))})
	seed(Entry{Provider: p, Prefix: "claude-3-7-sonnet", Caps: thinking(price(3, 15, 0.30))})

	seed(Entry{Provider: p, Prefix: "claude-3-5-sonnet", Caps: nonThinking(8192, price(3, 15, 0.30))})
	seed(Entry{Provider: p, Prefix: "claude-3-5-haiku", Caps: nonThinking(8192, price(0.80, 4, 0.08))})
	seed(Entry{Provider: p, Prefix: "claude-3-opus", Caps: nonThinking(4096, price(15, 75, 1.50))})
	seed(Entry{Provider: p, Prefix: "claude-3-haiku", Caps: nonThinking(4096, price(0.25, 1.25, 0.03))})

	fallback := base()
	fallback.Notes = []string{"unrecognised Claude model: reasoning, server tools and pricing not assumed"}
	RegisterBaseline(p, fallback)
}

// ── OpenAI ──────────────────────────────────────────────────────────
//
// The reasoning models are the sharpest example of why a capability table is
// needed: they take reasoning_effort and *reject* temperature, top_p, and the
// repetition penalties. Sending a request built for gpt-4o to o3 is a 400, not
// a silently ignored field. Note also that the -mini variants carry their own
// rows purely so longest-prefix matching does not price them at their larger
// sibling's rate.

func registerOpenAI() {
	const p = "openai"

	price := func(in, out, cachedIn float64) types.Pricing {
		return types.Pricing{
			Currency: "USD", InputPerMTok: in, OutputPerMTok: out,
			CachedInputPerMTok: cachedIn, AsOf: asOf, Source: "vendor list price",
		}
	}

	reasoning := func(ctxWindow, maxOut int, pricing types.Pricing) types.ModelCapabilities {
		c := with(toolUse,
			types.CapMaxOutputTokens, types.CapStructuredOutput,
			types.CapReasoning, types.CapReasoningEffort, types.CapPromptCaching,
			types.CapParallelToolControl,
			types.CapServerTools, types.CapWebSearch, types.CapCitations, types.CapRemoteMCP,
		)
		c.ContextWindow = ctxWindow
		c.MaxOutputTokens = maxOut
		c.ReasoningEfforts = []string{"minimal", "low", "medium", "high"}
		c.StructuredOutput = types.StructuredOutputNative
		c.ServerTools = []types.ServerToolKind{types.ServerToolWebSearch, types.ServerToolCodeExecution, types.ServerToolRemoteMCP}
		c.Media = imagesAndPDF
		c.Pricing = pricing
		c.Notes = []string{
			"rejects temperature, top_p, frequency_penalty and presence_penalty",
			"reasoning tokens are billed as output but are not returned as content",
			"the chat-completions surface caps output with max_completion_tokens, not max_tokens",
			"server-side tools and citations require the Responses API, which this adapter does not use yet",
		}
		return c
	}

	chat := func(ctxWindow, maxOut int, structured bool, pricing types.Pricing) types.ModelCapabilities {
		c := with(toolUse, classicSampling...)
		c = c.With(types.CapSeed, types.CapFrequencyPenalty, types.CapPresencePenalty,
			types.CapPromptCaching, types.CapParallelToolControl)
		if structured {
			c = c.With(types.CapStructuredOutput)
			c.StructuredOutput = types.StructuredOutputNative
		}
		c.ContextWindow = ctxWindow
		c.MaxOutputTokens = maxOut
		c.Media = imagesAndPDF
		c.Pricing = pricing
		c.Notes = []string{"no reasoning: reasoning_effort is not accepted"}
		return c
	}

	seed(Entry{Provider: p, Prefix: "gpt-5", Caps: reasoning(400_000, 128_000, price(1.25, 10, 0.125))})
	seed(Entry{Provider: p, Prefix: "o1", Caps: reasoning(200_000, 100_000, price(15, 60, 7.50))})
	seed(Entry{Provider: p, Prefix: "o3", Caps: reasoning(200_000, 100_000, price(2, 8, 0.50))})
	seed(Entry{Provider: p, Prefix: "o4-mini", Caps: reasoning(200_000, 100_000, price(1.10, 4.40, 0.275))})

	seed(Entry{Provider: p, Prefix: "gpt-4.1", Caps: chat(1_000_000, 32_768, true, price(2, 8, 0.50))})
	seed(Entry{Provider: p, Prefix: "gpt-4.1-mini", Caps: chat(1_000_000, 32_768, true, price(0.40, 1.60, 0.10))})
	seed(Entry{Provider: p, Prefix: "gpt-4o", Caps: chat(128_000, 16_384, true, price(2.50, 10, 1.25))})
	seed(Entry{Provider: p, Prefix: "gpt-4o-mini", Caps: chat(128_000, 16_384, true, price(0.15, 0.60, 0.075))})

	turbo := chat(128_000, 4096, false, price(10, 30, 0))
	turbo.Notes = append(turbo.Notes, "json_object mode only: no strict json_schema enforcement")
	seed(Entry{Provider: p, Prefix: "gpt-4-turbo", Caps: turbo})

	embed := caps(types.CapEmbeddings)
	embed.Media = noNativeMedia
	embed.Pricing = types.Pricing{Currency: "USD", InputPerMTok: 0.02, AsOf: asOf, Source: "vendor list price"}
	seed(Entry{Provider: p, Prefix: "text-embedding-3", Caps: embed})
	seed(Entry{Provider: p, Prefix: "text-embedding-ada", Caps: embed})

	fallback := chat(0, 0, false, types.Pricing{})
	fallback.Notes = []string{"unrecognised OpenAI model: reasoning, strict schema and pricing not assumed"}
	RegisterBaseline(p, fallback)
}

// ── Google ──────────────────────────────────────────────────────────
//
// Gemini sizes reasoning two different ways depending on generation: 2.5 takes
// a thinking budget in tokens (0 disables it on flash), 3.x takes a thinking
// *level*. Both are ThinkingConfig fields, which is why a single
// "reasoning_budget" flag would have been a lie for half the family. Search
// grounding is a server-side tool and returns grounding metadata this SDK maps
// onto citations.

func registerGoogle() {
	const p = "google"

	price := func(in, out, cachedIn float64) types.Pricing {
		return types.Pricing{
			Currency: "USD", InputPerMTok: in, OutputPerMTok: out,
			CachedInputPerMTok: cachedIn, AsOf: asOf, Source: "vendor list price",
		}
	}

	gemini := func(reasoningKnob types.Capability, levels []string, ctxWindow, maxOut int, pricing types.Pricing) types.ModelCapabilities {
		c := with(toolUse,
			types.CapTemperature, types.CapTopP, types.CapTopK, types.CapSeed,
			types.CapStopSequences, types.CapMaxOutputTokens,
			types.CapFrequencyPenalty, types.CapPresencePenalty,
			types.CapSafetySettings, types.CapStructuredOutput, types.CapPromptCaching,
			types.CapServerTools, types.CapWebSearch, types.CapCodeExecution, types.CapCitations,
		)
		if reasoningKnob != "" {
			c = c.With(types.CapReasoning, reasoningKnob, types.CapReasoningSignature)
			c.ReasoningEfforts = levels
		}
		c.ContextWindow = ctxWindow
		c.MaxOutputTokens = maxOut
		c.StructuredOutput = types.StructuredOutputNative
		c.ServerTools = []types.ServerToolKind{types.ServerToolWebSearch, types.ServerToolCodeExecution}
		c.Media = geminiNative
		c.Pricing = pricing
		return c
	}

	// 3.x: thinking level. Pricing not stated here.
	lvl := []string{"low", "high"}
	for _, prefix := range []string{"gemini-3.1-pro", "gemini-3-pro", "gemini-3.1-flash", "gemini-3-flash"} {
		c := gemini(types.CapReasoningEffort, lvl, 1_000_000, 65_536, types.Pricing{})
		c.Notes = []string{
			"reasoning depth is set with ThinkingConfig.ThinkingLevel, not a token budget",
			"thought signatures must round-trip or multi-turn function calling degrades",
			"unpriced in this table: set Pricing via Register before enforcing a budget",
		}
		seed(Entry{Provider: p, Prefix: prefix, Caps: c})
	}

	// 2.5: thinking budget.
	pro := gemini(types.CapReasoningBudget, nil, 1_000_000, 65_536, price(1.25, 10, 0.31))
	pro.Notes = []string{
		"reasoning depth is set with ThinkingConfig.ThinkingBudget in tokens",
		"pro cannot disable thinking",
		"list price is tiered by prompt length; the shorter-prompt tier is declared here",
	}
	seed(Entry{Provider: p, Prefix: "gemini-2.5-pro", Caps: pro})

	flash := gemini(types.CapReasoningBudget, nil, 1_000_000, 65_536, price(0.30, 2.50, 0.075))
	flash.Notes = []string{
		"reasoning depth is set with ThinkingConfig.ThinkingBudget in tokens",
		"budget 0 disables thinking on flash",
	}
	seed(Entry{Provider: p, Prefix: "gemini-2.5-flash", Caps: flash})

	// 2.0 and earlier: no thinking. Vendor-deprecated, and still this repo's
	// CLI default, which is itself a gap (see docs/model-capabilities.md).
	legacy := gemini("", nil, 1_000_000, 8192, price(0.10, 0.40, 0.025))
	legacy.Notes = []string{"no thinking support", "vendor-deprecated: migrate to a 2.5 or 3.x model"}
	seed(Entry{Provider: p, Prefix: "gemini-2.0", Caps: legacy})

	embed := caps(types.CapEmbeddings)
	embed.Media = noNativeMedia
	for _, prefix := range []string{"text-embedding-004", "text-embedding-005", "gemini-embedding"} {
		seed(Entry{Provider: p, Prefix: prefix, Caps: embed})
	}

	fallback := gemini("", nil, 0, 0, types.Pricing{})
	fallback.Notes = []string{"unrecognised Gemini model: thinking and pricing not assumed"}
	RegisterBaseline(p, fallback)
}

// ── Ollama ──────────────────────────────────────────────────────────
//
// Ollama is the one provider whose capabilities are a property of the pulled
// weights rather than the endpoint, so the baseline is the honest answer for
// most models and the entries below only mark the families where a capability
// is reliably present. Tool calling and vision in particular vary by tag.
//
// Pricing is Free rather than unpriced: local inference genuinely costs
// nothing per token, and a Budget must be able to tell that from "no rate card
// available", which it refuses to run against.

func registerOllama() {
	const p = "ollama"

	free := types.Pricing{Currency: "USD", Free: true, AsOf: asOf, Source: "local execution"}

	local := func(extra ...types.Capability) types.ModelCapabilities {
		c := with([]types.Capability{types.CapStreaming, types.CapSystemPrompt},
			types.CapTemperature, types.CapTopP, types.CapTopK, types.CapSeed,
			types.CapStopSequences, types.CapMaxOutputTokens,
			types.CapContextWindowOverride, types.CapStructuredOutput,
		)
		c = c.With(extra...)
		c.StructuredOutput = types.StructuredOutputNative
		c.Media = noNativeMedia
		c.Pricing = free
		c.Notes = []string{
			"num_ctx defaults low and silently truncates: set it explicitly",
			"the format grammar constrains thinking too, so schema output with thinking on can return empty content",
			"no server-side tools: web search and code execution must be local tools",
		}
		return c
	}

	reasoning := func() types.ModelCapabilities {
		c := local(types.CapTools, types.CapParallelTools,
			types.CapReasoning, types.CapReasoningToggle)
		c.Notes = append(c.Notes, "thinking is toggled with the think flag; no signature is returned to round-trip")
		return c
	}

	for _, prefix := range []string{"qwen3", "deepseek-r1", "gpt-oss", "magistral"} {
		seed(Entry{Provider: p, Prefix: prefix, Caps: reasoning()})
	}
	for _, prefix := range []string{"llama3.1", "llama3.2", "llama3.3", "mistral", "qwen2.5", "command-r"} {
		seed(Entry{Provider: p, Prefix: prefix, Caps: local(types.CapTools, types.CapParallelTools)})
	}
	for _, prefix := range []string{"llava", "gemma3", "qwen2.5vl", "minicpm-v"} {
		vision := local()
		vision.Media = ollamaImages
		seed(Entry{Provider: p, Prefix: prefix, Caps: vision})
	}

	embed := caps(types.CapEmbeddings)
	embed.Media = noNativeMedia
	embed.Pricing = free
	for _, prefix := range []string{"nomic-embed", "mxbai-embed", "all-minilm", "bge-m3", "snowflake-arctic-embed"} {
		seed(Entry{Provider: p, Prefix: prefix, Caps: embed})
	}

	fallback := local()
	fallback.Notes = append(fallback.Notes,
		"unrecognised local model: tool calling, vision and thinking depend on the pulled weights")
	RegisterBaseline(p, fallback)
}
