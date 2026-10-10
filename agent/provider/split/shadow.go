package split

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// startShadows launches the sampled shadow calls for one request. Each call
// is detached from the caller's cancellation, bounded by its own timeout, and
// tracked so Drain and Close can wait for it.
func (s *Split) startShadows(ctx context.Context, q request, variant string) {
	cfg := s.shared.cfg
	if len(cfg.Shadow) == 0 {
		return
	}
	// The caller may reuse its slices after the request; the shadow keeps its own.
	q.messages = append([]types.Message(nil), q.messages...)
	q.tools = append([]types.ToolDef(nil), q.tools...)
	for _, sh := range cfg.Shadow {
		if sh.Rate <= 0 || cfg.Sample() >= sh.Rate {
			continue
		}
		s.shared.inflight.add()
		go func(sh ShadowArm) {
			defer s.shared.inflight.done()
			res := runShadow(context.WithoutCancel(ctx), sh, q)
			res.Experiment, res.Label, res.Variant = cfg.Experiment, sh.Label, variant
			if cfg.OnShadow != nil {
				cfg.OnShadow(res)
			}
		}(sh)
	}
}

// runShadow reserves from the shadow budget, calls the provider, drains its
// stream, and settles the reservation with the reported usage. Each call gets
// its own provider session: one shadow provider serves every split session,
// and a detached shadow can still be running when the next turn's starts, so
// a shared session-scoped provider would reject the overlap or mix state
// across conversations.
func runShadow(ctx context.Context, sh ShadowArm, q request) ShadowResult {
	timeout := sh.Timeout
	if timeout <= 0 {
		timeout = time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	caps, _ := types.ProviderCapabilities(sh.Provider)
	model := types.ProviderModel(sh.Provider)
	id := "shadow:" + sh.Label + ":" + types.NewID()
	if _, err := sh.Budget.Reserve(id, caps.Pricing); err != nil {
		return ShadowResult{Err: err}
	}
	start := time.Now()
	var res ShadowResult
	var text strings.Builder
	sawUsage := false
	src, err := q.call(ctx, types.NewProviderSession(sh.Provider))
	if err == nil && src != nil {
		for d := range src {
			switch v := d.(type) {
			case types.PartDelta:
				text.WriteString(v.Text)
			case types.PartStart:
				if v.Kind == types.KindToolCall {
					res.ToolCalls++
				}
			case types.UsageDelta:
				res.Usage = res.Usage.Merge(v)
				sawUsage = true
			case types.ErrorDelta:
				if err == nil {
					err = v.Error
				}
			}
		}
	}
	res.Text, res.Latency, res.Err = text.String(), time.Since(start), err
	if settleErr := sh.Budget.Settle(id, model, caps.Pricing, types.UsageFromDelta(res.Usage), !sawUsage); settleErr != nil && res.Err == nil {
		res.Err = settleErr
	}
	return res
}

// tracker counts in-flight shadow calls. Unlike sync.WaitGroup it allows new
// calls to start while another goroutine waits.
type tracker struct {
	mu   sync.Mutex
	n    int
	idle chan struct{} // closed while n is zero
}

func newTracker() *tracker {
	idle := make(chan struct{})
	close(idle)
	return &tracker{idle: idle}
}

func (t *tracker) add() {
	t.mu.Lock()
	if t.n == 0 {
		t.idle = make(chan struct{})
	}
	t.n++
	t.mu.Unlock()
}

func (t *tracker) done() {
	t.mu.Lock()
	t.n--
	if t.n == 0 {
		close(t.idle)
	}
	t.mu.Unlock()
}

// wait returns a channel that is closed once no call is in flight.
func (t *tracker) wait() <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.idle
}
