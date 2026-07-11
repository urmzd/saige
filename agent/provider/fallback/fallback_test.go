package fallback

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

// mockProvider returns a fixed text response.
type mockProvider struct {
	response string
}

func (m *mockProvider) ChatStream(_ context.Context, _ []types.Message, _ []types.ToolDef) (<-chan types.Delta, error) {
	ch := make(chan types.Delta, 3)
	ch <- types.TextStartDelta{}
	ch <- types.TextContentDelta{Content: m.response}
	ch <- types.TextEndDelta{}
	close(ch)
	return ch, nil
}

// errorProviderSimple returns a fixed error (used in provider tests).
type errorProviderSimple struct {
	err error
}

func (p *errorProviderSimple) ChatStream(_ context.Context, _ []types.Message, _ []types.ToolDef) (<-chan types.Delta, error) {
	return nil, p.err
}

func TestFallbackProvider_FirstSucceeds(t *testing.T) {
	p1 := &mockProvider{response: "from-primary"}
	p2 := &mockProvider{response: "from-backup"}

	fb := New(p1, p2)
	ch, err := fb.ChatStream(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var text string
	for d := range ch {
		if tc, ok := d.(types.TextContentDelta); ok {
			text += tc.Content
		}
	}
	if text != "from-primary" {
		t.Errorf("got %q, want %q", text, "from-primary")
	}
}

func TestFallbackProvider_FallsBackOnError(t *testing.T) {
	failing := &errorProviderSimple{err: &types.ProviderError{
		Provider: "bad",
		Kind:     types.ErrorKindTransient,
		Err:      errors.New("connection refused"),
	}}
	good := &mockProvider{response: "from-backup"}

	fb := New(failing, good)
	ch, err := fb.ChatStream(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var text string
	for d := range ch {
		if tc, ok := d.(types.TextContentDelta); ok {
			text += tc.Content
		}
	}
	if text != "from-backup" {
		t.Errorf("got %q, want %q", text, "from-backup")
	}
}

func TestFallbackProvider_AllFail(t *testing.T) {
	p1 := &errorProviderSimple{err: &types.ProviderError{Provider: "a", Kind: types.ErrorKindTransient, Err: errors.New("fail-a")}}
	p2 := &errorProviderSimple{err: &types.ProviderError{Provider: "b", Kind: types.ErrorKindTransient, Err: errors.New("fail-b")}}

	fb := New(p1, p2)
	_, err := fb.ChatStream(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}

	var fe *types.FallbackError
	if !errors.As(err, &fe) {
		t.Fatalf("expected *FallbackError, got %T", err)
	}
	if len(fe.Errors) != 2 {
		t.Errorf("errors = %d, want 2", len(fe.Errors))
	}
	if !errors.Is(err, types.ErrProviderFailed) {
		t.Error("FallbackError should match ErrProviderFailed")
	}
}

func TestFallbackProvider_StopsOnPermanentWhenConfigured(t *testing.T) {
	perm := &errorProviderSimple{err: &types.ProviderError{Provider: "auth-fail", Kind: types.ErrorKindPermanent, Err: errors.New("unauthorized")}}
	good := &mockProvider{response: "should not reach"}

	fb := &Provider{
		Providers:  []types.Provider{perm, good},
		FallbackOn: types.IsTransient, // only fallback on transient
	}

	_, err := fb.ChatStream(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}

	var fe *types.FallbackError
	if !errors.As(err, &fe) {
		t.Fatalf("expected *FallbackError, got %T", err)
	}
	if len(fe.Errors) != 1 {
		t.Errorf("errors = %d, want 1 (should not have tried second provider)", len(fe.Errors))
	}
}

func TestFallbackProvider_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	p1 := &errorProviderSimple{err: &types.ProviderError{Provider: "a", Kind: types.ErrorKindTransient, Err: errors.New("fail")}}
	p2 := &mockProvider{response: "should not reach"}

	fb := New(p1, p2)
	_, err := fb.ChatStream(ctx, nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}

	var fe *types.FallbackError
	if !errors.As(err, &fe) {
		t.Fatalf("expected *FallbackError, got %T", err)
	}
	if len(fe.Errors) != 1 {
		t.Errorf("errors = %d, want 1 (should stop after context cancel)", len(fe.Errors))
	}
}

// scriptProvider emits a fixed delta sequence and counts calls (used for
// mid-stream error tests).
type scriptProvider struct {
	deltas []types.Delta
	calls  int32
}

func (p *scriptProvider) ChatStream(_ context.Context, _ []types.Message, _ []types.ToolDef) (<-chan types.Delta, error) {
	atomic.AddInt32(&p.calls, 1)
	ch := make(chan types.Delta, len(p.deltas)+1)
	for _, d := range p.deltas {
		ch <- d
	}
	close(ch)
	return ch, nil
}

func (p *scriptProvider) callCount() int32 { return atomic.LoadInt32(&p.calls) }

func collect(ch <-chan types.Delta) (text string, errDeltas []types.ErrorDelta) {
	for d := range ch {
		switch v := d.(type) {
		case types.TextContentDelta:
			text += v.Content
		case types.ErrorDelta:
			errDeltas = append(errDeltas, v)
		}
	}
	return text, errDeltas
}

func TestFallbackProvider_MidStreamErrorBeforeContent(t *testing.T) {
	failing := &scriptProvider{deltas: []types.Delta{
		types.ErrorDelta{Error: &types.ProviderError{Provider: "bad", Kind: types.ErrorKindTransient, Err: errors.New("overloaded")}},
	}}
	good := &mockProvider{response: "from-backup"}

	fb := New(failing, good)
	ch, err := fb.ChatStream(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	text, errDeltas := collect(ch)
	if text != "from-backup" {
		t.Errorf("text = %q, want %q", text, "from-backup")
	}
	if len(errDeltas) != 0 {
		t.Errorf("ErrorDelta count = %d, want 0 (fallback should be transparent)", len(errDeltas))
	}
}

func TestFallbackProvider_MidStreamErrorAfterContent(t *testing.T) {
	streamErr := &types.ProviderError{Provider: "flaky", Kind: types.ErrorKindTransient, Err: errors.New("dropped")}
	flaky := &scriptProvider{deltas: []types.Delta{
		types.TextStartDelta{},
		types.TextContentDelta{Content: "partial"},
		types.ErrorDelta{Error: streamErr},
	}}
	backup := &scriptProvider{deltas: []types.Delta{types.TextContentDelta{Content: "from-backup"}}}

	fb := New(flaky, backup)
	ch, err := fb.ChatStream(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	text, errDeltas := collect(ch)
	if text != "partial" {
		t.Errorf("text = %q, want %q (no duplicated output)", text, "partial")
	}
	if len(errDeltas) != 1 {
		t.Fatalf("ErrorDelta count = %d, want 1", len(errDeltas))
	}
	if !errors.Is(errDeltas[0].Error, streamErr) {
		t.Errorf("ErrorDelta = %v, want the original stream error", errDeltas[0].Error)
	}
	if backup.callCount() != 0 {
		t.Errorf("backup called %d times, want 0 (content already forwarded)", backup.callCount())
	}
}

func TestFallbackProvider_MidStreamErrorNotFallbackable(t *testing.T) {
	permErr := &types.ProviderError{Provider: "auth-fail", Kind: types.ErrorKindPermanent, Err: errors.New("unauthorized")}
	failing := &scriptProvider{deltas: []types.Delta{types.ErrorDelta{Error: permErr}}}
	backup := &scriptProvider{deltas: []types.Delta{types.TextContentDelta{Content: "should not reach"}}}

	fb := &Provider{
		Providers:  []types.Provider{failing, backup},
		FallbackOn: types.IsTransient, // permanent errors must not fall back
	}
	ch, err := fb.ChatStream(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	text, errDeltas := collect(ch)
	if text != "" {
		t.Errorf("text = %q, want empty", text)
	}
	if len(errDeltas) != 1 {
		t.Fatalf("ErrorDelta count = %d, want 1", len(errDeltas))
	}
	if !errors.Is(errDeltas[0].Error, permErr) {
		t.Errorf("ErrorDelta = %v, want the original permanent error propagated as-is", errDeltas[0].Error)
	}
	if backup.callCount() != 0 {
		t.Errorf("backup called %d times, want 0", backup.callCount())
	}
}

func TestFallbackProvider_MidStreamAllFail(t *testing.T) {
	err1 := &types.ProviderError{Provider: "a", Kind: types.ErrorKindTransient, Err: errors.New("fail-a")}
	err2 := &types.ProviderError{Provider: "b", Kind: types.ErrorKindTransient, Err: errors.New("fail-b")}
	p1 := &scriptProvider{deltas: []types.Delta{types.ErrorDelta{Error: err1}}}
	p2 := &scriptProvider{deltas: []types.Delta{types.ErrorDelta{Error: err2}}}

	fb := New(p1, p2)
	ch, err := fb.ChatStream(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, errDeltas := collect(ch)
	if len(errDeltas) != 1 {
		t.Fatalf("ErrorDelta count = %d, want 1", len(errDeltas))
	}
	var fe *types.FallbackError
	if !errors.As(errDeltas[0].Error, &fe) {
		t.Fatalf("expected *FallbackError, got %T", errDeltas[0].Error)
	}
	if len(fe.Errors) != 2 {
		t.Errorf("errors = %d, want 2", len(fe.Errors))
	}
}

func TestFallbackProvider_WithSchemaMidStreamErrorBeforeContent(t *testing.T) {
	failing := &scriptProvider{deltas: []types.Delta{
		types.ErrorDelta{Error: &types.ProviderError{Provider: "bad", Kind: types.ErrorKindTransient, Err: errors.New("overloaded")}},
	}}
	good := &mockProvider{response: "from-backup"}

	fb := New(failing, good)
	ch, err := fb.ChatStreamWithSchema(context.Background(), nil, nil, &types.ParameterSchema{Type: "object"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	text, errDeltas := collect(ch)
	if text != "from-backup" {
		t.Errorf("text = %q, want %q", text, "from-backup")
	}
	if len(errDeltas) != 0 {
		t.Errorf("ErrorDelta count = %d, want 0", len(errDeltas))
	}
}

func TestFallbackProvider_MidStreamContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	blocked := make(chan types.Delta)
	blocking := &funcProvider{fn: func() (<-chan types.Delta, error) { return blocked, nil }}

	fb := New(blocking)
	ch, err := fb.ChatStream(ctx, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cancel()
	// The relay must observe cancellation and close the consumer channel even
	// though the source never closes.
	for range ch {
	}
}

// funcProvider delegates ChatStream to a closure.
type funcProvider struct {
	fn func() (<-chan types.Delta, error)
}

func (p *funcProvider) ChatStream(_ context.Context, _ []types.Message, _ []types.ToolDef) (<-chan types.Delta, error) {
	return p.fn()
}

func TestFallbackProvider_Name(t *testing.T) {
	fb := New()
	if fb.Name() != "fallback" {
		t.Errorf("Name() = %q, want %q", fb.Name(), "fallback")
	}
}
