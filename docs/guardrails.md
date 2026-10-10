# Guardrails

Guardrails check the two edges of a conversation: the user's message on the way in and the final answer on the way out.
Each one returns a verdict:

| Verdict | Effect |
|---|---|
| `agent.Pass()` | The text goes through unchanged |
| `agent.Block(reason)` | A tripwire: the run stops with `*agent.GuardrailTrippedError` |
| `agent.Rewrite(text, reason)` | The text is replaced, for example redacted, and the run goes on |

```go
a := agent.NewAgent(cfg,
    agent.WithInputGuardrails(
        agent.InputGuardrail{Guardrail: guardrail.PII(true)},   // redact personal data first
        agent.InputGuardrail{Guardrail: guardrail.MaxLength(8000)},
        agent.InputGuardrail{
            Guardrail: guardrail.Classifier("support-only", cheapModel,
                "Only questions about our product are allowed."),
            Mode: agent.GuardrailParallel,
        },
    ),
    agent.WithOutputGuardrails(
        agent.OutputGuardrail{Guardrail: guardrail.PII(true)},
    ),
)
```

Write your own with `agent.NewGuardrail(name, func(ctx, in agent.GuardrailInput) (agent.GuardrailVerdict, error))`. The input carries the text, the phase (`input` or `output`), the branch before the message, and the run's identity. A guardrail error fails closed: the run stops as if it blocked.

## Input guardrails

Input guardrails check every user message that enters a run: its input, and each message submitted while it runs. They run after the `UserInput` hooks, in order. Each sees the previous one's rewrite, and the first block stops the run.

`GuardrailSequential` (the default) checks the message before it is appended. A blocked message is never recorded or sent. A rewritten one is recorded and sent in its rewritten form, so redaction belongs here.

`GuardrailParallel` starts the run's first model call at the same time and saves the guardrail's latency. When the guardrail blocks:

- a call still in flight is cancelled, and the block is recorded with `Canceled: true`;
- a call that already finished is discarded;
- either way the call's turn is not recorded and its usage is still charged.

A parallel guardrail cannot rewrite, because the model already has the text: a rewrite counts as a block. Messages submitted during a run are always checked before they are appended. Under a durable runner that keeps steps on one goroutine, parallel guardrails also run before the call.

## Output guardrails

Output guardrails check the final answer before it is recorded: a turn without tool calls that ends the user turn, the forced final answer of `MaxIterForceFinal`, or a text turn the output limit cut short when no automatic continuation follows.

The answer's text deltas have already streamed when the check runs. A block stops the run and the answer is not recorded. A rewrite records the rewritten answer and sends a `types.GuardrailDelta` with the replacement text, so a consumer can replace what it showed; `agent.Collect` does this for `Transcript.Text`. To keep raw model output from a consumer entirely, put `privacy.Provider` in front of the model.

An answer delivered through a stop tool, or the `final_answer` tool of tool output mode, is a tool result. Check it with an `AfterTool` hook.

## Tripwires

A block surfaces in three places:

- the run's error, a `*agent.GuardrailTrippedError` that matches `agent.ErrGuardrailTripped` and names the guardrail, the phase, the reason, and whether a model call was cancelled. It crosses the wire with the code `guardrail_tripped`;
- a `types.GuardrailDelta` with action `block`, sent before the error;
- a `types.GuardrailContent` record in the tree, in a system message of its own. A rewrite is recorded too, as `GuardrailContent` attached to the message it rewrote. Records are metadata and never reach the model.

`RunStop` hooks see the reason `guardrail`.

## Built-ins

Package `agent/guardrail`:

| Guardrail | Checks |
|---|---|
| `guardrail.PII(redact)` | Personal data with `privacy.DefaultDetector`: blocks, or rewrites each value to `[REDACTED:LABEL]` |
| `guardrail.Detect(name, detector, redact)` | The same with any `privacy.Detector`, such as a chain with a named-entity model |
| `guardrail.Regex(name, redact, patterns...)` | The same with patterns compiled into a `privacy.RegexDetector` |
| `guardrail.MaxLength(n)` | Blocks text longer than `n` characters |
| `guardrail.JSONSchema(schema)` | Blocks text that is not JSON matching the schema |
| `guardrail.Structured(name, check)` | Blocks text that is not JSON, or that `check` rejects |
| `guardrail.Classifier(name, provider, policy)` | Asks a model whether the text follows a policy written in plain language |

The classifier sends the text as data inside a tag the text cannot close, and expects `ALLOW` or `BLOCK: <reason>`; any other reply is an error, which blocks. Use a small, cheap model: it runs on every checked message.

## Budget

A guardrail that calls a model sends the call through `GuardrailInput.Metered(provider)`, as the classifier does. The run's budget then admits the call before it is sent and charges it after, like a turn, and its usage is reported as a `UsageDelta`. Without a budget the usage is still reported.

## Durable replay

Under a durable `StepRunner` each guardrail check is a recorded step, with its verdict, its rewrite, and the usage and budget receipts of its model calls. A replay applies the verdict without calling the guardrail and restores the receipts, so a classifier is neither called nor charged twice, and a guardrail that would now decide otherwise cannot change a recovered run.

## Sub-agents

A sub-agent inherits its parent's input and output guardrails. Its input is the delegated task and its output is the result its caller receives, so the same checks apply at every level. A child's trip fails the delegation, and the parent's model sees it as a tool error.

## How guardrails relate to gates and redaction

| Policy | Decides | Sees |
|---|---|---|
| Guardrail | Whether the conversation may go on with this message or answer, or in which form | The user's text and the final answer |
| `ToolGate` | Whether a tool call may run | The tool definition and the model's arguments |
| `ToolRedactor` and `privacy.Provider` | What each side of a boundary sees, reversibly | Tool traffic, or the whole provider request |

A guardrail never sees tool calls, and a gate never sees the user's message. Redaction through a vault is reversible and keeps tools working with real values; a guardrail rewrite is permanent and changes what is recorded.
