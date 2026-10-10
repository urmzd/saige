# Running saige agents in other harnesses

An agent you define with saige (see [agent definitions](agent-definitions.md)) can run inside other programs:

- **Coding harnesses** such as Claude Code, Codex, Gemini CLI, opencode and the Cursor agent reach it as an MCP tool. `saige export` writes their configuration; `saige launch` starts them with it.
- **Editors** that host Agent Client Protocol (ACP) agents, such as Zed, JetBrains AI Assistant and the Neovim and Emacs ACP clients, run it as their agent with `saige acp`.
- **Your own client** talks to it over HTTP with `saige serve`.

All three hosts share one session layer: each session binds the definition when it starts, keeps that pinned version, checks approval grants against the definition's `approval.grant`, and releases its MCP connections when it ends.

## Quick start

```bash
# Claude Code, without changing any configuration
saige launch claude --agent reviewer -- -p "review the last commit"

# Write project configuration for a harness, then use it as usual
saige export codex --agent reviewer --dry-run   # show what would change
saige export codex --agent reviewer

# An ACP agent for an editor
saige acp --agent reviewer
```

`--agent` takes a definition name (or `NAME@RANGE`) found in `~/.config/saige/agents`, the project's `.saige/agents`, or `--agents-dir`. Export refuses an untrusted project definition: trust it with `SAIGE_TRUST_PROJECT_AGENTS=1` or name its directory with `--agents-dir`.

## What works where

"Verified" means the path was run end to end with the harness installed: the harness called the saige agent tool and returned its answer. "Unverified" means it follows the harness's documentation but was not run.

| Harness | `saige launch` | `saige export` writes | Approvals inside the agent | Status |
|---|---|---|---|---|
| Claude Code | `--mcp-config`, `--agents`, a temporary `--plugin-dir` for skills; no files written | `.mcp.json`, `.claude/skills/`, `.claude/agents/NAME.md` | MCP elicitation | Verified (launch and export, `claude -p`) |
| Codex CLI | `-c mcp_servers.saige-NAME.*` overrides; skills to `.agents/skills/` | `.codex/config.toml`, `.agents/skills/`, `.codex/agents/NAME.toml` | Elicitation when `approval_policy` allows `mcp_elicitations`, held approvals otherwise | Verified (launch, `codex exec`) |
| opencode | `OPENCODE_CONFIG_CONTENT` with the server and the agent; skills to `.agents/skills/` | `opencode.json`, `.agents/skills/`, `.opencode/agents/NAME.md` | Held approvals (no elicitation) | Verified (export, `opencode run`, and a held approval approved and resumed through `opencode serve`) |
| Gemini CLI | writes `.gemini/settings.json`; skills to `.agents/skills/` | `.gemini/settings.json`, `.agents/skills/`, `.gemini/agents/NAME.md` | Unverified (held approvals if the client has no elicitation) | Unverified |
| Cursor agent CLI | writes `.cursor/mcp.json`; skills to `.agents/skills/` | `.cursor/mcp.json`, `.agents/skills/`, `.cursor/agents/NAME.md` | MCP elicitation (per Cursor's docs) | Unverified |
| ACP editors (Zed, JetBrains, Neovim, Emacs) | `saige acp --agent NAME` | none | `session/request_permission` | Conformance-tested against the ACP Go SDK client and live against a real model; not run inside an editor |

`saige export skills --agent NAME` writes only the skills, to `.agents/skills/`, `.claude/skills/` and `.kiro/skills/`, which together cover every harness above and Kiro.

## saige export

```bash
saige export HARNESS --agent NAME [--dir .] [--dry-run] [--diff] [--user] [--force] [--mcp-command saige-mcp]
```

Export writes three things.

1. **An MCP server entry** named `saige-NAME` that runs `saige-mcp --agents-dir DIR --agent NAME --root PROJECT --tools none`. The harness gets one tool, named after the agent, that hands a task to the saige agent. `--tools none` is dropped when the definition names registry tools, so their packs load. Paths are absolute. `--mcp-command` sets the binary, for example a path outside `PATH`. Codex starts MCP servers with a small environment, so its entry forwards the provider credentials with `env_vars`.
2. **The definition's skills**, with only the six agentskills.io frontmatter fields: `name`, `description`, `license`, `compatibility`, `metadata` and `allowed-tools`. Other keys are dropped, since some consumers reject unknown keys. The metadata gains `generated-by: saige export`, so a later export replaces its own copy but never a skill someone else wrote there (`--force` overrides). A skill that already lives in the target directory is left alone. Resource files are copied, and scripts stay executable.
3. **The definition in the harness's own agent format**: the prompt is the body, and the description, tools, model and turn limit map where the format has a field. Each field the format cannot hold (dials, memory, guardrails, compaction, budget, sub-agents, registry tools, approval rules, a model from another vendor) is printed as a note. The saige agent behind the MCP tool still has all of them.

| Definition field | Claude Code | Codex | opencode | Gemini CLI | Cursor |
|---|---|---|---|---|---|
| `tools.harness` | `tools: Read, Grep, Glob, Write, Edit, Bash, WebFetch` | `sandbox_mode` | `permission.edit/bash/webfetch` | `tools` | `readonly` |
| `tools.mcp` | `mcp__SERVER` or `mcp__SERVER__TOOL` | note | note | note | note |
| `model` | the model ID when Anthropic | `model` when OpenAI | `provider/model` | the model ID when Google | note |
| `skills` | `skills` | read from `.agents/skills` | read from `.agents/skills` | read from `.agents/skills` | note |
| `limits.max_iterations` | `maxTurns` | note | `steps` | `max_turns` | note |

Existing files are merged, never replaced. A JSON file keeps its other keys, their order and its indentation; only the `saige-NAME` entry changes. In Codex's `config.toml`, only the `[mcp_servers.saige-NAME]` table (and its sub-tables) is replaced, and comments and every other line stay as they were. A file that cannot be merged safely, such as malformed JSON or a `config.toml` that sets `mcp_servers` with a top-level dotted key, is refused with exit status 2. Running export twice changes nothing.

`--dry-run` prints each file as `would create` or `would update` with its diff and writes nothing. `--diff` prints diffs on a real run. `--user` writes the harness's user-level files under your home directory instead (`~/.claude.json`, `~/.codex/config.toml`, `~/.gemini/settings.json`, `~/.config/opencode/opencode.json`, `~/.cursor/mcp.json` and the matching skill and agent directories) and always prints the diff. Without `--user`, export never touches user-level configuration.

Notes per harness:

- **Claude Code** asks before it uses a project's `.mcp.json` servers in an interactive session. In print mode, pass `--mcp-config .mcp.json` or use `saige launch claude`.
- **Codex** reads a project's `.codex/config.toml` only once you trust the project. `saige launch codex` needs no trust, since it passes the server with `-c`.
- **opencode** searches for `opencode.json` up to the git root. A `.jsonc` file with comments cannot be merged; add the entry by hand or rename the file.

## saige launch

```bash
saige launch HARNESS --agent NAME [--dir .] [--dry-run] [--bin PATH] [--mcp-command saige-mcp] [-- HARNESS-ARGS...]
```

Launch starts the harness with the saige agent available and passes everything after `--` to it. It configures the harness through flags or the environment where the harness supports that, and writes project files only for what the harness reads from files alone. It prints every file it writes, with the diff, before starting the harness. It never writes user-level configuration. The harness's exit status becomes saige's.

```bash
saige launch claude --agent reviewer -- -p "review the last commit" --allowedTools mcp__saige-reviewer__reviewer
saige launch codex --agent reviewer -- exec "review the last commit"
saige launch opencode --agent reviewer -- run "review the last commit"
saige launch gemini --agent reviewer --dry-run
```

## saige acp

```bash
saige acp --agent NAME [--agents-dir DIR] [--workspace DIR] [--sessions-dir DIR] [--approval-timeout 10m]
```

`saige acp` speaks ACP protocol version 1 as newline-delimited JSON-RPC on stdin and stdout. Logs go to stderr. Configure your editor to start it, for example in Zed's `agent_servers`:

```json
{
  "agent_servers": {
    "saige": { "type": "custom", "command": "saige", "args": ["acp", "--agent", "reviewer"] }
  }
}
```

| ACP | saige |
|---|---|
| `initialize` | Protocol 1. Prompts take images, audio and embedded resources. MCP servers over stdio and HTTP. Sessions can be closed and resumed, and with `--sessions-dir` loaded and listed. One auth method, `env`: saige uses the provider credentials in its environment, and `authenticate` fails when there are none. |
| `session/new` | Binds the starting definition. The session's `cwd` is the workspace of its harness tools unless `--workspace` is set. The client's MCP servers join the host's `--mcp-config` servers, and are connected only if the definition names them under `tools.mcp`, narrowed by `allow`. A host server of the same name wins. SSE servers are skipped. |
| Config options | `agent` (every definition) and `model` (the definition's own, or any catalog preset). Changing the model keeps the conversation. Changing the agent starts a new conversation, since the old one carries the other agent's system prompt. Modes are not used: ACP v2 removes them. |
| `session/prompt` | Text, image, audio, embedded resources (text becomes a text part headed by the URI, a blob becomes a media part) and resource links (named in text so the agent reads them with its own tools; an http(s) media link becomes a media part) map to typed parts. Text streams as `agent_message_chunk`, thinking as `agent_thought_chunk`, tool calls as `tool_call` and `tool_call_update` with kind, title, location, raw input and output, the result, and a diff for `write_file` and `edit_file`. The stop reason is `end_turn`, `cancelled`, `max_turn_requests`, `max_tokens` or `refusal`. |
| Approvals | Each approval marker becomes `session/request_permission` with Allow, Reject and, when allowed, "Always allow". Always allow grants the tool for the rest of the session. It is offered only when the definition's `approval.grant` reaches the `tool` scope and the tool is not destructive, since grants never cover destructive tools. A request unanswered after `--approval-timeout` is denied. |
| `session/cancel` | Stops the run, ends open permission requests, and answers the prompt with `cancelled`. |
| `session/load`, `session/list` | With `--sessions-dir`, each session's conversation is saved after every turn. Load rebinds it under its ID and replays it as user and agent message chunks; resume does the same without the replay. Without the flag nothing is saved. |

saige uses its own harness tools for files and commands, not the client's `fs` and `terminal` methods, which ACP v2 removes.

## Approvals in harnesses without elicitation

When a saige agent behind `saige-mcp` wants to run a tool that needs approval, saige-mcp normally asks you through MCP elicitation. opencode and Zed's MCP client have no elicitation, so with the default `--approval elicit` saige-mcp **holds** the call instead:

1. The agent's run pauses before the tool. The agent tool returns an approval-required result (text and structured content) that names the tool, its arguments, a token, the two commands to decide it, and the resume tool. The text tells the model not to decide and to ask you.
2. You decide in a terminal:

   ```bash
   saige approvals list
   saige approvals approve apr_3590715aa3f6b9340ec82e421c09188f [--grant tool]
   saige approvals deny apr_3590715aa3f6b9340ec82e421c09188f --message "use the staging bucket"
   ```

3. The model calls `NAME_resume` with the token. The run continues with your decision, to its answer or to the next approval. A resume before you decide waits up to `--approval-wait` (30 seconds), then reports the approval still pending.

A grant (`--grant tool` or `session`) covers later calls of the same agent run, within the definition's `approval.grant`. A held call nobody decides within `--approval-timeout` (15 minutes) ends its run. Held approvals are files in `$SAIGE_APPROVALS_DIR`, or `~/.local/state/saige/approvals`, readable only by you. A decision is recorded once, so a second decision on the same token fails.

The held run lives in the saige-mcp process. It survives only while the harness keeps that process running: an interactive opencode session, or `opencode serve` with `opencode run --attach`. A plain `opencode run` starts a new MCP server each time, so a resume in a later run reports the token as unknown. A model with a shell tool could run `saige approvals` itself. Your harness's own shell permission prompt is what stops that, so keep it on for commands you have not allowed.

`--approval host` and `--approval deny` behave as before: host refuses calls the agent makes, because the client's prompt covered only the agent tool, and deny refuses every marked call.

## Shared sessions

`saige serve`, `saige acp` and saige-mcp's agent tool use one session layer (`cmd/internal/agenthost`). A session:

- owns one agent and its conversation, and runs one turn at a time;
- binds the definition when it starts and keeps the pinned version, even when the definitions reload;
- checks every decision's grant: well formed, not expired, and within the definition's `approval.grant`;
- gets an approval policy even when the definition declares none, so the grants a host accepts take effect;
- releases what binding opened (MCP connections, the harness workspace) when it ends.

Memory stores and durable runners reach a session through the same binding environment, so a host that configures them applies them to every session. None of the CLI hosts configures one yet.
