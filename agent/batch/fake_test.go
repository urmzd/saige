package batch

import (
	"context"
	"errors"
	"iter"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// fakeVendor is an in-memory vendor batch API. Batches end when ended is
// set (or immediately with autoEnd), and results come back in reverse
// submission order, as vendors are free to return them in any order.
type fakeVendor struct {
	mu       sync.Mutex
	batches  map[string]*fakeBatch
	submits  int
	autoEnd  bool
	failNext error // returned by the next Submit, after the batch is created when created is set
	created  bool
	outcome  func(r types.BatchRequest) types.BatchOutcome
	drop     map[string]bool // custom IDs left out of the results
	pricing  types.Pricing
}

type fakeBatch struct {
	id      string
	tag     string
	reqs    []types.BatchRequest
	state   types.BatchState
	created time.Time
}

func newFakeVendor() *fakeVendor {
	return &fakeVendor{batches: map[string]*fakeBatch{}, autoEnd: true,
		pricing: types.Pricing{InputPerMTok: 2, OutputPerMTok: 10, BatchDiscount: 0.5, AsOf: "2026-10-09"}}
}

func (f *fakeVendor) Name() string  { return "fake" }
func (f *fakeVendor) Model() string { return "fake-model" }

func (f *fakeVendor) Capabilities() types.ModelCapabilities {
	return types.ModelCapabilities{Provider: "fake", Model: "fake-model", Pricing: f.pricing}
}

func (f *fakeVendor) Submit(_ context.Context, reqs []types.BatchRequest, opts types.BatchSubmitOptions) (types.BatchHandle, error) {
	if err := CheckIDs(reqs); err != nil {
		return types.BatchHandle{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submits++
	id := "fb_" + strconv.Itoa(f.submits)
	err := f.failNext
	f.failNext = nil
	if err != nil && !f.created {
		return types.BatchHandle{}, err
	}
	st := types.BatchRunning
	if f.autoEnd {
		st = types.BatchEnded
	}
	f.batches[id] = &fakeBatch{id: id, tag: opts.Tag, reqs: slices.Clone(reqs), state: st, created: time.Now()}
	if err != nil {
		return types.BatchHandle{}, err
	}
	return types.BatchHandle{Provider: "fake", Model: "fake-model", ID: id}, nil
}

func (f *fakeVendor) end(id string, st types.BatchState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches[id].state = st
}

func (f *fakeVendor) Status(_ context.Context, h types.BatchHandle) (types.BatchStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.batches[h.ID]
	if !ok {
		return types.BatchStatus{}, types.ErrBatchNotFound
	}
	return types.BatchStatus{State: b.state, Counts: types.BatchCounts{Total: len(b.reqs)}}, nil
}

func (f *fakeVendor) Results(_ context.Context, h types.BatchHandle) iter.Seq2[types.BatchResult, error] {
	return func(yield func(types.BatchResult, error) bool) {
		f.mu.Lock()
		b, ok := f.batches[h.ID]
		f.mu.Unlock()
		if !ok {
			yield(types.BatchResult{}, types.ErrBatchNotFound)
			return
		}
		for i := len(b.reqs) - 1; i >= 0; i-- {
			r := b.reqs[i]
			if f.drop[r.CustomID] {
				continue
			}
			outcome := types.BatchSucceeded
			if f.outcome != nil {
				outcome = f.outcome(r)
			}
			res := types.BatchResult{CustomID: r.CustomID, Outcome: outcome}
			if outcome == types.BatchSucceeded {
				res.Message = types.AssistantMsg(types.Text("answer to " + lastText(r.Messages)))
				res.Usage = types.UsageDelta{PromptTokens: 1000, CompletionTokens: 100, TotalTokens: 1100}
			} else {
				res.Err = &types.BatchRequestError{Outcome: outcome}
			}
			if !yield(res, nil) {
				return
			}
		}
	}
}

func (f *fakeVendor) Cancel(_ context.Context, h types.BatchHandle) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.batches[h.ID]
	if !ok {
		return types.ErrBatchNotFound
	}
	b.state = types.BatchCanceled
	return nil
}

func (f *fakeVendor) submitCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.submits
}

// findingVendor adds BatchFinder.
type findingVendor struct{ *fakeVendor }

func (f findingVendor) FindBatch(_ context.Context, q types.BatchQuery) (types.BatchHandle, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range f.batches {
		if b.tag == q.Tag && !b.created.Before(q.Since) {
			return types.BatchHandle{Provider: "fake", Model: "fake-model", ID: b.id}, true, nil
		}
	}
	return types.BatchHandle{}, false, nil
}

var errTransport = errors.New("connection reset by peer")

func lastText(msgs []types.Message) string {
	if len(msgs) == 0 {
		return ""
	}
	if u, ok := msgs[len(msgs)-1].(types.UserMessage); ok {
		for _, c := range u.Parts {
			if t, ok := c.(types.TextPart); ok {
				return t.Text
			}
		}
	}
	return ""
}

func requests(ids ...string) []types.BatchRequest {
	out := make([]types.BatchRequest, len(ids))
	for i, id := range ids {
		out[i] = types.BatchRequest{CustomID: id, Messages: []types.Message{types.UserMsg(types.Text("q-" + id))}}
	}
	return out
}
