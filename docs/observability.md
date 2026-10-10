# Observability

`agent/otel` adds OpenTelemetry spans and metrics to an agent. It is opt-in: nothing in `agent` imports it.

## Enable tracing

```go
a, err := agent.New(agent.Config{
    Provider:  provider,
    Tools:     tools,
    SubAgents: []agent.SubAgentDef{researcher},
    Metrics:   myMetrics, // optional; keeps receiving every record
}, otel.WithTracing(otel.Config{
    TracerProvider: tp,
    MeterProvider:  mp,
    Redactor:       maskEmails, // optional
}))
if err != nil {
    return err
}
```

`WithTracing` wraps the provider and tools. It also wraps the provider and tools of sub-agents and handoff members that are set when the option runs. Put `WithSubAgents` and `WithHandoffs` before it, or set them on the base config.

A `Metrics` sink set before the option keeps receiving every record next to the OTel bridge. If the meter cannot create its instruments, the error is logged and the existing sink stays in place.

## Spans

| Span | Kind | Attributes |
|------|------|------------|
| `chat {model}` | client | `gen_ai.operation.name`, `gen_ai.provider.name`, `gen_ai.request.model`, `gen_ai.request.*` options, `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`, `gen_ai.usage.cache_read.input_tokens`, `gen_ai.usage.cache_creation.input_tokens`, `gen_ai.response.model`, `gen_ai.response.id`, `gen_ai.response.finish_reasons`, `gen_ai.response.time_to_first_chunk`, `saige.route.*`, `saige.dials.*` |
| `execute_tool {name}` | internal | `gen_ai.tool.name`, `gen_ai.tool.description`, `saige.tool.capability` |
| `invoke_agent {name}` | internal | `gen_ai.agent.name` |

Usage is merged across the parts a provider streams and written once when the stream ends, so a provider that reports prompt tokens first and completion tokens last keeps both. Partial usage is kept when the stream fails.

`saige.route.profile`, `.provider`, `.model`, `.experiment`, `.variant` and `.reason` come from the last route a router or traffic split reported for the call. A profile built from a catalog preset also sets `saige.route.preset`, `saige.route.config_hash` and `saige.catalog.revision`.

Each route is also recorded as a `saige.route.attempt` span event with its route attributes and the attempt's effective `gen_ai.request.*` options. When the call ends, the span's `gen_ai.request.*` attributes are set from the serving attempt's effective options: the profile's configured options merged with the per-call overrides. Without a route that reports options, they reflect the per-call overrides only.

A call with [dials](dials.md) also records how they compiled: `saige.dials.requested` lists every dial as `name:value`, `saige.dials.mapped` the mapped ones as `name:from→to`, `saige.dials.dropped` the dropped ones, and `saige.dials.policy` the policy in force. A router reports them per attempt on its route, so they appear on each `saige.route.attempt` event and, for the serving attempt, on the span. For a single adapter that declares its capabilities, the traced call compiles the dials itself and records the effective options it sends.

`gen_ai.response.time_to_first_chunk` is measured from before the provider call to the first text chunk, so it includes connection setup.

`WithTracing` also sets `agent.Config.RunTracer`, so each run opens an `invoke_agent` span. Every chat and tool span of the run, including those of its sub-agents, shares one trace under it, and the span is marked failed when the run fails. To trace a run of an agent built without `WithTracing`, set `RunTracer: otel.NewAgentTracer(cfg)` on its config.

## Errors

A failed span gets an error status, an exception event, and these attributes:

| Attribute | Value |
|-----------|-------|
| `error.type` | The error kind for a classified error (`rate_limit`, `context_length`, `auth`, `unavailable`, `truncated`, ...), `canceled` or `timeout` for a context error, `tool_error` for a tool result flagged `IsError`, `panic` for a recovered tool panic, or the Go type name otherwise |
| `saige.error.transient` | Whether a retry can succeed. Set for classified errors only |
| `saige.error.retry_after_ms` | The delay the server asked for, when it sent one |

Provider and tool errors can contain request bodies or user data. Set `Config.Redactor` to rewrite every message before it reaches a span status or exception event. Spans carry no message content.

## Metrics

| Instrument | Unit | Operations |
|------------|------|------------|
| `gen_ai.client.operation.duration` | s | `chat`, `execute_tool`, `invoke_agent` (keyed by `gen_ai.operation.name`) |
| `gen_ai.client.token.usage` | `{token}` | `gen_ai.token.type` is `input`, `output`, `cache_read` or `cache_creation` |
| `gen_ai.client.operation.time_to_first_chunk` | s | `chat` |

The agent loop records `input` and `output` token usage, `cache_read` and `cache_creation` usage when the provider reports prompt caching, and one `invoke_agent` duration per run with `error.type` set when the run failed. A custom `Metrics` sink receives the cache counts and the run outcome by also implementing `types.CacheUsageRecorder` and `types.AgentOutcomeRecorder`; otherwise it gets `RecordAgentInvocation`.

## Wrappers keep tool behavior

The agent loop looks for optional interfaces on a tool: markers for approval, `RichTool` for typed parts and citations, `Cacheable`, `Configurable`, and the handoff and sub-agent interfaces. `otel.WrapTool` keeps each of them:

| Tool | Result |
|------|--------|
| `*types.MarkedTool` | Re-marked around a traced inner tool. The approval prompt still fires and the span covers only the approved work |
| `HandoffSignaler` or `SubAgentInvoker` | Returned unchanged |
| `Configurable` | Stays `Configurable`; binding returns a traced instance |
| Already traced | Returned unchanged |
| Anything else | `*otel.TracedTool`, which is a `RichTool` and reports the inner tool's cache policy |

`*otel.TracedTool` and `*toolcache.Tool` implement `Unwrap() types.Tool`, and `*otel.TracedProvider` implements `Unwrap() types.Provider`, so an interface behind a decorator can be found by walking the chain.
