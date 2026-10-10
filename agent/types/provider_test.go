package types

import (
	"context"
	"errors"
	"testing"
)

type testProvider struct{}

func (testProvider) Stream(ctx context.Context, _ Request) (<-chan Delta, error) {
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

func TestNameOf(t *testing.T) {
	if got := NameOf(testProvider{}); got != "unknown" {
		t.Errorf("NameOf(unnamed) = %q, want unknown", got)
	}
	if got := NameOf(testNamedProvider{}); got != "test-provider" {
		t.Errorf("NameOf(named) = %q, want test-provider", got)
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

func (p *scriptedProvider) Stream(_ context.Context, req Request) (<-chan Delta, error) {
	messages, tools := req.Messages, req.Tools
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
		PartStart{Index: 0, Kind: KindText},
		PartDelta{Index: 0, Text: "hello "},
		PartDelta{Index: 0, Text: "world"},
		PartEnd{Index: 0},
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
	if !ok || len(um.Parts) != 1 {
		t.Fatalf("message = %+v, want single-block UserMessage", p.messages[0])
	}
	if tc, ok := um.Parts[0].(TextPart); !ok || tc.Text != "hi" {
		t.Errorf("prompt block = %+v, want TextContent{hi}", um.Parts[0])
	}
}

func TestGenerateTextSurfacesErrorDelta(t *testing.T) {
	streamErr := errors.New("boom")
	p := &scriptedProvider{deltas: []Delta{
		PartDelta{Index: 0, Text: "partial"},
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
