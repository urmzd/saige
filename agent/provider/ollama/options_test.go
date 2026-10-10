package ollama

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestChatStreamWithOptions(t *testing.T) {
	temp := 0.2
	for _, tc := range []struct {
		name    string
		opts    types.RequestOptions
		want    []string
		wantErr bool
	}{
		{name: "no options sends every tool", want: []string{"lookup", "write"}},
		{name: "named for one call", opts: types.RequestOptions{ToolChoice: &types.ToolChoice{Mode: types.ToolChoiceNamed, Name: "lookup"}}, want: []string{"lookup"}},
		{name: "none for one call", opts: types.RequestOptions{ToolChoice: &types.ToolChoice{Mode: types.ToolChoiceNone}}},
		{name: "required is rejected", opts: types.RequestOptions{ToolChoice: &types.ToolChoice{Mode: types.ToolChoiceRequired}}, wantErr: true},
		{name: "sampling options are rejected", opts: types.RequestOptions{Temperature: &temp}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, got := captureChat(t, ChatChunk{Done: true})
			a := NewAdapter(NewClient(server.URL, "qwen3:4b", ""))
			ch, err := a.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("go"))}, Tools: testTools, Options: new(tc.opts)})
			if tc.wantErr {
				if !errors.Is(err, types.ErrInvalidModelConfig) {
					t.Fatalf("err = %v, want a local configuration error", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for range ch {
			}
			var names []string
			for _, tool := range got.Tools {
				names = append(names, tool.Function.Name)
			}
			if !slices.Equal(names, tc.want) {
				t.Fatalf("tools sent = %v, want %v", names, tc.want)
			}
			if a.toolChoice != nil {
				t.Fatal("per-call options leaked into the adapter")
			}
		})
	}
}
