package types

import (
	"context"
	"errors"
	"testing"
)

type testProvider struct{}

func (testProvider) ChatStream(ctx context.Context, messages []Message, tools []ToolDef) (<-chan Delta, error) {
	ch := make(chan Delta)
	close(ch)
	return ch, nil
}

type testNamedProvider struct {
	testProvider
}

func (testNamedProvider) Name() string { return "test-provider" }

type testCloserProvider struct {
	testProvider
	closed bool
}

func (p *testCloserProvider) Close() error {
	p.closed = true
	return nil
}

func TestProviderName(t *testing.T) {
	if got := ProviderName(testProvider{}); got != "unknown" {
		t.Errorf("ProviderName(unnamed) = %q, want unknown", got)
	}
	if got := ProviderName(testNamedProvider{}); got != "test-provider" {
		t.Errorf("ProviderName(named) = %q, want test-provider", got)
	}
}

func TestCloseProvider(t *testing.T) {
	// Non-closer returns nil
	if err := CloseProvider(testProvider{}); err != nil {
		t.Errorf("CloseProvider(non-closer) = %v, want nil", err)
	}

	// Closer gets called
	p := &testCloserProvider{}
	if err := CloseProvider(p); err != nil {
		t.Errorf("CloseProvider(closer) = %v, want nil", err)
	}
	if !p.closed {
		t.Error("Close was not called")
	}
}

// scriptedProvider replays a fixed delta sequence and records the last request.
type scriptedProvider struct {
	deltas   []Delta
	err      error
	messages []Message
	tools    []ToolDef
}

func (p *scriptedProvider) ChatStream(_ context.Context, messages []Message, tools []ToolDef) (<-chan Delta, error) {
	p.messages, p.tools = messages, tools
	if p.err != nil {
		return nil, p.err
	}
	ch := make(chan Delta, len(p.deltas))
	for _, d := range p.deltas {
		ch <- d
	}
	close(ch)
	return ch, nil
}

func TestGenerateTextConcatenatesDeltas(t *testing.T) {
	p := &scriptedProvider{deltas: []Delta{
		TextStartDelta{},
		TextContentDelta{Content: "hello "},
		TextContentDelta{Content: "world"},
		TextEndDelta{},
	}}
	got, err := GenerateText(context.Background(), p, "hi")
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello world" {
		t.Errorf("GenerateText = %q, want 'hello world'", got)
	}
	// Single-turn user prompt, no tools.
	if len(p.messages) != 1 || len(p.tools) != 0 {
		t.Fatalf("request = %d messages, %d tools, want 1 and 0", len(p.messages), len(p.tools))
	}
	um, ok := p.messages[0].(UserMessage)
	if !ok || len(um.Content) != 1 {
		t.Fatalf("message = %+v, want single-block UserMessage", p.messages[0])
	}
	if tc, ok := um.Content[0].(TextContent); !ok || tc.Text != "hi" {
		t.Errorf("prompt block = %+v, want TextContent{hi}", um.Content[0])
	}
}

func TestGenerateTextSurfacesErrorDelta(t *testing.T) {
	streamErr := errors.New("boom")
	p := &scriptedProvider{deltas: []Delta{
		TextContentDelta{Content: "partial"},
		ErrorDelta{Error: streamErr},
	}}
	_, err := GenerateText(context.Background(), p, "hi")
	if !errors.Is(err, streamErr) {
		t.Errorf("GenerateText error = %v, want %v", err, streamErr)
	}
}

func TestGenerateTextPropagatesChatStreamError(t *testing.T) {
	callErr := errors.New("connect refused")
	p := &scriptedProvider{err: callErr}
	_, err := GenerateText(context.Background(), p, "hi")
	if !errors.Is(err, callErr) {
		t.Errorf("GenerateText error = %v, want %v", err, callErr)
	}
}

func TestProviderError(t *testing.T) {
	err := &ProviderError{
		Provider: "openai",
		Model:    "gpt-4",
		Kind:     ErrorKindTransient,
		Code:     429,
		Err:      errors.New("rate limited"),
	}

	if !errors.Is(err, ErrProviderFailed) {
		t.Error("ProviderError should match ErrProviderFailed")
	}
	if !IsTransient(err) {
		t.Error("429 should be transient")
	}
}
