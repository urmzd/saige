package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"

	sdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/urmzd/saige/agent/provider/retry"
	"github.com/urmzd/saige/agent/types"
)

type requestTransport func(*http.Request) (*http.Response, error)

func (f requestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestInvalidOptionsRejectBeforeHTTP(t *testing.T) {
	for _, tc := range []struct {
		name, model string
		opts        []Option
	}{
		{"temperature zero on required reasoning", "o3", []Option{WithTemperature(0)}},
		{"reasoning on chat model", "gpt-4o", []Option{WithReasoningEffort("high")}},
		{"empty effort", "o3", []Option{WithReasoningEffort("")}},
		{"invalid effort", "o3", []Option{WithReasoningEffort("minimal")}},
		{"required reasoning disabled", "gpt-5", []Option{WithReasoningEffort("none")}},
		{"conditional temperature", "gpt-5.2", []Option{WithReasoningEffort("high"), WithTemperature(0)}},
		{"conditional top p", "gpt-5.1", []Option{WithReasoningEffort("low"), WithTopP(1)}},
		{"NaN", "gpt-4o", []Option{WithTemperature(math.NaN())}},
		{"negative limit", "o3", []Option{WithMaxTokens(-1)}},
		{"oversized limit", "o3", []Option{WithMaxTokens(100001)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := NewAdapter("test", tc.model, tc.opts...)
			calls := 0
			a.client = sdk.NewClient(option.WithHTTPClient(&http.Client{Transport: requestTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				return nil, errors.New("unexpected network request")
			})}))
			for _, schema := range []*types.ParameterSchema{nil, {Type: "object"}} {
				ch, err := a.ChatStreamWithSchema(context.Background(), nil, nil, schema)
				if ch != nil || !errors.Is(err, types.ErrInvalidModelConfig) || types.IsTransient(err) {
					t.Fatalf("channel=%v error=%v, want permanent configuration rejection", ch, err)
				}
			}
			if calls != 0 {
				t.Fatalf("sent %d invalid requests", calls)
			}
		})
	}
}

func TestAcceptedSettingsReachWire(t *testing.T) {
	for _, tc := range []struct {
		model  string
		opts   []Option
		want   map[string]any
		absent []string
	}{
		{"gpt-4o", []Option{WithTemperature(0), WithSeed(0), WithParallelToolCalls(false)}, map[string]any{"temperature": float64(0), "seed": float64(0), "parallel_tool_calls": false}, []string{"reasoning_effort"}},
		{"o3", []Option{WithReasoningEffort("high"), WithMaxTokens(2048)}, map[string]any{"reasoning_effort": "high", "max_completion_tokens": float64(2048)}, []string{"temperature", "max_tokens"}},
		{"gpt-5.2", []Option{WithReasoningEffort("none"), WithTemperature(0)}, map[string]any{"reasoning_effort": "none", "temperature": float64(0)}, nil},
		{"gpt-5.1", []Option{WithTemperature(0)}, map[string]any{"temperature": float64(0)}, []string{"reasoning_effort"}},
		{"o3", nil, nil, []string{"reasoning_effort", "temperature"}},
		{"gpt-5.2", []Option{WithReasoningEffort("none"), WithTemperature(0), WithPromptCache("scope", "in_memory")}, map[string]any{"temperature": float64(0), "prompt_cache_key": "scope", "prompt_cache_retention": "in_memory"}, nil},
	} {
		t.Run(tc.model, func(t *testing.T) {
			a := NewAdapter("test", tc.model, tc.opts...)
			calls := 0
			a.client = sdk.NewClient(option.WithHTTPClient(&http.Client{Transport: requestTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				for k, v := range tc.want {
					if body[k] != v {
						t.Errorf("%s=%v, want %v", k, body[k], v)
					}
				}
				for _, k := range tc.absent {
					if _, ok := body[k]; ok {
						t.Errorf("unexpected %s", k)
					}
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: [DONE]\n\n")), Request: r}, nil
			})}))
			ch, err := a.ChatStream(context.Background(), []types.Message{types.NewUserMessage("hello")}, nil)
			if err != nil {
				t.Fatal(err)
			}
			for d := range ch {
				if e, ok := d.(types.ErrorDelta); ok {
					t.Fatal(e.Error)
				}
			}
			if calls != 1 {
				t.Fatalf("requests=%d, want 1", calls)
			}
		})
	}
}

func TestModelSwitchAndRetryCannotHideInvalidSettings(t *testing.T) {
	a := NewAdapter("test", "gpt-4o", WithTemperature(0))
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
	switched := a.WithModel("o3")
	wrapped := retry.New(switched, retry.Config{MaxAttempts: 2})
	_, err := wrapped.ChatStream(context.Background(), nil, nil)
	if !errors.Is(err, types.ErrInvalidModelConfig) {
		t.Fatalf("got %v", err)
	}
	if err := a.Validate(); err != nil {
		t.Fatalf("original adapter mutated: %v", err)
	}
}
