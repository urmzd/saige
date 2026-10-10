package fallback

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

// mockProvider returns a fixed text response.
type mockProvider struct {
	response string
}

func (m *mockProvider) Stream(_ context.Context, _ types.Request) (<-chan types.Delta, error) {
	ch := make(chan types.Delta, 3)
	ch <- types.PartStart{Index: 0, Kind: types.KindText}
	ch <- types.PartDelta{Index: 0, Text: m.response}
	ch <- types.PartEnd{Index: 0}
	close(ch)
	return ch, nil
}

// errorProviderSimple returns a fixed error (used in provider tests).
type errorProviderSimple struct {
	err error
}

func (p *errorProviderSimple) Stream(_ context.Context, _ types.Request) (<-chan types.Delta, error) {
	return nil, p.err
}

func TestFallbackProvider_FirstSucceeds(t *testing.T) {
	p1 := &mockProvider{response: "from-primary"}
	p2 := &mockProvider{response: "from-backup"}

	fb := must.Get(Of(p1, p2))
	ch, err := fb.Stream(context.Background(), types.Request{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var text string
	for d := range ch {
		if tc, ok := d.(types.PartDelta); ok {
			text += tc.Text
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

	fb := must.Get(Of(failing, good))
	ch, err := fb.Stream(context.Background(), types.Request{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var text string
	for d := range ch {
		if tc, ok := d.(types.PartDelta); ok {
			text += tc.Text
		}
	}
	if text != "from-backup" {
		t.Errorf("got %q, want %q", text, "from-backup")
	}
}

func TestFallbackProvider_AllFail(t *testing.T) {
	p1 := &errorProviderSimple{err: &types.ProviderError{Provider: "a", Kind: types.ErrorKindTransient, Err: errors.New("fail-a")}}
	p2 := &errorProviderSimple{err: &types.ProviderError{Provider: "b", Kind: types.ErrorKindTransient, Err: errors.New("fail-b")}}

	fb := must.Get(Of(p1, p2))
	_, err := fb.Stream(context.Background(), types.Request{})
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

	fb := must.Get(New(Config{
		Providers:  []types.Provider{perm, good},
		FallbackOn: types.IsTransient, // only fallback on transient
	}))

	_, err := fb.Stream(context.Background(), types.Request{})
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

	fb := must.Get(Of(p1, p2))
	_, err := fb.Stream(ctx, types.Request{})
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

func (p *scriptProvider) Stream(_ context.Context, _ types.Request) (<-chan types.Delta, error) {
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
		case types.PartDelta:
			text += v.Text
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

	fb := must.Get(Of(failing, good))
	ch, err := fb.Stream(context.Background(), types.Request{})
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

// A UsageDelta before content (Anthropic emits usage at message_start) must
// not latch the no-fallback gate: an ErrorDelta after usage but before any
// content still falls back transparently.
func TestFallbackProvider_MidStreamErrorAfterUsageStillFallsBack(t *testing.T) {
	failing := &scriptProvider{deltas: []types.Delta{
		types.UsageDelta{PromptTokens: 7, TotalTokens: 7},
		types.ErrorDelta{Error: &types.ProviderError{Provider: "bad", Kind: types.ErrorKindTransient, Err: errors.New("died before content")}},
	}}
	good := &mockProvider{response: "from-backup"}

	fb := must.Get(Of(failing, good))
	ch, err := fb.Stream(context.Background(), types.Request{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var text string
	var errDeltas, usageDeltas int
	for d := range ch {
		switch v := d.(type) {
		case types.PartDelta:
			text += v.Text
		case types.ErrorDelta:
			errDeltas++
		case types.UsageDelta:
			usageDeltas++
		}
	}
	if text != "from-backup" {
		t.Errorf("text = %q, want %q (usage-only preamble must not block fallback)", text, "from-backup")
	}
	if errDeltas != 0 {
		t.Errorf("ErrorDelta count = %d, want 0 (fallback should be transparent)", errDeltas)
	}
	// The failed provider's usage was already forwarded; the aggregator merges
	// it with the successor's usage, so it is expected downstream.
	if usageDeltas != 1 {
		t.Errorf("UsageDelta count = %d, want 1 (forwarded from the failed provider)", usageDeltas)
	}
}

func TestFallbackProvider_MidStreamErrorAfterContent(t *testing.T) {
	streamErr := &types.ProviderError{Provider: "flaky", Kind: types.ErrorKindTransient, Err: errors.New("dropped")}
	flaky := &scriptProvider{deltas: []types.Delta{
		types.PartStart{Index: 0, Kind: types.KindText},
		types.PartDelta{Index: 0, Text: "partial"},
		types.ErrorDelta{Error: streamErr},
	}}
	backup := &scriptProvider{deltas: []types.Delta{types.PartDelta{Index: 0, Text: "from-backup"}}}

	fb := must.Get(Of(flaky, backup))
	ch, err := fb.Stream(context.Background(), types.Request{})
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
	backup := &scriptProvider{deltas: []types.Delta{types.PartDelta{Index: 0, Text: "should not reach"}}}

	fb := must.Get(New(Config{
		Providers:  []types.Provider{failing, backup},
		FallbackOn: types.IsTransient, // permanent errors must not fall back
	}))
	ch, err := fb.Stream(context.Background(), types.Request{})
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

	fb := must.Get(Of(p1, p2))
	ch, err := fb.Stream(context.Background(), types.Request{})
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
	good := &schemaProvider{mockProvider{response: "from-backup"}}

	fb := must.Get(Of(&schemaScriptProvider{failing}, good))
	ch, err := fb.Stream(context.Background(), types.Request{Schema: &types.ParameterSchema{Type: "object"}})
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

	fb := must.Get(Of(blocking))
	ch, err := fb.Stream(ctx, types.Request{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cancel()
	// The relay must observe cancellation and close the consumer channel even
	// though the source never closes.
	for range ch {
	}
}

// funcProvider delegates Stream to a closure.
type funcProvider struct {
	fn func() (<-chan types.Delta, error)
}

func (p *funcProvider) Stream(_ context.Context, _ types.Request) (<-chan types.Delta, error) {
	return p.fn()
}

// Cancelling the consumer context while the provider goroutine is blocked on
// an unbuffered send must not leak that goroutine: every relay return path
// that abandons a live src drains it.
func TestFallbackProvider_CancelDrainsBlockedProducer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	producerDone := make(chan struct{})
	producing := make(chan struct{})
	p := &funcProvider{fn: func() (<-chan types.Delta, error) {
		src := make(chan types.Delta) // unbuffered: every send blocks on the relay
		go func() {
			defer close(producerDone)
			defer close(src)
			src <- types.PartStart{Index: 0, Kind: types.KindText}
			close(producing)
			src <- types.PartDelta{Index: 0, Text: "in-flight"}
			src <- types.PartEnd{Index: 0}
		}()
		return src, nil
	}}

	fb := must.Get(Of(p))
	ch, err := fb.Stream(ctx, types.Request{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	<-ch // consume the first delta so the producer is mid-stream
	<-producing
	cancel()
	// Abandon the stream like a cancelled consumer: just wait for the relay to
	// close its output channel.
	for range ch {
	}

	select {
	case <-producerDone:
		// Producer unblocked: the relay drained src on its way out.
	case <-time.After(2 * time.Second):
		t.Fatal("provider goroutine leaked: relay abandoned src without draining it")
	}
}

// switchableProvider implements types.ModelSwitcher for WithModel tests.
type switchableProvider struct {
	model string
}

func (p *switchableProvider) Stream(_ context.Context, _ types.Request) (<-chan types.Delta, error) {
	ch := make(chan types.Delta)
	close(ch)
	return ch, nil
}

func (p *switchableProvider) Model() string { return p.model }

func (p *switchableProvider) WithTarget(t types.Target) (types.Provider, error) {
	model := string(t.Model)
	return &switchableProvider{model: model}, nil
}

func TestFallbackProvider_WithModel(t *testing.T) {
	s1 := &switchableProvider{model: "model-a"}
	s2 := &switchableProvider{model: "model-a"}
	plain := &mockProvider{response: "no-switch"} // does not implement ModelSwitcher

	fb := must.Get(New(Config{
		Providers:  []types.Provider{s1, s2, plain},
		FallbackOn: types.IsTransient,
	}))

	switched, ok := must.Get(fb.WithTarget(types.ModelTarget("model-b"))).(*Provider)
	if !ok {
		t.Fatalf("WithModel returned %T, want *Provider", must.Get(fb.WithTarget(types.ModelTarget("model-b"))))
	}
	if len(switched.Providers) != 3 {
		t.Fatalf("Providers = %d, want 3", len(switched.Providers))
	}
	for i, p := range switched.Providers[:2] {
		if got := types.ProviderModel(p); got != "model-b" {
			t.Errorf("provider %d model = %q, want %q", i, got, "model-b")
		}
	}
	if switched.Providers[2] != types.Provider(plain) {
		t.Errorf("non-switcher child should pass through unchanged")
	}
	if switched.FallbackOn == nil {
		t.Error("FallbackOn should be preserved")
	}
	// Originals are untouched.
	if s1.model != "model-a" || s2.model != "model-a" {
		t.Errorf("original providers mutated: %q, %q", s1.model, s2.model)
	}

	var _ types.TargetSwitcher = fb
}

func TestFallbackProvider_Name(t *testing.T) {
	fb := must.Get(Of(&mockProvider{}))
	if fb.Name() != "fallback" {
		t.Errorf("Name() = %q, want %q", fb.Name(), "fallback")
	}
}

// schemaProvider is a mockProvider that can enforce a response schema.
type schemaProvider struct{ mockProvider }

func (p *schemaProvider) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	return p.mockProvider.Stream(ctx, req)
}

func (p *schemaProvider) SupportsSchema() bool { return true }

// schemaScriptProvider is a scriptProvider that can enforce a response schema.
type schemaScriptProvider struct{ *scriptProvider }

func (p *schemaScriptProvider) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	return p.scriptProvider.Stream(ctx, req)
}

func (p *schemaScriptProvider) SupportsSchema() bool { return true }

// A RouteDelta before content (routers and splits send one per attempt) must
// not latch the no-fallback gate, alone or together with a usage preamble.
func TestFallbackProvider_RoutePreambleStillFallsBack(t *testing.T) {
	fail := types.ErrorDelta{Error: &types.ProviderError{Provider: "bad", Kind: types.ErrorKindUnavailable, Err: errors.New("down")}}
	tests := []struct {
		name     string
		preamble []types.Delta
	}{
		{name: "route only", preamble: []types.Delta{types.RouteDelta{Profile: "a"}}},
		{name: "route and usage", preamble: []types.Delta{types.RouteDelta{Profile: "a"}, types.UsageDelta{PromptTokens: 3}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			failing := &scriptProvider{deltas: append(append([]types.Delta{}, tt.preamble...), fail)}
			good := &scriptProvider{deltas: []types.Delta{
				types.PartStart{Index: 0, Kind: types.KindText}, types.PartDelta{Index: 0, Text: "from-backup"}, types.PartEnd{Index: 0},
			}}
			ch, err := must.Get(Of(failing, good)).Stream(context.Background(), types.Request{})
			if err != nil {
				t.Fatal(err)
			}
			text, errDeltas := collect(ch)
			if text != "from-backup" || len(errDeltas) != 0 {
				t.Errorf("text = %q, errors = %v; want the backup's answer and no error", text, errDeltas)
			}
			if good.callCount() != 1 {
				t.Errorf("backup calls = %d, want 1", good.callCount())
			}
		})
	}
}
