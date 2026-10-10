package anthropic

import (
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/urmzd/saige/agent/types"
)

// cacheRequest builds a request with two system blocks, two tools, and a
// conversation ending in the given message.
func cacheRequest(last types.Message) anthropic.MessageNewParams {
	msgs := []types.Message{
		types.SystemMsg(types.Text("first")),
		types.SystemMsg(types.Text("last")),
		types.UserMsg(types.Text("question")),
		types.AssistantMessage{Parts: []types.AssistantPart{types.ToolCallPart{ID: "c1", Name: "lookup", Arguments: map[string]any{}}}},
	}
	if last != nil {
		msgs = append(msgs, last)
	}
	system, aMsgs := toAnthropicParams(msgs)
	return anthropic.MessageNewParams{
		System:   system,
		Messages: aMsgs,
		Tools:    toAnthropicTools([]types.ToolDef{{Name: "lookup"}, {Name: "fetch"}}),
	}
}

// breakpoints reports which of the policy's locations carry a marker.
func breakpoints(p anthropic.MessageNewParams) (tools, system, conversation []string) {
	for i, t := range p.Tools {
		if cc := t.GetCacheControl(); cc != nil && cc.TTL != "" {
			tools = append(tools, string(cc.TTL)+"@"+string(rune('0'+i)))
		}
	}
	for i, b := range p.System {
		if b.CacheControl.TTL != "" {
			system = append(system, string(b.CacheControl.TTL)+"@"+string(rune('0'+i)))
		}
	}
	for mi, m := range p.Messages {
		for bi, b := range m.Content {
			if cc := b.GetCacheControl(); cc != nil && cc.TTL != "" {
				conversation = append(conversation, string(cc.TTL)+"@"+string(rune('0'+mi))+"."+string(rune('0'+bi)))
			}
		}
	}
	return tools, system, conversation
}

func TestPromptCachePolicyBreakpoints(t *testing.T) {
	toolResult := types.UserToolResults(types.ToolResultPart{CallID: "c1", Parts: []types.ToolOutputPart{types.Text("found")}})
	for _, tc := range []struct {
		name             string
		policy           PromptCachePolicy
		last             types.Message
		wantTools        []string
		wantSystem       []string
		wantConversation []string
	}{
		{"system only", PromptCachePolicy{TTL: "1h", System: true}, toolResult, nil, []string{"1h@1"}, nil},
		{"default marks tools, system, and trailing tool result", DefaultPromptCachePolicy("5m"), toolResult,
			[]string{"5m@1"}, []string{"5m@1"}, []string{"5m@2.0"}},
		{"trailing user text", PromptCachePolicy{TTL: "5m", Conversation: true}, types.UserMsg(types.Text("next")),
			nil, nil, []string{"5m@2.0"}},
		{"assistant tail gets no conversation marker", PromptCachePolicy{TTL: "5m", Conversation: true}, nil, nil, nil, nil},
		{"disabled places nothing", PromptCachePolicy{}, toolResult, nil, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := NewAdapter("unused", "claude-sonnet-4-5", WithPromptCachePolicy(tc.policy))
			params := cacheRequest(tc.last)
			if err := a.applyPromptCache(&params); err != nil {
				t.Fatal(err)
			}
			tools, system, conversation := breakpoints(params)
			if !equal(tools, tc.wantTools) || !equal(system, tc.wantSystem) || !equal(conversation, tc.wantConversation) {
				t.Fatalf("tools %v system %v conversation %v", tools, system, conversation)
			}
		})
	}
}

func TestPromptCachePolicyValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		model  string
		policy PromptCachePolicy
		params anthropic.MessageNewParams
	}{
		{"bad ttl", "claude-sonnet-4-5", PromptCachePolicy{TTL: "10m", System: true}, cacheRequest(nil)},
		{"system cache without system text", "claude-sonnet-4-5", PromptCachePolicy{TTL: "5m", System: true}, anthropic.MessageNewParams{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := NewAdapter("unused", tc.model, WithPromptCachePolicy(tc.policy))
			if err := a.applyPromptCache(&tc.params); err == nil {
				t.Fatal("invalid cache policy accepted")
			}
		})
	}
	// The legacy option is the system-only policy.
	a := NewAdapter("unused", "claude-sonnet-4-5", WithSystemPromptCache("1h"))
	if a.cachePolicy != (PromptCachePolicy{TTL: "1h", System: true}) {
		t.Fatalf("WithSystemPromptCache = %+v", a.cachePolicy)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
