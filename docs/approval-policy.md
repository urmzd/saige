# Approval policy and grants

Gates and markers decide which tool calls need a human decision.
An `agent.ApprovalPolicy` decides whether a person must be asked again, based on the decisions already made in the conversation.

```go
a, err := agent.New(cfg, agent.WithApprovalPolicy(agent.ApprovalPolicy{
    RiskDefaults: true, // read runs, write and unknown ask, destructive always asks
    DenyAfter:    3,    // stop asking about a tool after three denials
    HideDenied:   false,
    RampAfter:    0,    // opt in to auto-approve writes after k approvals
}))
if err != nil {
    return err
}
```

Without a policy every held call is asked about, as before.

## Grants

A host attaches a scope to an approval with `Resolution.Grant`, or `ApprovalDecision.Grant` for a durable engine's `Decide` and for `InterruptReply`:

```go
stream.ResolveMarkerErr(m.ToolCallID, agent.Resolution{
    Approved: true,
    Approver: "user:ada",
    Grant: &types.GrantRequest{
        Scope:     types.GrantArgs,
        Match:     []types.ArgMatch{{Field: "path", PathPrefix: "/srv/app"}},
        ExpiresAt: time.Now().Add(time.Hour),
    },
})
```

| Scope | Later calls approved without asking |
|---|---|
| `once` | None. Same as no grant |
| `tool` | Every call of the same tool |
| `args` | Calls of the same tool whose arguments match every matcher |
| `session` | Every held call of any tool |

A matcher names a field (dots step into nested objects) and one condition: `Equals`, `Prefix`, or `PathPrefix`.
`PathPrefix` cleans both paths and matches whole segments, so `/srv/app/../etc` and `/srv/application` do not match `/srv/app`.
`ExpiresAt` ends a grant. Without it the grant lasts for the conversation.
`ResolveMarkerErr` rejects an invalid request with `types.ErrInvalidGrant`.
A grant on a refusal is ignored.

## Denials, risk defaults, and the ramp

| Setting | Effect |
|---|---|
| `DenyAfter: n` | After n denials of one tool, later calls are refused with the reason, without asking |
| `HideDenied` | The tool is removed from the model's tool list instead, after the `ToolPolicy` |
| `RiskDefaults` | Adds a capability gate: read allows, write and unknown ask, destructive asks. It composes with `ToolGate`, most restrictive wins |
| `RampAfter: k` | A write-class tool approved k times runs without asking |

Grants and the ramp never cover a destructive tool. Every destructive call is asked about.

## Records and replay

Each decision is recorded as `types.ApprovalPart` in the tool result message of its call: `approved`, `granted` (with the `types.Grant`), `denied`, `auto_approved` (with the grant ID, or the ramp reason), and `auto_denied`.
Records are metadata, stripped before the provider call, and persisted with the tree.
At the start of each run the policy rebuilds its grants and counts from the records on the branch, so a restored conversation decides the same way. `agent.Grants(messages)` lists them.

Compaction keeps only what the model sees, so it would drop the records. When compaction moves a run to a new branch, the loop writes the whole state onto it as one `snapshot` record, which replaces any state before it. Grants and denial counts therefore survive compaction.

Records are written with the tool results of their turn. A turn that ends before its results are persisted, such as one cancelled mid-batch, loses that turn's records: its grants are not kept and its denials are not counted. Losing a grant only means the next call asks again.

Under a durable runner the policy's verdict on each call is a recorded step, `approval-<phase>-<callID>`.
A replay reads the verdict instead of deciding again, so it reaches the same result after a grant expired or the clock moved.
The decision that created a grant is saved by the engine with the rest of the approval, so the grant is recreated on replay.

`RunContext.Approval` and `types.CallApprovalFrom(ctx)` tell a tool how its call was cleared: the approver, or the ID of the grant that approved it.

## Who can grant

Only the host creates grants, from the decisions it delivers through `Resolution`, `InterruptReply`, or a durable engine's `Decide`.
The model's text and tool arguments, tool and skill output, and gates cannot create one.
Records are read only from system messages, which only the agent loop writes.
State is per conversation. A sub-agent inherits the policy but starts with no grants.

`saige serve` configures every session with a policy. A client attaches a grant to an approval in the interrupt body, and `--deny-after` sets the denial limit. See the [CLI reference](../cmd/saige/README.md).

See [`examples/agent/approval-grants`](../examples/agent/approval-grants/).
