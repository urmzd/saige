package retry

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

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

// errorProviderSimple returns a fixed error.
type errorProviderSimple struct {
	err error
}

func (p *errorProviderSimple) ChatStream(_ context.Context, _ []types.Message, _ []types.ToolDef) (<-chan types.Delta, error) {
	return nil, p.err
}

func TestRetryProvider_SucceedsFirstTry(t *testing.T) {
	inner := &mockProvider{response: "ok"}
	rp := New(inner, DefaultConfig())

	ch, err := rp.ChatStream(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var text string
	for d := range ch {
		if tc, ok := d.(types.TextContentDelta); ok {
			text += tc.Content
		}
	}
	if text != "ok" {
		t.Errorf("got %q, want %q", text, "ok")
	}
}

func TestRetryProvider_RetriesOnTransient(t *testing.T) {
	var calls atomic.Int32
	inner := &countingProvider{
		calls:     &calls,
		failUntil: 2,
		err: &types.ProviderError{
			Provider: "flaky",
			Kind:     types.ErrorKindTransient,
			Err:      errors.New("timeout"),
		},
		response: "recovered",
	}

	cfg := Config{
		MaxAttempts: 3,
		BaseDelay:   1 * time.Millisecond, // fast for tests
		MaxDelay:    5 * time.Millisecond,
		Multiplier:  2.0,
	}
	rp := New(inner, cfg)

	ch, err := rp.ChatStream(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var text string
	for d := range ch {
		if tc, ok := d.(types.TextContentDelta); ok {
			text += tc.Content
		}
	}
	if text != "recovered" {
		t.Errorf("got %q, want %q", text, "recovered")
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3", calls.Load())
	}
}

func TestRetryProvider_StopsOnPermanent(t *testing.T) {
	inner := &errorProviderSimple{err: &types.ProviderError{
		Provider: "auth",
		Kind:     types.ErrorKindPermanent,
		Err:      errors.New("unauthorized"),
	}}

	cfg := Config{
		MaxAttempts: 5,
		BaseDelay:   1 * time.Millisecond,
	}
	rp := New(inner, cfg)

	_, err := rp.ChatStream(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	// Should not have retried -- permanent error
	var pe *types.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *ProviderError, got %T", err)
	}
}

func TestRetryProvider_ExhaustsAttempts(t *testing.T) {
	inner := &errorProviderSimple{err: &types.ProviderError{
		Provider: "down",
		Kind:     types.ErrorKindTransient,
		Err:      errors.New("server error"),
	}}

	cfg := Config{
		MaxAttempts: 2,
		BaseDelay:   1 * time.Millisecond,
	}
	rp := New(inner, cfg)

	_, err := rp.ChatStream(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}

	var re *types.RetryError
	if !errors.As(err, &re) {
		t.Fatalf("expected *RetryError, got %T", err)
	}
	if re.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", re.Attempts)
	}
	if !errors.Is(re.Last, types.ErrProviderFailed) {
		t.Error("last error should match ErrProviderFailed")
	}
}

func TestRetryProvider_ContextCancelledDuringBackoff(t *testing.T) {
	inner := &errorProviderSimple{err: &types.ProviderError{
		Provider: "slow",
		Kind:     types.ErrorKindTransient,
		Err:      errors.New("timeout"),
	}}

	ctx, cancel := context.WithCancel(context.Background())

	cfg := Config{
		MaxAttempts: 10,
		BaseDelay:   1 * time.Second, // long delay
	}
	rp := New(inner, cfg)

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := rp.ChatStream(ctx, nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestRetryProvider_Name(t *testing.T) {
	inner := &mockProvider{response: "ok"}
	rp := New(inner, DefaultConfig())
	if rp.Name() != "retry(unknown)" {
		t.Errorf("Name() = %q, want %q", rp.Name(), "retry(unknown)")
	}
}

func TestRetryProvider_DefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.MaxAttempts != 3 {
		t.Errorf("MaxAttempts = %d, want 3", cfg.MaxAttempts)
	}
	if cfg.BaseDelay != 500*time.Millisecond {
		t.Errorf("BaseDelay = %v, want 500ms", cfg.BaseDelay)
	}
}

// scriptedStreamProvider replays one delta sequence per ChatStream call,
// advancing through Scripts on each call. It models providers (and the
// streaming adapters in front of them) that deliver failures ON the channel as
// an ErrorDelta rather than via a synchronous error.
type scriptedStreamProvider struct {
	calls   atomic.Int32
	Scripts [][]types.Delta
}

func (p *scriptedStreamProvider) ChatStream(_ context.Context, _ []types.Message, _ []types.ToolDef) (<-chan types.Delta, error) {
	idx := int(p.calls.Add(1)) - 1
	ch := make(chan types.Delta, 8)
	go func() {
		defer close(ch)
		if idx < len(p.Scripts) {
			for _, d := range p.Scripts[idx] {
				ch <- d
			}
		}
	}()
	return ch, nil
}

func transientErr() error {
	return &types.ProviderError{
		Provider: "flaky",
		Kind:     types.ErrorKindTransient,
		Err:      errors.New("overloaded"),
	}
}

func permanentErr() error {
	return &types.ProviderError{
		Provider: "auth",
		Kind:     types.ErrorKindPermanent,
		Err:      errors.New("unauthorized"),
	}
}

// collect drains a delta channel into text, the first error delta seen, and the
// raw delta count.
func collect(ch <-chan types.Delta) (text string, n int, firstErr error) {
	for d := range ch {
		n++
		switch v := d.(type) {
		case types.TextContentDelta:
			text += v.Content
		case types.ErrorDelta:
			if firstErr == nil {
				firstErr = v.Error
			}
		}
	}
	return text, n, firstErr
}

// TestRetryProvider_ChannelError exercises the channel-wrapping retry path: a
// transient ErrorDelta arriving on the stream BEFORE any content should trigger
// a re-invocation, while errors after content (or permanent errors) surface.
func TestRetryProvider_ChannelError(t *testing.T) {
	tests := []struct {
		name      string
		scripts   [][]types.Delta
		wantText  string
		wantErr   bool
		wantCalls int32
	}{
		{
			name: "transient error before content retries then succeeds",
			scripts: [][]types.Delta{
				// Attempt 1: usage preamble, then a mid-stream error before content.
				{
					types.UsageDelta{PromptTokens: 5},
					types.ErrorDelta{Error: transientErr()},
				},
				// Attempt 2: clean success.
				{
					types.TextStartDelta{},
					types.TextContentDelta{Content: "recovered"},
					types.TextEndDelta{},
				},
			},
			wantText:  "recovered",
			wantErr:   false,
			wantCalls: 2,
		},
		{
			name: "error after content is surfaced, not retried",
			scripts: [][]types.Delta{
				{
					types.TextStartDelta{},
					types.TextContentDelta{Content: "partial"},
					types.ErrorDelta{Error: transientErr()},
				},
				// This second script must never be reached.
				{types.TextContentDelta{Content: "should-not-happen"}},
			},
			wantText:  "partial",
			wantErr:   true,
			wantCalls: 1,
		},
		{
			name: "permanent error before content is surfaced, not retried",
			scripts: [][]types.Delta{
				{types.ErrorDelta{Error: permanentErr()}},
				{types.TextContentDelta{Content: "should-not-happen"}},
			},
			wantText:  "",
			wantErr:   true,
			wantCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inner := &scriptedStreamProvider{Scripts: tt.scripts}
			cfg := Config{
				MaxAttempts: 2,
				BaseDelay:   1 * time.Millisecond,
				MaxDelay:    2 * time.Millisecond,
				Multiplier:  2.0,
			}
			rp := New(inner, cfg)

			ch, err := rp.ChatStream(context.Background(), nil, nil)
			if err != nil {
				t.Fatalf("ChatStream returned synchronous error: %v", err)
			}
			text, _, firstErr := collect(ch)

			if text != tt.wantText {
				t.Errorf("text = %q, want %q", text, tt.wantText)
			}
			if tt.wantErr && firstErr == nil {
				t.Error("expected a terminal ErrorDelta, got none")
			}
			if !tt.wantErr && firstErr != nil {
				t.Errorf("unexpected error delta: %v", firstErr)
			}
			if got := inner.calls.Load(); got != tt.wantCalls {
				t.Errorf("provider calls = %d, want %d", got, tt.wantCalls)
			}
		})
	}
}

// TestRetryProvider_ChannelErrorExhausted verifies that when every attempt
// emits a transient ErrorDelta before any content, the retry loop exhausts its
// attempts and reports a synchronous RetryError — matching the existing
// synchronous-error exhaustion contract.
func TestRetryProvider_ChannelErrorExhausted(t *testing.T) {
	inner := &scriptedStreamProvider{Scripts: [][]types.Delta{
		{types.ErrorDelta{Error: transientErr()}},
		{types.ErrorDelta{Error: transientErr()}},
	}}
	cfg := Config{MaxAttempts: 2, BaseDelay: 1 * time.Millisecond}
	rp := New(inner, cfg)

	_, err := rp.ChatStream(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("expected a synchronous RetryError after exhausting attempts")
	}
	var re *types.RetryError
	if !errors.As(err, &re) {
		t.Fatalf("expected *RetryError, got %T: %v", err, err)
	}
	if re.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", re.Attempts)
	}
	if got := inner.calls.Load(); got != 2 {
		t.Errorf("provider calls = %d, want 2", got)
	}
}

// countingProvider fails for the first N calls, then succeeds.
type countingProvider struct {
	calls     *atomic.Int32
	failUntil int32
	err       error
	response  string
}

func (p *countingProvider) ChatStream(_ context.Context, _ []types.Message, _ []types.ToolDef) (<-chan types.Delta, error) {
	n := p.calls.Add(1)
	if n <= p.failUntil {
		return nil, p.err
	}
	ch := make(chan types.Delta, 3)
	ch <- types.TextStartDelta{}
	ch <- types.TextContentDelta{Content: p.response}
	ch <- types.TextEndDelta{}
	close(ch)
	return ch, nil
}
