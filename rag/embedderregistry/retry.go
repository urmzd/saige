package embedderregistry

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/urmzd/saige/agent/provider/retry"
	agenttypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/rag/types"
)

// Retrying is a VariantEmbedder decorator that retries transient failures
// with exponential backoff and full jitter, honoring a Retry-After the
// provider sent. It uses the same configuration and error taxonomy as the
// chat retry decorator (retry.Config, agent/types.ProviderError), so one
// rate-limited embedding call no longer aborts a whole ingest.
//
// An error is retried when retry.Config.ShouldRetry says so or, by default,
// when IsRetryableEmbedError reports it transient. Shape errors
// (types.ErrEmbeddingShape) and caller cancellation are never retried. When
// every attempt fails, the error is an *agent/types.RetryError wrapping the
// last failure, so errors.Is and the agent/types classification helpers
// still see the cause.
type Retrying struct {
	inner types.VariantEmbedder
	cfg   retry.Config
	// rand returns a value in [0, 1) for jitter; tests replace it.
	rand func() float64
	// sleep waits d or until ctx is done; tests replace it.
	sleep func(ctx context.Context, d time.Duration) bool
}

// NewRetrying wraps inner with retries. Zero fields of cfg take the values
// of retry.DefaultConfig, except ShouldRetry, DisableJitter, and
// MaxRetryAfter, whose zero values are meaningful.
func NewRetrying(inner types.VariantEmbedder, cfg retry.Config) *Retrying {
	def := retry.DefaultConfig()
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = def.MaxAttempts
	}
	if cfg.BaseDelay <= 0 {
		cfg.BaseDelay = def.BaseDelay
	}
	if cfg.MaxDelay <= 0 {
		cfg.MaxDelay = def.MaxDelay
	}
	if cfg.Multiplier <= 0 {
		cfg.Multiplier = def.Multiplier
	}
	return &Retrying{inner: inner, cfg: cfg, rand: rand.Float64, sleep: sleepContext}
}

// Name reports the inner embedder's name, implementing types.Named.
func (r *Retrying) Name() string {
	if n, ok := r.inner.(types.Named); ok {
		return n.Name()
	}
	return ""
}

// Unwrap returns the inner embedder.
func (r *Retrying) Unwrap() types.VariantEmbedder { return r.inner }

// Embed calls the inner embedder until it succeeds, fails permanently, or
// runs out of attempts.
func (r *Retrying) Embed(ctx context.Context, variants []types.ContentVariant) ([][]float32, error) {
	var lastErr error
	for attempt := 0; attempt < r.cfg.MaxAttempts; attempt++ {
		out, err := r.inner.Embed(ctx, variants)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if ctx.Err() != nil || !r.shouldRetry(err) {
			return nil, err
		}
		if attempt == r.cfg.MaxAttempts-1 {
			break
		}
		if !r.sleep(ctx, r.delay(attempt, err)) {
			return nil, errors.Join(ctx.Err(), err)
		}
	}
	return nil, &agenttypes.RetryError{Attempts: r.cfg.MaxAttempts, Last: lastErr}
}

func (r *Retrying) shouldRetry(err error) bool {
	if errors.Is(err, types.ErrEmbeddingShape) {
		return false
	}
	if r.cfg.ShouldRetry != nil {
		return r.cfg.ShouldRetry(err)
	}
	return IsRetryableEmbedError(err)
}

// delay is the wait after a failed attempt, computed by retry.Config.Delay
// so embedding and chat retries back off the same way.
func (r *Retrying) delay(attempt int, err error) time.Duration {
	return r.cfg.Delay(attempt, err, r.rand())
}

// IsRetryableEmbedError reports whether an embedding error is worth
// retrying: a classified transient provider error (rate limit, overload,
// unavailable) or an unclassified network failure such as a reset
// connection or a timeout. Caller cancellation never is.
func IsRetryableEmbedError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if agenttypes.IsTransient(err) {
		return true
	}
	var pe *agenttypes.ProviderError
	if errors.As(err, &pe) {
		// The provider classified it; trust a permanent verdict.
		return false
	}
	kind, ok := agenttypes.ClassifyTransportError(err)
	return ok && kind.Transient()
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
