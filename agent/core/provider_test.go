package core

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
