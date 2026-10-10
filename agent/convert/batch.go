package convert

import (
	"context"
	"fmt"
	"iter"

	"github.com/urmzd/saige/agent/types"
)

// Batch is the conversion decorator of a batch provider. Submit plans every
// request against the offering the batch serves, the way Provider plans a
// streaming call, and rejects the whole batch when any request's plan
// rejects, before anything is uploaded. It then runs the planned
// conversions on a copy of each request's messages and submits the copies.
// The caller's requests are never modified, so a job's manifest, which the
// batch runner computes over them, names the original parts.
//
// The conversion runtime is read from the context Submit is called with
// (WithRuntime): its policy, dial layers and budget apply to every request.
// The modality dial is spent here and removed from each request's options.
//
// Wrap a vendor batch provider, not a local one: a local batch makes
// ordinary calls through its provider, which converts them itself.
type Batch struct {
	inner types.BatchProvider
	conv  *Provider
}

var _ types.BatchProvider = (*Batch)(nil)

// NewBatch wraps inner with policy. The offering is the one inner reports
// as a provider (an adapter is both); a batch provider that reports none
// is not planned, and its requests pass through. layers are the modality
// dial layers inner was built with.
func NewBatch(inner types.BatchProvider, policy types.ConversionPolicy, layers ...types.DialLayer) types.BatchProvider {
	p, _ := inner.(types.Provider)
	return wrapBatch(inner, New(p, policy, layers...))
}

// Batch returns the batch decorator for inner with this decorator's policy,
// layers and cache, planning against the offering of the provider this
// decorator wraps. Use it for the batch provider behind a provider built by
// provider.Build.
func (p *Provider) Batch(inner types.BatchProvider) types.BatchProvider {
	return wrapBatch(inner, p)
}

func wrapBatch(inner types.BatchProvider, conv *Provider) types.BatchProvider {
	b := &Batch{inner: inner, conv: conv}
	if _, ok := inner.(types.BatchFinder); ok {
		return &findingBatch{b}
	}
	return b
}

// Submit implements types.BatchProvider. A rejection names the request and
// matches types.ErrInvalidModelConfig, so the batch runner records the job
// as failed without having sent it.
func (b *Batch) Submit(ctx context.Context, requests []types.BatchRequest, opts types.BatchSubmitOptions) (types.BatchHandle, error) {
	rt, _ := RuntimeFrom(ctx)
	plans := make([]*Plan, len(requests))
	for i, r := range requests {
		o := r.Options
		pl, _, ok, err := b.conv.plan(ctx, types.Request{Messages: r.Messages, Tools: r.Tools, Schema: r.Schema, Options: &o})
		if err != nil {
			return types.BatchHandle{}, fmt.Errorf("batch request %q: %w", r.CustomID, err)
		}
		if ok && pl.Converts() {
			plans[i] = &pl
		}
	}
	out := make([]types.BatchRequest, len(requests))
	for i, r := range requests {
		q := r
		q.Options = r.Options.WithoutModality()
		if pl := plans[i]; pl != nil {
			msgs, _, err := pl.Apply(ctx, r.Messages, rt, b.conv.policyFor(rt).Cache)
			if err != nil {
				return types.BatchHandle{}, fmt.Errorf("batch request %q: %w", r.CustomID, err)
			}
			q.Messages = msgs
		}
		out[i] = q
	}
	return b.inner.Submit(ctx, out, opts)
}

// Status implements types.BatchProvider by forwarding.
func (b *Batch) Status(ctx context.Context, h types.BatchHandle) (types.BatchStatus, error) {
	return b.inner.Status(ctx, h)
}

// Results implements types.BatchProvider by forwarding.
func (b *Batch) Results(ctx context.Context, h types.BatchHandle) iter.Seq2[types.BatchResult, error] {
	return b.inner.Results(ctx, h)
}

// Cancel implements types.BatchProvider by forwarding.
func (b *Batch) Cancel(ctx context.Context, h types.BatchHandle) error { return b.inner.Cancel(ctx, h) }

// Name reports the inner batch provider's name, so a batch runner keys its
// jobs by the vendor that serves them.
func (b *Batch) Name() string {
	if n, ok := b.inner.(interface{ Name() string }); ok {
		return n.Name()
	}
	return "unknown"
}

// Model reports the inner batch provider's model.
func (b *Batch) Model() string {
	if m, ok := b.inner.(interface{ Model() string }); ok {
		return m.Model()
	}
	return ""
}

// Capabilities forwards the inner batch provider's capabilities, which the
// batch runner prices requests by.
func (b *Batch) Capabilities() types.ModelCapabilities {
	if c, ok := b.inner.(interface {
		Capabilities() types.ModelCapabilities
	}); ok {
		return c.Capabilities()
	}
	return types.ModelCapabilities{}
}

// Unwrap returns the inner batch provider.
func (b *Batch) Unwrap() types.BatchProvider { return b.inner }

// findingBatch is a Batch over a provider that can look up a batch after a
// crash lost its handle.
type findingBatch struct{ *Batch }

var _ types.BatchFinder = (*findingBatch)(nil)

// FindBatch implements types.BatchFinder by forwarding.
func (b *findingBatch) FindBatch(ctx context.Context, q types.BatchQuery) (types.BatchHandle, bool, error) {
	return b.inner.(types.BatchFinder).FindBatch(ctx, q)
}
