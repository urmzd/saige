package google

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestChatStreamWithOptions(t *testing.T) {
	temp, parallel := 0.5, false
	for _, tc := range []struct {
		name       string
		opts       types.RequestOptions
		wantChoice string
		wantTemp   any
		wantErr    bool
	}{
		{name: "required for one call", opts: types.RequestOptions{ToolChoice: &types.ToolChoice{Mode: types.ToolChoiceRequired}}, wantChoice: "map[mode:ANY]"},
		{name: "named", opts: types.RequestOptions{ToolChoice: &types.ToolChoice{Mode: types.ToolChoiceNamed, Name: "lookup"}},
			wantChoice: "map[allowedFunctionNames:[lookup] mode:ANY]"},
		{name: "none", opts: types.RequestOptions{ToolChoice: &types.ToolChoice{Mode: types.ToolChoiceNone}}, wantChoice: "map[mode:NONE]"},
		{name: "temperature", opts: types.RequestOptions{Temperature: &temp}, wantTemp: temp},
		{name: "parallel control cannot be sent", opts: types.RequestOptions{ParallelTools: &parallel}, wantErr: true},
		{name: "unknown tool", opts: types.RequestOptions{ToolChoice: &types.ToolChoice{Mode: types.ToolChoiceNamed, Name: "missing"}}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var bodies []map[string]any
			a, err := New(context.Background(), Config{APIKey: "k", Model: "gemini-2.5-flash"}, WithHTTPClient(&http.Client{Transport: captureTransport{events: []string{doneEvent}, bodies: &bodies}}))
			if err != nil {
				t.Fatal(err)
			}
			ch, err := a.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("go"))}, Tools: testTools, Options: new(tc.opts)})
			if tc.wantErr {
				if !errors.Is(err, types.ErrInvalidModelConfig) || len(bodies) != 0 {
					t.Fatalf("err = %v with %d requests, want a local rejection", err, len(bodies))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for range ch {
			}
			if tc.wantChoice != "" {
				cfg, _ := bodies[0]["toolConfig"].(map[string]any)
				if got := fmt.Sprint(cfg["functionCallingConfig"]); got != tc.wantChoice {
					t.Fatalf("functionCallingConfig = %s, want %s", got, tc.wantChoice)
				}
			}
			if tc.wantTemp != nil {
				gen, _ := bodies[0]["generationConfig"].(map[string]any)
				if gen["temperature"] != tc.wantTemp {
					t.Fatalf("temperature = %v, want %v", gen["temperature"], tc.wantTemp)
				}
			}
			if a.toolChoice != nil {
				t.Fatal("per-call options leaked into the adapter")
			}
		})
	}
}
