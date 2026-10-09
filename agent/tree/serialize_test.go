package tree

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestMessageRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		msg  types.Message
	}{
		{
			name: "tool call with an argument error",
			msg: types.AssistantMessage{Content: []types.AssistantContent{
				types.ToolUseContent{ID: "c1", Name: "write", ArgumentsError: "unexpected end of JSON input"},
			}},
		},
		{
			name: "truncated turn",
			msg: types.AssistantMessage{Content: []types.AssistantContent{
				types.TextContent{Text: "partial"}, types.TruncationContent{Reason: "max_tokens"},
			}},
		},
		{
			name: "server tool call and route",
			msg: types.AssistantMessage{Content: []types.AssistantContent{
				types.ServerToolContent{ID: "s1", Kind: types.ServerToolKind("web_search"), Name: "web_search", Text: "found"},
				types.RouteContent{Profile: "fast", Provider: "openai", Model: "gpt-4o", Experiment: "e", Variant: "b"},
			}},
		},
		{
			name: "steered user message",
			msg: types.UserMessage{Content: []types.UserContent{
				types.TextContent{Text: "also check tests"}, types.SteerContent{ID: "sub-1"},
			}},
		},
		{
			name: "system route",
			msg: types.SystemMessage{Content: []types.SystemContent{
				types.TextContent{Text: "s"}, types.RouteContent{Profile: "p"},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := MarshalMessage(tt.msg)
			if err != nil {
				t.Fatal(err)
			}
			got, err := UnmarshalMessage(tt.msg.Role(), raw)
			if err != nil {
				t.Fatalf("unmarshal %s: %v", raw, err)
			}
			if !reflect.DeepEqual(got, tt.msg) {
				t.Fatalf("round trip = %#v, want %#v", got, tt.msg)
			}
		})
	}
}

func TestUnmarshalJSONFailureLeavesTree(t *testing.T) {
	tr, err := New(types.NewSystemMessage("sys"))
	if err != nil {
		t.Fatal(err)
	}
	before, err := json.Marshal(tr)
	if err != nil {
		t.Fatal(err)
	}
	bad := `{"root_id":"x","active":"main","nodes":[{"id":"x","role":"assistant","message":{"content":[{"type":"nope","data":{}}]}}]}`
	if err := json.Unmarshal([]byte(bad), tr); err == nil {
		t.Fatal("unknown content type accepted")
	}
	after, err := json.Marshal(tr)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("failed restore changed the tree:\n%s\n%s", before, after)
	}
}

func TestUnmarshalJSONConcurrentReaders(t *testing.T) {
	src, err := New(types.NewSystemMessage("sys"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.AddChild(context.Background(), src.Root().ID, types.NewUserMessage("hi")); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(src)
	if err != nil {
		t.Fatal(err)
	}
	dst, err := New(types.NewSystemMessage("other"))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := json.Unmarshal(data, dst); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			_ = dst.Branches()
			_ = dst.Root()
		}()
	}
	wg.Wait()
}

func TestTreeFormatVersion(t *testing.T) {
	src, err := New(types.NewSystemMessage("sys"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(src)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if string(doc["v"]) != "1" {
		t.Fatalf("written version = %s, want 1", doc["v"])
	}

	withVersion := func(v string) []byte {
		out := make(map[string]json.RawMessage, len(doc))
		for k, val := range doc {
			out[k] = val
		}
		if v == "" {
			delete(out, "v")
		} else {
			out["v"] = json.RawMessage(v)
		}
		b, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	tests := []struct {
		name    string
		data    []byte
		wantErr error
	}{
		{name: "current version", data: withVersion("1")},
		{name: "no version reads as the first format", data: withVersion("")},
		{name: "future version", data: withVersion("2"), wantErr: ErrTreeFormatVersion},
		{name: "negative version", data: withVersion("-1"), wantErr: ErrTreeFormatVersion},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var dst Tree
			err := json.Unmarshal(tt.data, &dst)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if dst.rootID != src.rootID {
				t.Fatalf("root = %q, want %q", dst.rootID, src.rootID)
			}
		})
	}
}
