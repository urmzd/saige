package tree_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
)

func TestPrintDocumentPreservesTreeAndMetadata(t *testing.T) {
	metadata := json.RawMessage(`{"spec":{"sample":2},"config":{"model":"example"},"large_id":9007199254740993}`)
	conversation, err := tree.New(types.NewSystemMessage("system"), tree.WithMetadata(metadata))
	if err != nil {
		t.Fatal(err)
	}
	metadata[2] = 'X'
	ctx := context.Background()
	user, err := conversation.AddChild(ctx, conversation.Root().ID, types.NewUserMessage("read"))
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := conversation.AddChild(ctx, user.ID, types.AssistantMessage{Content: []types.AssistantContent{
		types.ThinkingContent{Thinking: "visible reasoning", Signature: "signature"},
		types.ToolUseContent{ID: "call1", Name: "read", Arguments: map[string]any{"id": json.Number("9007199254740993"), "nullable": nil}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conversation.AddChild(ctx, assistant.ID, types.NewToolResultMessage(types.ToolResultContent{ToolCallID: "call1", Text: `{"result":null}`})); err != nil {
		t.Fatal(err)
	}
	_, alternate, err := conversation.Branch(ctx, user.ID, "alternate", types.NewAssistantMessage("alternative"))
	if err != nil {
		t.Fatal(err)
	}
	if err := conversation.Archive(alternate.ID, "test", false); err != nil {
		t.Fatal(err)
	}
	if _, err := conversation.Checkpoint(conversation.Active(), "saved"); err != nil {
		t.Fatal(err)
	}
	var output, again bytes.Buffer
	if err := tree.Print(&output, conversation); err != nil {
		t.Fatal(err)
	}
	if err := tree.Print(&again, conversation); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), again.Bytes()) {
		t.Fatal("unstable print output")
	}
	var document struct {
		Content     []json.RawMessage          `json:"content"`
		Metadata    json.RawMessage            `json:"metadata"`
		Checkpoints map[string]json.RawMessage `json:"checkpoints"`
	}
	if err := json.Unmarshal(output.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Content) != 5 || len(document.Checkpoints) != 1 || !bytes.Contains(document.Metadata, []byte(`"spec"`)) || bytes.Contains(document.Metadata, []byte(`"sXec"`)) {
		t.Fatal("lost tree data or copied metadata")
	}
	whole, err := json.Marshal(conversation)
	if err != nil {
		t.Fatal(err)
	}
	var original struct {
		Nodes []json.RawMessage `json:"nodes"`
	}
	if err := json.Unmarshal(whole, &original); err != nil {
		t.Fatal(err)
	}
	byID := map[string]json.RawMessage{}
	for _, raw := range original.Nodes {
		var node struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &node); err != nil {
			t.Fatal(err)
		}
		byID[node.ID] = raw
	}
	seen := map[string]bool{}
	for _, raw := range document.Content {
		var node struct {
			ID      string `json:"id"`
			Parent  string `json:"parent_id"`
			Created string `json:"created_at"`
			Updated string `json:"updated_at"`
		}
		if err := json.Unmarshal(raw, &node); err != nil {
			t.Fatal(err)
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, raw); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(compact.Bytes(), byID[node.ID]) || node.Created == "" || node.Updated == "" {
			t.Fatal("changed native node fields")
		}
		if node.Parent != "" && !seen[node.Parent] {
			t.Fatal("child preceded parent")
		}
		seen[node.ID] = true
	}
	for _, source := range [][]byte{whole, output.Bytes()} {
		var restored tree.Tree
		if err := json.Unmarshal(source, &restored); err != nil {
			t.Fatal(err)
		}
		again.Reset()
		if err := tree.Print(&again, &restored); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(output.Bytes(), again.Bytes()) {
			t.Fatal("tree round trip lost data")
		}
	}
}

type printWriter func([]byte) (int, error)

func (w printWriter) Write(p []byte) (int, error) { return w(p) }

func TestPrintFailureAndWriterReentry(t *testing.T) {
	conversation, err := tree.New(types.NewSystemMessage("system"))
	if err != nil {
		t.Fatal(err)
	}
	if err := tree.Print(printWriter(func(p []byte) (int, error) { return len(p) - 1, nil }), conversation); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal("short write ignored", err)
	}
	failure := errors.New("disk full")
	if err := tree.Print(printWriter(func([]byte) (int, error) { return 0, failure }), conversation); !errors.Is(err, failure) {
		t.Fatal("write error lost", err)
	}
	if err := tree.Print(printWriter(func(p []byte) (int, error) {
		if err := conversation.SetActive(conversation.Active()); err != nil {
			return 0, err
		}
		return len(p), nil
	}), conversation); err != nil {
		t.Fatal(err)
	}
	if tree.Print(io.Discard, nil) == nil || tree.Print(nil, conversation) == nil {
		t.Fatal("nil input accepted")
	}
	if _, err := tree.MarshalNode(nil); err == nil {
		t.Fatal("nil node accepted")
	}
	for _, metadata := range []string{`null`, `[]`, `invalid`} {
		if _, err := tree.New(types.NewSystemMessage("system"), tree.WithMetadata(json.RawMessage(metadata))); err == nil {
			t.Fatal("invalid metadata accepted", metadata)
		}
	}
}
