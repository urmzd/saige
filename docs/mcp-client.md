# MCP client

`agent/mcp` connects an agent to Model Context Protocol servers and exposes their tools as ordinary `types.Tool` values.
Every imported tool is gateable, logged, and durable through the `StepRunner`, like a local tool.

```go
pool := mcp.NewPool()
defer pool.Close(ctx)

spec := mcp.Remote("docs", "https://mcp.example.com/mcp")
spec.Retry = mcp.DefaultRetryPolicy()
spec.MaxConcurrent = 4

if _, err := pool.Acquire(ctx, spec); err != nil {
    return err
}
if _, err := pool.RegisterAll(ctx, registry); err != nil {
    return err
}
agent.WithToolGate(types.Gates(pool.Gate(), mcp.CapabilityGate(mcp.DefaultCapabilityPolicy())))
```

## Configuration files

`mcp.LoadConfig` reads the `mcpServers` document that Claude Code and Gemini CLI use, so an existing `.mcp.json` works unchanged.

```json
{
  "mcpServers": {
    "fs":  {"command": "mcp-server-filesystem", "args": ["${HOME}/src"]},
    "api": {"type": "http", "url": "https://mcp.example.com/mcp",
            "headers": {"Authorization": "Bearer ${API_TOKEN}"}}
  }
}
```

`${VAR}` and `${VAR:-default}` expand from the environment, so secrets stay out of the file.
An unset variable with no default is an error.
Remote servers loaded from a file use `SafeHTTPClient(false)` by default, which refuses cloud metadata and link-local addresses at dial time.
`WithConfigRegistry` records each load as a revision that can be pinned or rolled back.

## Connections

| Need | Field or call |
|------|---------------|
| Share one session between agents | `Pool.Acquire`, keyed by `ServerSpec.Identity` |
| Retry throttled requests | `ServerSpec.Retry`, `ServerSpec.ConnectRetry` |
| Detect a dead connection early | `ServerSpec.KeepAlive` |
| Bound calls to one server | `ServerSpec.MaxConcurrent` |
| Rotating bearer token | `ServerSpec.TokenFunc` |
| OAuth authorization code, PKCE, dynamic registration | `ServerSpec.OAuthHandler` (go-sdk `auth` package) |
| Block internal addresses | `SafeHTTPClient(blockPrivate)` |
| Test a connection | `Probe`, `Pool.Health`, `Pool.Preflight` |

The identity covers the transport, header names with a hash of their values, the allowlist, and the prefix.
Two specs with different `AllowedTools` never share a session.
Policy fields such as `Gate`, `TokenFunc`, `HTTPClient`, retries, limits, and timeouts are not part of the identity.
A pool refuses to share a session between two specs with one identity whose policy fields differ, so a second spec's gate or credential is never silently dropped.
Functions and clients are compared by instance. Give each principal a distinct `Name` when one pool serves several.

### Failure handling

A call that finds its session dead reconnects once.
The call itself is repeated only when the tool is known to be idempotent: it is listed in `IdempotentTools`, or its annotations claim `readOnlyHint` or `idempotentHint` and the spec sets `TrustHints`.
An untrusted server can label a write as a read, and repeating that call would apply the write twice in the user's account on that server.
Otherwise the model receives an error result that says the call was not retried, because the first attempt may have run.

`RetryPolicy` follows the same rule for HTTP responses:

| Response | Retried for |
|----------|-------------|
| 429, or 503 with `Retry-After` | every method |
| 502, 503, 504 | read methods, and `tools/call` on idempotent tools |
| 500 and other 5xx | the handshake only (`ConnectRetry`) |
| 401, 403, other 4xx | never |
| transport error | never |

`Retry-After` and `Retry-After-Ms` are honored. `MaxTotalDelay` caps the sum of waits, and `OnRetry` reports each retry for metrics and budgets.

## Tool lists and drift

The client caches the server's catalog and drops it when the server sends `tools/list_changed`.
`ServerSpec.OnToolsChanged` then fires, and `Pool.Refresh` applies the change: new tools are registered, and removed tools leave the pool's routing.
A removed tool that is still in a registry keeps its route to its server, so `Pool.Gate` still applies that server's gate, and a call to it returns an error result instead of reaching the server until a later listing offers it again.

`Client.Catalog` returns the raw catalog with a fingerprint per tool.
The fingerprint covers the name, description, and input schema, so a rewritten description under the same name is detected.

```go
drift := mcp.DiffCatalogs(baseline, current)
if err := drift.Within(0, "search", "fetch"); err != nil {
    return err // a required tool disappeared or changed
}
```

## Names and trust

Tool names get the prefix `Name + "_"` by default.
Registration refuses a name that another server or a local tool already holds, so a remote server cannot replace a local tool and receive its calls.

Tool annotations become `ToolDef.Capability`.
A hint that raises the class (`destructiveHint`) is always honored.
A hint that lowers it (`readOnlyHint`, `destructiveHint: false`) is honored only with `TrustHints`, because an untrusted server can label a write as a read.
`CapabilityGate` turns the class into a gate decision. It never grants permission on its own: compose it with `types.Gates` and keep a per-server `Gate` for servers you do not control.

## Results

`MaxResultBytes` (default 256 KiB) caps text in one result, and `MaxBinaryBytes` (default 5 MiB) caps images, audio, and blobs.
Text is cut with a `[truncated N bytes]` marker.
Structured content is never cut, because partial JSON is invalid: an oversized document is dropped whole with a note.
An oversized image becomes a `[dropped image: type, N bytes]` note.
Each note appears in the text and as a text block, because providers send the blocks when a result has any.

`ArgTransform` rewrites arguments before each call. `CoerceScalarsToArrays` wraps a scalar in an array when the tool's schema declares an array.

## Resources, prompts, and progress

`Client.Resources`, `ReadResource`, `Prompts`, and `GetPrompt` expose the server's other primitives.
Resource content carries a citation and is data, never instructions: the host decides where it goes.

`mcp.WithProgress(ctx, fn)` receives progress notifications for calls made with that context.
Each call gets its own token, so concurrent calls never see each other's progress.
