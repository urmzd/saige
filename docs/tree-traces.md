# Print a conversation tree

Use `tree.Print` to write one JSON document with the actual conversation nodes in `content`.
The tree also retains metadata, branch links, active-branch selection, and checkpoints.
Each content entry uses the same node schema as `json.Marshal(conversation)`.

```go
metadata, err := json.Marshal(map[string]any{
    "spec": runSpecification,
    "revision": runRevision,
    "config": effectiveConfiguration,
})
if err != nil {
    return err
}
conversation, err := tree.New(
    types.NewSystemMessage(systemPrompt),
    tree.WithMetadata(metadata),
)
if err != nil {
    return err
}
// Attach conversation to the agent with agent.WithTree(conversation).
file, err := os.Create("agent.json")
if err != nil {
    return err
}
defer file.Close()
if err := tree.Print(file, conversation); err != nil {
    return err
}
return file.Sync()
```

Each node retains these fields:

- Its ID, parent ID, branch ID, depth, state, and version.
- Its role and typed message content, including tool calls and results.
- Its creation and update timestamps.
- Its archive metadata and summary references, when present.

The message content keeps the SDK schema, including its field names and type tags.
Tool results keep their internal system role. The printer does not invent a tool role.
Binary attachments follow the existing serializer rules: references remain, but byte buffers do not.

The printer includes all branches and node states. It orders nodes by depth, creation time, then ID.
Parents precede their children. The same snapshot produces the same bytes.
The printer takes a read lock for the snapshot, then releases it before file output.
An output error can leave a partial file. The caller owns file creation, sync, close, and atomic replacement.

Use `tree.MarshalNode(node)` when a WAL or event sink receives individual node revisions.
It uses the same serializer as `tree.Print`. The caller must prevent concurrent changes to that node.
Each revision retains its original timestamps. A later revision has the same ID and its updated version.

Metadata is an immutable JSON object. The tree takes its own copy.
Include the run specification and configuration, but exclude credentials and HTTP headers.
Both `Print` and `MarshalJSON` preserve metadata. `UnmarshalJSON` accepts the printed `content` array and the existing `nodes` format.
The existing JSON serializer still uses `nodes`, so existing consumers retain their write format.
Provider requests, token usage, retries, and partial stream fragments are separate execution events.
