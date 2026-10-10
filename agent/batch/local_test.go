package batch

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// echoProvider answers "echo: <last user text>", tracking concurrency.
type echoProvider struct {
	active, peak atomic.Int32
	delay        time.Duration
	mu           sync.Mutex
	schemas      int
	options      int
}

func (p *echoProvider) Name() string  { return "echo" }
func (p *echoProvider) Model() string { return "echo-1" }

func (p *echoProvider) answer(ctx context.Context, msgs []types.Message) (<-chan types.Delta, error) {
	n := p.active.Add(1)
	for {
		peak := p.peak.Load()
		if n <= peak || p.peak.CompareAndSwap(peak, n) {
			break
		}
	}
	out := make(chan types.Delta, 8)
	go func() {
		defer close(out)
		defer p.active.Add(-1)
		select {
		case <-time.After(p.delay):
		case <-ctx.Done():
			out <- types.ErrorDelta{Error: ctx.Err()}
			return
		}
		out <- types.PartStart{Index: 0, Kind: types.KindText}
		out <- types.PartDelta{Index: 0, Text: "echo: " + lastText(msgs)}
		out <- types.PartEnd{Index: 0}
		out <- types.UsageDelta{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5, FinishReasons: []string{"stop"}}
	}()
	return out, nil
}

func (p *echoProvider) chatStream(ctx context.Context, msgs []types.Message, _ []types.ToolDef) (<-chan types.Delta, error) {
	return p.answer(ctx, msgs)
}

// Stream implements types.Provider.
func (p *echoProvider) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	if req.Options != nil {
		return p.chatStreamWithOptions(ctx, req.Messages, req.Tools, *req.Options)
	}
	if req.Schema != nil {
		return p.chatStreamWithSchema(ctx, req.Messages, req.Tools, req.Schema)
	}
	return p.chatStream(ctx, req.Messages, req.Tools)
}

// SupportsOptions implements types.OptionsProvider.
func (p *echoProvider) SupportsOptions() bool { return true }

// SupportsSchema implements types.StructuredOutputProvider.
func (p *echoProvider) SupportsSchema() bool { return true }

func (p *echoProvider) chatStreamWithSchema(ctx context.Context, msgs []types.Message, _ []types.ToolDef, _ *types.ParameterSchema) (<-chan types.Delta, error) {
	p.mu.Lock()
	p.schemas++
	p.mu.Unlock()
	return p.answer(ctx, msgs)
}

func (p *echoProvider) chatStreamWithOptions(ctx context.Context, msgs []types.Message, _ []types.ToolDef, _ types.RequestOptions) (<-chan types.Delta, error) {
	p.mu.Lock()
	p.options++
	p.mu.Unlock()
	return p.answer(ctx, msgs)
}

// plainProvider has no schema or options support.
type plainProvider struct{ inner echoProvider }

func (p *plainProvider) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	msgs, tools := req.Messages, req.Tools
	return p.inner.Stream(ctx, types.Request{Messages: msgs, Tools: tools})
}

func TestLocalBoundedConcurrency(t *testing.T) {
	p := &echoProvider{delay: 20 * time.Millisecond}
	l := NewLocal(p, 3)
	ctx := context.Background()
	reqs := requests("a", "b", "c", "d", "e", "f", "g", "h")
	reqs[1].Schema = &types.ParameterSchema{Type: "object"}
	temp := 0.2
	reqs[2].Options = types.RequestOptions{Temperature: &temp}
	h, err := l.Submit(ctx, reqs, types.BatchSubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]types.BatchResult{}
	for res, err := range l.Results(ctx, h) {
		if err != nil {
			t.Fatal(err)
		}
		got[res.CustomID] = res
	}
	if len(got) != len(reqs) {
		t.Fatalf("results = %d, want %d", len(got), len(reqs))
	}
	if got["d"].Text() != "echo: q-d" || got["d"].Usage.TotalTokens != 5 {
		t.Fatalf("result d = %+v", got["d"])
	}
	if peak := p.peak.Load(); peak > 3 || peak < 2 {
		t.Fatalf("peak concurrency = %d, want 2..3", peak)
	}
	if p.schemas != 1 || p.options != 1 {
		t.Fatalf("schema calls %d, option calls %d, want 1 each", p.schemas, p.options)
	}
	st, err := l.Status(ctx, h)
	if err != nil || st.State != types.BatchEnded || st.Counts.Succeeded != len(reqs) {
		t.Fatalf("status = %+v, %v", st, err)
	}
	if _, err := l.Status(ctx, types.BatchHandle{ID: "nope"}); !errors.Is(err, types.ErrBatchNotFound) {
		t.Fatalf("unknown handle err = %v", err)
	}
}

// TestLocalRejectsAtSubmit checks D-12: a request the wrapped provider
// cannot express is refused before any call.
func TestLocalRejectsAtSubmit(t *testing.T) {
	p := &plainProvider{}
	l := NewLocal(p, 2)
	reqs := requests("a")
	reqs[0].Schema = &types.ParameterSchema{Type: "object"}
	if _, err := l.Submit(context.Background(), reqs, types.BatchSubmitOptions{}); !errors.Is(err, types.ErrInvalidModelConfig) {
		t.Fatalf("err = %v, want ErrInvalidModelConfig", err)
	}
	both := requests("a")
	both[0].Schema = &types.ParameterSchema{Type: "object"}
	temp := 0.1
	both[0].Options.Temperature = &temp
	if _, err := NewLocal(&echoProvider{}, 1).Submit(context.Background(), both, types.BatchSubmitOptions{}); !errors.Is(err, types.ErrOptionsUnsupported) {
		t.Fatalf("schema with options err = %v", err)
	}
	if _, err := l.Submit(context.Background(), requests("a", "a"), types.BatchSubmitOptions{}); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("duplicate err = %v", err)
	}
	if p.inner.peak.Load() != 0 {
		t.Fatal("a rejected batch made calls")
	}
}

func TestLocalCancel(t *testing.T) {
	p := &echoProvider{delay: time.Second}
	l := NewLocal(p, 1)
	ctx := context.Background()
	h, err := l.Submit(ctx, requests("a", "b", "c"), types.BatchSubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := l.Cancel(ctx, h); err != nil {
		t.Fatal(err)
	}
	canceled := 0
	for res, err := range l.Results(ctx, h) {
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome == types.BatchCanceledOutcome {
			canceled++
		}
	}
	if canceled != 3 {
		t.Fatalf("canceled = %d, want 3", canceled)
	}
	if st, _ := l.Status(ctx, h); st.State != types.BatchCanceled {
		t.Fatalf("state = %s", st.State)
	}
}

// TestLocalPricedInteractive checks that a local batch is charged at the
// interactive rates: its calls are ordinary calls.
func TestLocalPricedInteractive(t *testing.T) {
	r := NewRunner(NewLocal(&echoProvider{}, 1), NewMemoryStore(), WithPricing(types.Pricing{InputPerMTok: 2, BatchDiscount: 0.5}))
	if got := r.batchPricing().InputPerMTok; got != 2 {
		t.Fatalf("input rate = %v, want 2", got)
	}
	r = NewRunner(newFakeVendor(), NewMemoryStore())
	if got := r.batchPricing().InputPerMTok; got != 1 {
		t.Fatalf("vendor batch input rate = %v, want 1", got)
	}
}
