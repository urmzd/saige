package embedderregistry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/provider/retry"
	agenttypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/rag/types"
)

func TestRetryingDelay(t *testing.T) {
	limited := &agenttypes.ProviderError{Kind: agenttypes.ErrorKindRateLimit, RetryAfter: 3 * time.Second, Err: errors.New("429")}
	tests := []struct {
		name    string
		cfg     retry.Config
		attempt int
		err     error
		u       float64
		want    time.Duration
	}{
		{"exponential without jitter", retry.Config{BaseDelay: time.Second, MaxDelay: time.Minute, Multiplier: 2, DisableJitter: true}, 2, errors.New("x"), 0.5, 4 * time.Second},
		{"capped at max delay", retry.Config{BaseDelay: time.Second, MaxDelay: 3 * time.Second, Multiplier: 2, DisableJitter: true}, 5, errors.New("x"), 0.5, 3 * time.Second},
		{"full jitter scales", retry.Config{BaseDelay: time.Second, MaxDelay: time.Minute, Multiplier: 2}, 1, errors.New("x"), 0.25, 500 * time.Millisecond},
		{"retry-after is a floor", retry.Config{BaseDelay: time.Second, MaxDelay: time.Minute, Multiplier: 2}, 0, limited, 0.1, 3 * time.Second},
		{"retry-after is capped", retry.Config{BaseDelay: time.Second, MaxDelay: time.Minute, Multiplier: 2, MaxRetryAfter: time.Second}, 0, limited, 0, time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRetrying(nil, tt.cfg)
			r.rand = func() float64 { return tt.u }
			if got := r.delay(tt.attempt, tt.err); got != tt.want {
				t.Errorf("delay = %v, want %v", got, tt.want)
			}
		})
	}
}

// scriptedEmbedder fails with the scripted errors, then succeeds.
type scriptedEmbedder struct {
	errs  []error
	calls int
}

func (s *scriptedEmbedder) Embed(_ context.Context, variants []types.ContentVariant) ([][]float32, error) {
	s.calls++
	if s.calls <= len(s.errs) {
		return nil, s.errs[s.calls-1]
	}
	out := make([][]float32, len(variants))
	for i := range out {
		out[i] = []float32{1}
	}
	return out, nil
}

func TestRetryingEmbed(t *testing.T) {
	transient := &agenttypes.ProviderError{Kind: agenttypes.ErrorKindRateLimit, Code: 429, Err: errors.New("slow down"),
		RetryAfter: 2 * time.Second}
	permanent := &agenttypes.ProviderError{Kind: agenttypes.ErrorKindAuth, Code: 401, Err: errors.New("bad key")}
	tests := []struct {
		name      string
		errs      []error
		attempts  int
		wantCalls int
		wantErr   error
		wantRetry bool
		wantWaits []time.Duration
	}{
		{name: "rate limit then success", errs: []error{transient}, attempts: 3, wantCalls: 2, wantWaits: []time.Duration{2 * time.Second}},
		{name: "permanent is not retried", errs: []error{permanent}, attempts: 3, wantCalls: 1, wantErr: agenttypes.ErrAuth},
		{name: "shape errors are not retried", errs: []error{types.ErrEmbeddingShape}, attempts: 3, wantCalls: 1, wantErr: types.ErrEmbeddingShape},
		{name: "exhausted", errs: []error{transient, transient, transient}, attempts: 2, wantCalls: 2, wantErr: agenttypes.ErrRateLimited, wantRetry: true,
			wantWaits: []time.Duration{2 * time.Second}},
		{name: "network reset is retried", errs: []error{errors.New("read tcp: connection reset by peer")}, attempts: 2, wantCalls: 2,
			wantWaits: []time.Duration{0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inner := &scriptedEmbedder{errs: tt.errs}
			r := NewRetrying(inner, retry.Config{MaxAttempts: tt.attempts, BaseDelay: time.Millisecond})
			r.rand = func() float64 { return 0 }
			var waits []time.Duration
			r.sleep = func(_ context.Context, d time.Duration) bool { waits = append(waits, d); return true }

			out, err := r.Embed(context.Background(), []types.ContentVariant{{Text: "x"}})
			if inner.calls != tt.wantCalls {
				t.Errorf("calls = %d, want %d", inner.calls, tt.wantCalls)
			}
			if len(waits) != len(tt.wantWaits) {
				t.Fatalf("waits = %v, want %v", waits, tt.wantWaits)
			}
			for i := range waits {
				if waits[i] != tt.wantWaits[i] {
					t.Errorf("wait %d = %v, want %v", i, waits[i], tt.wantWaits[i])
				}
			}
			if tt.wantErr == nil {
				if err != nil || len(out) != 1 {
					t.Fatalf("out=%v err=%v", out, err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			var re *agenttypes.RetryError
			if got := errors.As(err, &re); got != tt.wantRetry {
				t.Errorf("RetryError = %v, want %v", got, tt.wantRetry)
			}
		})
	}
}

func TestRetryingStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	transient := &agenttypes.ProviderError{Kind: agenttypes.ErrorKindUnavailable, Err: errors.New("503")}
	inner := &scriptedEmbedder{errs: []error{transient, transient}}
	r := NewRetrying(inner, retry.Config{MaxAttempts: 5})
	r.sleep = func(context.Context, time.Duration) bool { cancel(); return false }
	_, err := r.Embed(ctx, []types.ContentVariant{{Text: "x"}})
	if !errors.Is(err, context.Canceled) || inner.calls != 1 {
		t.Fatalf("err = %v calls = %d, want cancellation after one call", err, inner.calls)
	}
}

func TestIsRetryableEmbedError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"canceled", context.Canceled, false},
		{"deadline", context.DeadlineExceeded, true},
		{"rate limited provider error", &agenttypes.ProviderError{Kind: agenttypes.ErrorKindRateLimit, Err: errors.New("x")}, true},
		{"permanent provider error wins over its text", &agenttypes.ProviderError{Kind: agenttypes.ErrorKindInvalidRequest, Err: errors.New("connection reset")}, false},
		{"unclassified reset", errors.New("connection reset by peer"), true},
		{"unclassified other", errors.New("boom"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsRetryableEmbedError(tt.err); got != tt.want {
				t.Errorf("IsRetryableEmbedError = %v, want %v", got, tt.want)
			}
		})
	}
}
