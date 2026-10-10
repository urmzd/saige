package otel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"

	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

// fakeProvider streams a fixed delta sequence, optionally after a delay
// before Stream returns, or fails before streaming.
type fakeProvider struct {
	deltas []types.Delta
	delay  time.Duration
	err    error
	opts   *types.RequestOptions
}

func (p *fakeProvider) Name() string  { return "fake" }
func (p *fakeProvider) Model() string { return "m1" }

func (p *fakeProvider) Stream(_ context.Context, _ types.Request) (<-chan types.Delta, error) {
	time.Sleep(p.delay)
	if p.err != nil {
		return nil, p.err
	}
	ch := make(chan types.Delta, len(p.deltas))
	for _, d := range p.deltas {
		ch <- d
	}
	close(ch)
	return ch, nil
}

// optionsProvider also accepts request options.
type optionsProvider struct{ fakeProvider }

func (p *optionsProvider) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	if req.Options != nil {
		o := *req.Options
		p.opts = &o
	}
	return p.fakeProvider.Stream(ctx, types.Request{Messages: req.Messages, Tools: req.Tools})
}

func (p *optionsProvider) SupportsOptions() bool { return true }

// schemaProvider also accepts structured output requests.
type schemaProvider struct{ fakeProvider }

func (p *schemaProvider) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	return p.fakeProvider.Stream(ctx, types.Request{Messages: req.Messages, Tools: req.Tools})
}

func (p *schemaProvider) SupportsSchema() bool { return true }

func drain(t *testing.T, ch <-chan types.Delta) []types.Delta {
	t.Helper()
	var out []types.Delta
	for d := range ch {
		out = append(out, d)
	}
	return out
}

func TestTracedProviderChatStream(t *testing.T) {
	// Anthropic reports prompt and cache counts at message start and
	// completion counts at message end.
	anthropicUsage := []types.Delta{
		types.UsageDelta{PromptTokens: 120, CachedPromptTokens: 100, CacheWriteTokens: 7, Cumulative: true},
		types.PartDelta{Index: 0, Text: "hi"},
		types.UsageDelta{CompletionTokens: 9, Cumulative: true, ResponseModel: "m1-2025", FinishReasons: []string{"end_turn"}},
		types.DoneDelta{},
	}
	rateLimited := &types.ProviderError{Provider: "fake", Kind: types.ErrorKindRateLimit, RetryAfter: 2 * time.Second, Err: errors.New("slow down")}

	tests := []struct {
		name      string
		provider  *fakeProvider
		wantErr   bool
		check     func(t *testing.T, s *spySpan)
		wantCount int
		// wantModel is gen_ai.request.model; empty means the provider's "m1".
		wantModel string
	}{
		{
			name:      "usage parts merge",
			provider:  &fakeProvider{deltas: anthropicUsage},
			wantCount: len(anthropicUsage),
			check: func(t *testing.T, s *spySpan) {
				for key, want := range map[string]int64{
					"gen_ai.usage.input_tokens":                120,
					"gen_ai.usage.output_tokens":               9,
					"gen_ai.usage.cache_read.input_tokens":     100,
					"gen_ai.usage.cache_creation.input_tokens": 7,
				} {
					if got := s.int(key); got != want {
						t.Errorf("%s = %d, want %d", key, got, want)
					}
				}
				if got := s.str("gen_ai.response.model"); got != "m1-2025" {
					t.Errorf("response model = %q", got)
				}
			},
		},
		{
			name: "route and variant",
			provider: &fakeProvider{deltas: []types.Delta{
				types.RouteDelta{Profile: "primary", Provider: "a", Model: "x", Reason: "primary"},
				types.RouteDelta{Profile: "canary", Provider: "b", Model: "y", Experiment: "exp1", Variant: "treatment", Reason: "fallback"},
				types.PartDelta{Index: 0, Text: "ok"},
			}},
			wantCount: 3,
			wantModel: "y",
			check: func(t *testing.T, s *spySpan) {
				for key, want := range map[string]string{
					"gen_ai.provider.name":   "b",
					"saige.route.profile":    "canary",
					"saige.route.provider":   "b",
					"saige.route.model":      "y",
					"saige.route.experiment": "exp1",
					"saige.route.variant":    "treatment",
					"saige.route.reason":     "fallback",
				} {
					if got := s.str(key); got != want {
						t.Errorf("%s = %q, want %q", key, got, want)
					}
				}
			},
		},
		{
			name:      "in-band error keeps partial usage",
			provider:  &fakeProvider{deltas: []types.Delta{types.UsageDelta{PromptTokens: 5}, types.ErrorDelta{Error: rateLimited}}},
			wantCount: 2,
			check: func(t *testing.T, s *spySpan) {
				if s.status != codes.Error {
					t.Errorf("status = %v, want error", s.status)
				}
				if got := s.str("error.type"); got != "rate_limit" {
					t.Errorf("error.type = %q, want rate_limit", got)
				}
				if got := s.int("saige.error.retry_after_ms"); got != 2000 {
					t.Errorf("retry_after_ms = %d, want 2000", got)
				}
				if v, _ := s.attr("saige.error.transient"); !v.AsBool() {
					t.Error("rate limit not marked transient")
				}
				if got := s.int("gen_ai.usage.input_tokens"); got != 5 {
					t.Errorf("input tokens = %d, want 5", got)
				}
			},
		},
		{
			name:     "call error",
			provider: &fakeProvider{err: &types.ProviderError{Kind: types.ErrorKindContextLength, Err: errors.New("too long")}},
			wantErr:  true,
			check: func(t *testing.T, s *spySpan) {
				if got := s.str("error.type"); got != "context_length" {
					t.Errorf("error.type = %q, want context_length", got)
				}
				if v, _ := s.attr("saige.error.transient"); v.AsBool() {
					t.Error("context length marked transient")
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracer, rec := newSpyTracer()
			p := must.Get(NewTracedProvider(tt.provider, tracer))
			ch, err := p.Stream(context.Background(), types.Request{})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil {
				if got := drain(t, ch); len(got) != tt.wantCount {
					t.Fatalf("forwarded %d deltas, want %d", len(got), tt.wantCount)
				}
			}
			spans := rec.named("chat m1")
			if len(spans) != 1 {
				t.Fatalf("chat spans = %d, want 1", len(rec.all()))
			}
			if spans[0].endCount() != 1 {
				t.Fatalf("span ended %d times, want 1", spans[0].endCount())
			}
			wantModel := tt.wantModel
			if wantModel == "" {
				wantModel = "m1"
			}
			if got := spans[0].str("gen_ai.request.model"); got != wantModel {
				t.Errorf("request model = %q, want %q", got, wantModel)
			}
			tt.check(t, spans[0])
		})
	}
}

func TestTracedProviderTimeToFirstChunk(t *testing.T) {
	const delay = 30 * time.Millisecond
	var log []recordEvent
	m, err := NewMetrics(spyMeter{created: new([]string), log: &log})
	if err != nil {
		t.Fatal(err)
	}
	tracer, rec := newSpyTracer()
	inner := &fakeProvider{delay: delay, deltas: []types.Delta{types.PartStart{Index: 0, Kind: types.KindText}, types.PartDelta{Index: 0, Text: "a"}, types.PartDelta{Index: 0, Text: "b"}}}
	p := must.Get(NewTracedProvider(inner, tracer, WithProviderMetrics(m)))
	ch, err := p.Stream(context.Background(), types.Request{})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, ch)

	v, ok := rec.all()[0].attr("gen_ai.response.time_to_first_chunk")
	if !ok {
		t.Fatal("time_to_first_chunk not set")
	}
	if got := time.Duration(v.AsFloat64() * float64(time.Second)); got < delay {
		t.Errorf("time_to_first_chunk = %v, want at least %v (the delay before ChatStream returned)", got, delay)
	}
	var records int
	for _, ev := range log {
		if ev.instrument == "gen_ai.client.operation.time_to_first_chunk" {
			records++
			if ev.value < delay.Seconds() {
				t.Errorf("histogram value = %v, want at least %v", ev.value, delay.Seconds())
			}
		}
	}
	if records != 1 {
		t.Errorf("time_to_first_chunk records = %d, want 1", records)
	}
}

func TestTracedProviderEntryPoints(t *testing.T) {
	temp := 0.2
	maxTokens := int64(64)
	opts := types.RequestOptions{Temperature: &temp, MaxOutputTokens: &maxTokens, ToolChoice: &types.ToolChoice{Mode: types.ToolChoiceRequired}}

	t.Run("options forwarded", func(t *testing.T) {
		tracer, rec := newSpyTracer()
		inner := &optionsProvider{fakeProvider{deltas: []types.Delta{types.DoneDelta{}}}}
		ch, err := must.Get(NewTracedProvider(inner, tracer)).Stream(context.Background(), types.Request{Options: &opts})
		if err != nil {
			t.Fatal(err)
		}
		drain(t, ch)
		if inner.opts == nil || inner.opts.ToolChoice == nil || inner.opts.ToolChoice.Mode != types.ToolChoiceRequired {
			t.Fatalf("options did not reach the inner provider: %+v", inner.opts)
		}
		s := rec.all()[0]
		if v, _ := s.attr("gen_ai.request.temperature"); v.AsFloat64() != temp {
			t.Errorf("temperature = %v", v.AsFloat64())
		}
		if got := s.int("gen_ai.request.max_tokens"); got != maxTokens {
			t.Errorf("max_tokens = %d", got)
		}
		if got := s.str("saige.request.tool_choice"); got != "required" {
			t.Errorf("tool_choice = %q", got)
		}
	})

	t.Run("options rejected when unsupported", func(t *testing.T) {
		tracer, rec := newSpyTracer()
		_, err := must.Get(NewTracedProvider(&fakeProvider{}, tracer)).Stream(context.Background(), types.Request{Options: &opts})
		if !errors.Is(err, types.ErrInvalidModelConfig) || !errors.Is(err, types.ErrOptionsUnsupported) {
			t.Fatalf("err = %v, want ErrOptionsUnsupported", err)
		}
		if s := rec.all()[0]; s.endCount() != 1 || s.status != codes.Error {
			t.Fatalf("span ended %d times with status %v", s.endCount(), s.status)
		}
	})

	t.Run("schema", func(t *testing.T) {
		tracer, rec := newSpyTracer()
		inner := &schemaProvider{fakeProvider{deltas: []types.Delta{types.DoneDelta{}}}}
		ch, err := must.Get(NewTracedProvider(inner, tracer)).Stream(context.Background(), types.Request{Schema: &types.ParameterSchema{Type: "object"}})
		if err != nil {
			t.Fatal(err)
		}
		drain(t, ch)
		s := rec.all()[0]
		if got := s.str("gen_ai.output.type"); got != "json" {
			t.Errorf("output type = %q", got)
		}
		if s.endCount() != 1 {
			t.Errorf("span ended %d times", s.endCount())
		}
	})

	t.Run("schema rejected when unsupported", func(t *testing.T) {
		tracer, _ := newSpyTracer()
		inner := &fakeProvider{deltas: []types.Delta{types.DoneDelta{}}}
		p := must.Get(NewTracedProvider(inner, tracer))
		_, err := p.Stream(context.Background(), types.Request{Schema: &types.ParameterSchema{Type: "object"}})
		if !errors.Is(err, types.ErrSchemaUnsupported) || !errors.Is(err, types.ErrInvalidModelConfig) {
			t.Fatalf("err = %v, want ErrSchemaUnsupported", err)
		}
		if types.IsTransient(err) {
			t.Fatal("an unsupported schema must not be retried")
		}
		// Without a schema the call is a plain chat.
		ch, err := p.Stream(context.Background(), types.Request{})
		if err != nil {
			t.Fatal(err)
		}
		drain(t, ch)
	})

	t.Run("wrapper survives model switch and session", func(t *testing.T) {
		tracer, _ := newSpyTracer()
		p := must.Get(NewTracedProvider(switchingFake{&fakeProvider{}}, tracer))
		if _, ok := must.Get(p.WithTarget(types.ModelTarget("other"))).(*TracedProvider); !ok {
			t.Error("WithTarget dropped tracing")
		}
		if _, ok := p.NewSession().(*TracedProvider); !ok {
			t.Error("NewSession dropped tracing")
		}
		if p.Unwrap() != p.Inner {
			t.Error("Unwrap does not return the inner provider")
		}
	})
}

func TestRedactorScrubsErrorMessages(t *testing.T) {
	const secret = "alice@example.com"
	redact := func(s string) string { return strings.ReplaceAll(s, secret, "[email]") }
	err := fmt.Errorf("request for %s failed", secret)

	check := func(t *testing.T, s *spySpan) {
		t.Helper()
		if strings.Contains(s.statusDesc, secret) {
			t.Errorf("status description leaks the secret: %q", s.statusDesc)
		}
		if len(s.errs) != 1 || strings.Contains(s.errs[0].Error(), secret) {
			t.Errorf("recorded error leaks the secret: %v", s.errs)
		}
		if !strings.Contains(s.statusDesc, "[email]") {
			t.Errorf("status description = %q, want the redacted text", s.statusDesc)
		}
	}

	t.Run("provider", func(t *testing.T) {
		tracer, rec := newSpyTracer()
		_, _ = must.Get(NewTracedProvider(&fakeProvider{err: err}, tracer, WithProviderRedactor(redact))).Stream(context.Background(), types.Request{})
		check(t, rec.all()[0])
	})
	t.Run("tool", func(t *testing.T) {
		tracer, rec := newSpyTracer()
		_, _ = NewTracedTool(plainTool{name: "t", err: err}, tracer, WithToolRedactor(redact)).Execute(context.Background(), nil)
		check(t, rec.all()[0])
	})
}

func TestErrorType(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{&types.ProviderError{Kind: types.ErrorKindTransient, Err: errors.New("x")}, "transient"},
		{&types.ProviderError{Kind: types.ErrorKindPermanent, Err: errors.New("x")}, "permanent"},
		{fmt.Errorf("wrapped: %w", &types.ProviderError{Kind: types.ErrorKindAuth, Err: errors.New("x")}), "auth"},
		{&types.ProviderError{Kind: types.ErrorKindUnavailable, Err: errors.New("x")}, "unavailable"},
		{&types.ResponseTruncatedError{}, "truncated"},
		{context.Canceled, "canceled"},
		{fmt.Errorf("step: %w", context.DeadlineExceeded), "timeout"},
		{errors.New("plain"), "*errors.errorString"},
		{fmt.Errorf("call: %w", panicError{}), "panic"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := errorType(tt.err); got != tt.want {
				t.Errorf("errorType(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

// panicError stands for the agent's recovered tool panic.
type panicError struct{}

func (panicError) Error() string   { return "tool t panicked: boom" }
func (panicError) ToolPanic() bool { return true }

// switchingFake is a fakeProvider that can be re-targeted.
type switchingFake struct{ *fakeProvider }

func (f switchingFake) WithTarget(types.Target) (types.Provider, error) { return f, nil }
