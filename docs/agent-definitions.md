# Agent definitions

An agent definition is a portable, versioned description of one agent: a Markdown file whose YAML frontmatter says what the agent is and whose body is its system prompt. The CLI, `saige serve` and `saige-mcp` load definitions the same way, and so can your own host through the Go packages.

Four concerns stay separate:

| Layer | Package | Job |
| --- | --- | --- |
| What a definition is | `agent/definition` | `Definition`, strict `Parse`, the JSON Schema |
| Where it comes from | `agent/definition`, `agent/definition/pgsource` | `Source`: a directory, an `fs.FS`, a reader, HTTPS, Postgres, and `Layered` over any of them |
| How a reference resolves | `agent/definition` | `Registry`: `name@range` to one version, reference checks, cycles, digests, reload |
| How it becomes an agent | `agent/definition/bind` | `Bind`: model, tools, skills, memory, sub-agents, approvals, compaction, guardrails and limits as agent options |

Loading a definition never connects to a server, reads a credential or runs a tool. Only `Bind` does, with what the host puts in its `Env`. [D-43](../DESIGN_DECISIONS.md) records why.

## A first definition

<!-- fsrc src="../examples/agents/assistant.agent.md" fence="markdown" -->
```markdown
---
apiVersion: saige/v1
name: assistant
version: 1.0.0
description: A concise assistant that reads files in the workspace to answer questions.
model: anthropic/claude-haiku-5-5
dials:
  creativity: focused
tools:
  harness: [read]
limits:
  max_iterations: 6
metadata:
  owner: examples
---
You are a concise assistant working in a software repository.

Answer the question you are given in a few sentences. When the answer
depends on the files in the workspace, read them first with your tools
instead of guessing, and name the files you relied on.
```
<!-- /fsrc -->

Run it:

```bash
saige ask --agents-dir examples/agents --agent assistant "What module path does go.mod declare?"
```

A definition with a sub-agent and approval rules:

<!-- fsrc src="../examples/agents/repo-steward.agent.md" fence="markdown" -->
```markdown
---
apiVersion: saige/v1
name: repo-steward
version: 1.2.0
description: Answers questions about a repository's state, running read-only git commands and delegating file reading to the assistant.
model:
  use: anthropic
  fallback: [openai/gpt-6-luna]
tools:
  harness: [read, exec]
subagents:
  - ref: assistant@^1.0
    description: Hand the assistant a question that needs files read and summarized.
    budget:
      max_iterations: 4
      timeout: 2m
approval:
  allow:
    - Bash(git status:*)
    - Bash(git log:*)
    - Bash(git diff:*)
  deny:
    - Bash(git push:*)
    - Bash(rm:*)
  grant: tool
  deny_after: 2
compaction:
  strategy: clear_tool_results
  keep_tool_results: 4
limits:
  max_iterations: 10
  budget:
    max_cost: 0.25
---
You look after a git repository and answer questions about its state.

Use shell commands through execute_code with language shell for git: status,
log and diff run without asking, and anything else waits for a person to
approve it. Never push and never delete files.

When a question needs the contents of files rather than git history, delegate
it to the assistant and use its answer. Reply with a short summary and the
commands you ran.
```
<!-- /fsrc -->

## File format

The file starts with a `---` line, then YAML frontmatter, then a closing `---` line. Everything after it is the system prompt. `saige agent schema` prints the JSON Schema of the frontmatter (also checked in as `agent/definition/definition.schema.json`) for editors.

Decoding is strict, as for the [model catalog](catalog.md): an unknown key (with a suggestion for a likely typo), a duplicate key, a value of the wrong kind, `null`, and YAML anchors, aliases, tags and merge keys are errors. Every error names the file, the line and the field path, such as `reviewer.agent.md: line 12: skills[1].mode: "lazzy" is not one of lazy, eager, pinned, search, trigger; did you mean "lazy"?`.

The digest is `sha256:` over the file with line endings normalized, so a checkout with CRLF endings has the same digest.

| Field | Meaning |
| --- | --- |
| `apiVersion` | Required. `saige/v1`; any other value is rejected |
| `name` | Required. Lowercase letters, digits and hyphens, starting with a letter, at most 48 characters. Also the agent's name at run time |
| `version` | Required. A semantic version such as `1.4.0` |
| `description` | What the agent is for. A parent shows it in the sub-agent's tool |
| `model` | A catalog preset name or `provider/model` (`vertex/` selects Vertex AI), or an object with `use` and a `fallback` list of `provider/model`. A fallback without credentials is dropped with a warning |
| `dials` | [Dials](dials.md): `creativity`, `reasoning`, `max_output`, `tools`, `parallel`, `reproducible`, `cache` |
| `tools` | `harness` groups (`read`, `write`, `exec`, `web`), `mcp` servers by the host's name for them (with an optional `allow` list of remote tool names), and `registry` tools by name |
| `skills` | Skill names, or objects with `name`, `mode`, `triggers` and `hash` (see below) |
| `memory` | `store` (a store the host names), `recall` (`tool`, `inject`, `select` or `off`), `write` kinds, `budget`, `retention`, `namespace`, `read_only` |
| `subagents` | References `name@range`, or objects with `ref`, `mode` (`delegate`, `spawn` or `handoff`), `description` and `budget` (`max_iterations`, `timeout`, `max_cost`, `max_tokens`, `max_requests`) |
| `approval` | `capabilities`, `allow`, `ask` and `deny` rules, `grant`, `deny_after`, `ramp_after`, `hide_denied` (see below) |
| `compaction` | The catalog's compaction form: `strategy` and its parameters, or a `chain` (see [context management](context-management.md)) |
| `guardrails` | `input` and `output` lists of built-ins: `pii`, `regex` (with `patterns` of `label` and `expr`), `max_length` (with `max`), `classifier` (with `policy`); `redact` and `parallel` where they apply |
| `limits` | `max_iterations`, `llm_timeout`, `tool_timeout`, and a `budget` with `max_cost`, `max_tokens`, `max_requests`, `warn_at`, `on_exceed` (`stop` or `ask`), `allow_unpriced` |
| `metadata` | Free-form strings. Never changes behavior |

### Skill modes

| Mode | How the skill reaches the model |
| --- | --- |
| `lazy` (default) | Listed in the system prompt; the model loads it with `load_skill` |
| `eager` | Its instructions are in the system prompt, from the snapshot taken at bind time |
| `pinned` | `eager` for one exact snapshot: the skill's hash must equal `hash`, or binding fails. `saige agent show` prints the hash |
| `search` | Left out of the prompt; the model finds it with `search_skills` |
| `trigger` | Listed like `lazy`, and its instructions are put in front of any user message that matches one of `triggers` (Go regular expressions) |

Only the skills a definition names are reachable, as `skills.AllowNames` makes them. Skills supply content and never grant permission ([D-28](../DESIGN_DECISIONS.md)).

### Approval rules

Rules use the permission syntax of Claude Code. A rule is a tool name, optionally ending in `*`, with an optional specifier in parentheses:

| Rule | Covers |
| --- | --- |
| `read_file` | Every call of `read_file` |
| `mcp_github_*` | Every tool whose name starts with `mcp_github_` |
| `Bash(git status:*)` | Shell commands that start with `git status`, through `bash` or `execute_code` with language `shell` |
| `Bash(npm test)` | Exactly the command `npm test` |
| `Read(src/**)` | File tools reading a path under `src/` |
| `WebFetch(domain:go.dev)` | `fetch_url` for `go.dev` and its subdomains |

The Claude Code names `Bash`, `Read`, `Write`, `Edit`, `Glob`, `Grep`, `LS` and `WebFetch` name saige's `bash` and `execute_code`, `read_file`, `write_file`, `edit_file`, `glob`, `grep`, `list_dir` and `fetch_url`.

A call is decided in this order: a `deny` rule refuses it, an `ask` rule asks, an `allow` rule runs it, a tool that carried an approval marker asks, and anything else falls to its capability class. `capabilities` sets the class defaults; without it reads run and writes, destructive and undeclared tools ask.

An `allow` rule matches a shell command only when it is one simple command, so `Bash(git status:*)` never allows `git status && rm -rf ~`, `git status; curl …` or a command with a redirect or substitution: those ask. `ask` and `deny` rules match when any command in a compound one matches.

With an approval block the tools give their approval markers to the gate, so an `allow` rule can run `execute_code` without a prompt. Delegating to a sub-agent is never held; the sub-agent's own calls are. The host's own gate (`Env.ToolGate`) still applies to every call.

`grant` caps the scope a person may attach to an approval ([approval policy](approval-policy.md)): `once`, `args`, `tool` or `session`. `deny_after`, `ramp_after` and `hide_denied` map onto `agent.ApprovalPolicy`.

## Sources and layers

| Source | Reads |
| --- | --- |
| `DirSource(dir)` | Every `*.agent.md` under `dir`, recursively, and every `.md` file directly inside a directory named `agents`. The walk is confined to `dir` |
| `FSSource(name, fsys, root)` | The same files in an `fs.FS`, such as definitions compiled in with `embed` |
| `ReaderSource(name, open)` | One file from any reader: an object store, a blob, a fixture |
| `HTTPSource(url, opts)` | One file over HTTPS, with `ETag` and `If-None-Match` so an unchanged file costs a 304 |
| `pgsource.New(pool, notifier)` | Rows of `saige_agent_definitions` (name, version, body, digest) |
| `Layered(sources...)` | Its sources lowest first. A later definition with the same name and version replaces an earlier one; other versions stay side by side |

`Untrusted(src)` marks a source whose definitions arrived with a cloned repository. Such a definition is checked against the allowlist in `definition.UntrustedFields`, as an untrusted catalog layer is. It may not set `tools.mcp`, `memory`, `dials`, `compaction`, `approval.allow`, `approval.capabilities`, `approval.ramp_after` or `limits.budget.allow_unpriced`, and it may use only the `read` and `write` harness groups. A field added to the format stays closed to untrusted definitions until someone decides.

### Postgres

`postgres.RunMigrations` creates `saige_agent_definitions` and a trigger that announces every change on the `saige_agent_definitions` channel, including edits made with plain SQL. `Source.Put` parses a definition before it writes anything, and `Load` rejects a row whose body does not match its name, version or digest.

```go
notifier := postgres.NewNotifier(pool, postgres.NotifierOptions{})
src := pgsource.New(pool, notifier)
reg := definition.NewRegistry(src, definition.Checks{})
if _, err := reg.Load(ctx); err != nil { ... }
stop, err := reg.Watch(ctx, definition.WatchOptions{}) // reloads on NOTIFY
```

## Resolution and pinning

`Registry.Resolve("reviewer@^1.2")` returns the highest version the range admits. Ranges use npm syntax: exact (`1.2.3`), partial (`1.2`, `1.x`), caret (`^1.2`), tilde (`~1.2.3`), comparators (`>=1.0.0 <2.0.0`), hyphen ranges and `||`. A pre-release matches only a range that names a pre-release of the same version.

`Load` checks the whole set before it replaces what the registry holds: every sub-agent reference resolves, there is no cycle, and the `Checks` you pass accept every model and skill (`bind.ModelCheck` and `bind.SkillCheck` check them against a catalog and a skill catalog). A failed reload keeps the previous set. The errors name what is missing and what is available:

```text
invalid agent definition lead@1.0.0: subagents[0].ref: no version of helper matches "^2" (available: 1.4.0, 1.0.0)
invalid agent definition a@1.0.0: subagents: sub-agent cycle: a@1.0.0 -> b@1.0.0 -> a@1.0.0
```

A resolution is pinned. Its `Digest` covers the definition and every sub-agent it resolved to, and a reload never changes a `Resolved` already handed out, so a running agent keeps the definition it started with. `Registry.Pinned(digest)` finds the same resolution again. `bind.Bound.Pin()` returns the name, version and digest to record with a run; `saige serve --agent` returns it as `agent` when a session is created.

`Registry.Watch` reloads when the source is a `Watcher` (Postgres with a notifier) and every `WatchOptions.Interval` otherwise.

## Binding

```go
res, err := reg.Resolve("repo-steward@^1")
b, err := bind.Bind(ctx, res, bind.Env{
    Catalog: catalog.Default(),
    Harness: tools.HarnessOptions{Root: "."},
    Skills:  skillCatalog,
})
defer b.Close(ctx)
a, err := b.NewAgent()
```

| Definition | Becomes |
| --- | --- |
| `model` | A `preset.Build` bundle, applied with `agent.WithPreset`. `Env.Preset` overrides the root agent's model |
| `dials` | `agent.Config.Dials` |
| `tools.harness` | A `tools.Harness` toolset under `Env.Harness.Root`, one per agent |
| `tools.mcp` | An `mcp.Pool` over `Env.MCPServers`, its tools and its gate. `allow` may only narrow the host's own allow list |
| `tools.registry` | Tools from `Env.Tools` |
| `skills` | The skill tools and prompt section over `Env.Skills`; a `UserInput` hook for triggers |
| `memory` | `memory.Tools` over `Env.MemoryStores` and `Env.MemoryScope`; a `UserInput` hook for `inject` and `select` |
| `subagents` | `agent.SubAgentDef` (delegate, spawn) or `agent.HandoffDef` (handoff) |
| `approval` | A `ToolGate` behind `Env.ToolGate`, and `agent.ApprovalPolicy` |
| `compaction` | `agent.Config.CompactCfg` |
| `guardrails` | `agent.InputGuardrail` and `agent.OutputGuardrail` from `agent/guardrail`; a classifier runs on the agent's model |
| `limits` | `MaxIter`, `LLMTimeout`, `ToolTimeout`, and a `types.Budget`. Each `Bound.NewAgent` call, and each delegation, gets a budget of its own |

A sub-agent inherits its parent's run policy as every sub-agent does (see [delegation](delegation.md)), and its own definition replaces what it declares: its approval rules (still behind the host's gate), compaction, dials, timeouts and budget. A sub-agent with no approval block keeps its parent's. A handoff member shares its entry agent's run, so a definition used with `mode: handoff` may not declare approval, skills, memory, compaction, guardrails, MCP servers, sub-agents, a budget or timeouts; binding says which.

Everything a definition names by reference must be in the `Env`. A missing preset, tool, MCP server, skill catalog or memory store fails with `bind.ErrUnsupported` and says what is missing.

## CLI

Directories are searched lowest precedence first: `~/.config/saige/agents`, the project's `.saige/agents` (untrusted unless `SAIGE_TRUST_PROJECT_AGENTS=1`, or named with `--agents-dir`), then each `--agents-dir`.

```bash
saige agent list                                  # every definition, highest version first
saige agent show repo-steward@^1                  # source, digests, sub-agents, skill hashes, the file
saige agent validate [PATH...]                    # offline checks; exits 2 when anything is invalid
saige agent schema                                # JSON Schema of the frontmatter

saige ask --agent assistant "question"            # also chat and serve
saige ask --agent repo-steward --model claude-haiku-5-5 --provider anthropic "question"
saige serve --agent repo-steward --workspace . --agents-reload 10s
saige ask --agent researcher --mcp-config .mcp.json "question"
```

With `--agent`, the harness tools work under `--workspace` with the CLI's sandbox flags, `registry` tools come from `--rag-db` and `--kg-db`, MCP servers from `--mcp-config`, and skills from `.agents/skills`, `.claude/skills` and `~/.agents/skills`. `--preset`, `--model` and `--provider` override the definition's model; `--tools` and `--system` cannot be combined with `--agent`. The CLI provides no memory store, so a definition with a `memory` block runs only in a host that provides one. `saige serve --agent` binds a fresh agent for each session from the definition the registry resolves when the session starts; `--agents-reload` lets new sessions see edits while running sessions keep their pin.

`saige-mcp --agents-dir DIR --agent NAME` publishes the definition as the agent tool, named after the definition unless `--agent-tool` says otherwise. Its harness tools work under `--root`, and its `registry` tools are the packs the server exposes. A name the directory does not hold falls back to a preset or `provider/model`, as before. Each call binds the pinned resolution afresh, so calls share no scratch workspace or budget.
