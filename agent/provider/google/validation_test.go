package google

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/types"
	"google.golang.org/genai"
)

func TestReasoningValidation(t *testing.T) {
	for _, tc := range []struct {
		name, model string
		opt         Option
		valid       bool
	}{
		{"pro required", "gemini-2.5-pro", WithoutThinking(), false},
		{"pro minimum", "gemini-2.5-pro", WithThinkingBudget(128), true},
		{"pro below minimum", "gemini-2.5-pro", WithThinkingBudget(127), false},
		{"pro maximum", "gemini-2.5-pro", WithThinkingBudget(32768), true},
		{"pro over maximum", "gemini-2.5-pro", WithThinkingBudget(32769), false},
		{"dynamic", "gemini-2.5-pro", WithThinkingBudget(-1), true},
		{"invalid negative", "gemini-2.5-pro", WithThinkingBudget(-2), false},
		{"flash disabled", "gemini-2.5-flash", WithoutThinking(), true},
		{"lite too small", "gemini-2.5-flash-lite", WithThinkingBudget(511), false},
		{"lite disabled", "gemini-2.5-flash-lite", WithoutThinking(), true},
		{"wrong control", "gemini-2.5-pro", WithThinkingLevel(genai.ThinkingLevelHigh), false},
		{"wrong control generation 3", "gemini-3.1-pro", WithThinkingBudget(128), false},
		{"required generation 3", "gemini-3.1-pro", WithoutThinking(), false},
		{"medium", "gemini-3.1-pro", WithThinkingLevel(genai.ThinkingLevelMedium), true},
		{"invalid enum", "gemini-3.1-pro", WithThinkingLevel("maximum"), false},
		{"empty enum", "gemini-3.1-pro", WithThinkingLevel(""), false},
		{"legacy", "gemini-2.0-flash", WithThinkingBudget(1024), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A nil SDK client proves rejection precedes SDK use on both paths.
			a := &Adapter{model: tc.model}
			tc.opt(a)
			err := a.Validate()
			if (err == nil) != tc.valid {
				t.Fatalf("Validate=%v valid=%v", err, tc.valid)
			}
			if !tc.valid {
				_, err := a.ChatStream(context.Background(), nil, nil)
				if !errors.Is(err, types.ErrInvalidModelConfig) {
					t.Fatalf("plain path: %v", err)
				}
				_, err = a.ChatStreamWithSchema(context.Background(), nil, nil, &types.ParameterSchema{Type: "object"})
				if !errors.Is(err, types.ErrInvalidModelConfig) {
					t.Fatalf("schema path: %v", err)
				}
			}
		})
	}
}

func TestDynamicThinkingAndModelSwitch(t *testing.T) {
	a := &Adapter{model: "gemini-2.5-flash"}
	WithThinkingBudget(-1)(a)
	_, config := a.buildRequest(nil, nil)
	if !config.ThinkingConfig.IncludeThoughts || *config.ThinkingConfig.ThinkingBudget != -1 {
		t.Fatal("dynamic thinking lost")
	}
	WithoutThinking()(a)
	if err := a.WithModel("gemini-2.5-pro").(*Adapter).Validate(); !errors.Is(err, types.ErrInvalidModelConfig) {
		t.Fatalf("model switch: %v", err)
	}
	if err := a.Validate(); err != nil {
		t.Fatalf("original changed: %v", err)
	}
}

func TestValidationPreservesContextCacheBinding(t *testing.T) {
	a := &Adapter{model: "gemini-2.5-flash"}
	WithThinkingBudget(128)(a)
	messages := []types.Message{types.NewUserMessage("cached reference"), types.NewUserMessage("question")}
	WithContextCache(ContextCache{Name: "cachedContents/test", Model: a.model, PrefixCount: 1, ExpiresAt: time.Now().Add(-time.Hour)})(a)
	for _, schema := range []*types.ParameterSchema{nil, {Type: "object"}} {
		// A nil client proves both entry points reject expiry before SDK use.
		stream, err := a.ChatStreamWithSchema(context.Background(), messages, nil, schema)
		if stream != nil || err == nil || !strings.Contains(err.Error(), "expired") {
			t.Fatalf("stream=%v err=%v", stream, err)
		}
	}
	stream, err := a.ChatStream(context.Background(), messages, nil)
	if stream != nil || err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("plain stream=%v err=%v", stream, err)
	}
}
