package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

// offeringProvider declares an offering and records what each call sent.
type offeringProvider struct {
	name     string
	offering types.Offering
	pricing  types.Pricing
	thinking *types.ThinkingPart

	mu   sync.Mutex
	sent [][]types.Message
}

func (p *offeringProvider) Name() string             { return p.name }
func (p *offeringProvider) Model() string            { return p.name + "-model" }
func (p *offeringProvider) Offering() types.Offering { return p.offering }
func (p *offeringProvider) Capabilities() types.ModelCapabilities {
	mc := p.offering.Capabilities()
	mc.Pricing = p.pricing
	return mc
}
func (p *offeringProvider) Stream(_ context.Context, req types.Request) (<-chan types.Delta, error) {
	p.mu.Lock()
	p.sent = append(p.sent, req.Messages)
	p.mu.Unlock()
	ch := make(chan types.Delta, 8)
	i := 0
	if p.thinking != nil {
		for _, d := range types.PartDeltas(i, *p.thinking) {
			ch <- d
		}
		i++
	}
	ch <- types.PartStart{Index: i, Kind: types.KindText}
	ch <- types.PartDelta{Index: i, Text: "ok"}
	ch <- types.PartEnd{Index: i}
	ch <- types.UsageDelta{PromptTokens: 10, CompletionTokens: 2}
	close(ch)
	return ch, nil
}

func (p *offeringProvider) lastSent() []types.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sent[len(p.sent)-1]
}

func textModel(name string) types.Offering {
	return types.Offering{ID: "acme/" + name + "@acme", Model: types.ModelInfo{Vendor: "acme", Prefix: types.ModelID(name), Known: true},
		Endpoint: types.EndpointInfo{Name: "acme"}}
}

func runConverting(t *testing.T, a *Agent, msgs ...types.Message) ([]types.Delta, error) {
	t.Helper()
	stream := a.Invoke(context.Background(), msgs)
	var ds []types.Delta
	for d := range stream.Deltas() {
		ds = append(ds, d)
	}
	return ds, stream.Wait()
}

func doc(text string) types.DocumentPart {
	return types.DocumentPart{Source: types.Bytes("text/x-notes", []byte(text))}
}

// Reject is the default: a part the model cannot take fails the call
// instead of reaching it as a placeholder.
func TestAgentRejectsUnsupportedMediaByDefault(t *testing.T) {
	p := &offeringProvider{name: "acme", offering: textModel("t")}
	a := must.Get(New(Config{Provider: p}))
	_, err := runConverting(t, a, types.UserMsg(types.Text("read this"), doc("notes")))
	if !errors.Is(err, types.ErrModalityUnsupported) {
		t.Fatalf("err = %v, want ErrModalityUnsupported", err)
	}
	if len(p.sent) != 0 {
		t.Error("the rejected request reached the provider")
	}
}

// WithExtractors permits extract for the extractor's modality: the provider
// sees the text, the conversation keeps the document, and the turn records
// what was converted.
func TestAgentExtractorsConvertTheView(t *testing.T) {
	p := &offeringProvider{name: "acme", offering: textModel("t")}
	upper := types.ExtractorFunc(func(_ context.Context, data []byte, _ types.MediaType) ([]types.UserPart, error) {
		return []types.UserPart{types.Text("EXTRACTED: " + strings.ToUpper(string(data)))}, nil
	})
	a := must.Get(New(Config{Provider: p}, WithExtractors(map[types.MediaType]types.Extractor{"text/x-notes": upper})))
	ds, err := runConverting(t, a, types.UserMsg(types.Text("read this"), doc("notes")))
	if err != nil {
		t.Fatal(err)
	}
	sent := p.lastSent()[len(p.lastSent())-1].(types.UserMessage).Parts
	if got := sent[1].(types.TextPart).Text; got != "EXTRACTED: NOTES" {
		t.Fatalf("provider saw %q, want the extraction", got)
	}
	var conv *types.ConversionDelta
	for _, d := range ds {
		if c, ok := d.(types.ConversionDelta); ok {
			conv = &c
		}
	}
	if conv == nil || conv.Report.Decisions[0].Action != types.DecisionExtracted || conv.Report.Decisions[0].Scope != types.DialScopeAgent {
		t.Fatalf("conversion delta = %+v", conv)
	}
	msgs, _ := a.Tree().FlattenBranch("main")
	var keptDoc bool
	var route *types.RoutePart
	for _, m := range msgs {
		for _, part := range types.PartsOf(m) {
			switch v := part.(type) {
			case types.DocumentPart:
				keptDoc = true
			case types.RoutePart:
				route = &v
			}
		}
	}
	if !keptDoc {
		t.Error("the stored history lost the original document")
	}
	if route == nil || route.Conversions == nil || route.Conversions.Hash != conv.Report.Hash || route.Provider != "acme" {
		t.Fatalf("route part = %+v, want the executed report", route)
	}
}

// The modality dial at agent scope works for a provider that takes no
// request options: it is spent by the conversion step, not sent.
func TestAgentModalityDialOmits(t *testing.T) {
	p := &offeringProvider{name: "acme", offering: textModel("t")}
	omit := types.ModalityDial{Per: map[types.Modality][]types.ModalityAction{types.ModalityDocument: {types.ActOmit}}}
	a := must.Get(New(Config{Provider: p}, WithDials(types.Dials{Modality: &omit})))
	if _, err := runConverting(t, a, types.UserMsg(types.Text("read this"), doc("notes"))); err != nil {
		t.Fatal(err)
	}
	notice := p.lastSent()[len(p.lastSent())-1].(types.UserMessage).Parts[1].(types.TextPart).Text
	if !strings.Contains(notice, "omitted") || !strings.Contains(notice, "text/x-notes") {
		t.Fatalf("notice = %q", notice)
	}
}

// Reasoning is recorded with the provider that signed it, so a later
// attempt on Anthropic can leave another vendor's reasoning out.
func TestAgentRecordsThinkingOrigin(t *testing.T) {
	p := &offeringProvider{name: "google", offering: textModel("t"), thinking: &types.ThinkingPart{Text: "hmm", Signature: "sig"}}
	a := must.Get(New(Config{Provider: p}))
	if _, err := runConverting(t, a, types.UserMsg(types.Text("q"))); err != nil {
		t.Fatal(err)
	}
	msgs, _ := a.Tree().FlattenBranch("main")
	for _, m := range msgs {
		for _, part := range types.PartsOf(m) {
			if th, ok := part.(types.ThinkingPart); ok {
				if th.Origin != "google" {
					t.Fatalf("origin = %q, want google", th.Origin)
				}
				return
			}
		}
	}
	t.Fatal("no reasoning recorded")
}

// meteredExtractor is a converter that calls a model: it is priced, so its
// call is reserved from the turn's budget reservation and settled on its own.
type meteredExtractor struct {
	mu    sync.Mutex
	calls int
}

func (*meteredExtractor) Name() string                 { return "metered" }
func (*meteredExtractor) Version() string              { return "1" }
func (*meteredExtractor) Action() types.ModalityAction { return types.ActExtract }
func (*meteredExtractor) Produces(types.Part) []types.Modality {
	return []types.Modality{types.ModalityText}
}
func (*meteredExtractor) Accepts(p types.Part) bool { _, ok := p.(types.DocumentPart); return ok }
func (*meteredExtractor) Pricing() types.Pricing {
	return types.Pricing{InputPerMTok: 1, OutputPerMTok: 1}
}
func (*meteredExtractor) Estimate(types.Part, types.Offering) (types.ConversionEstimate, error) {
	return types.ConversionEstimate{InputTokens: 100, OutputTokens: 100, Cost: 200}, nil
}
func (m *meteredExtractor) Convert(context.Context, types.Part, types.ConvertEnv) ([]types.Part, types.ConversionUsage, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	return []types.Part{types.TextPart{Text: "metered text"}},
		types.ConversionUsage{Usage: types.UsageDelta{PromptTokens: 50, CompletionTokens: 30}, Pricing: types.Pricing{InputPerMTok: 1, OutputPerMTok: 1}, Model: "meter"}, nil
}

// A conversion is charged to the run's budget, and replaying the turn
// restores that charge without converting again.
func TestAgentConversionBudgetAndReplay(t *testing.T) {
	m := &meteredExtractor{}
	pol := types.ConversionPolicy{Dial: types.ModalityDial{Per: map[types.Modality][]types.ModalityAction{types.ModalityDocument: {types.ActExtract}}},
		Converters: []types.Converter{m}}
	runner := newRecordingRunner()
	run := func() *types.Budget {
		b := types.NewBudget(types.BudgetPolicy{Limit: types.USD(1), PerCallCost: 1000})
		p := &offeringProvider{name: "acme", offering: textModel("t"), pricing: types.Pricing{InputPerMTok: 1, OutputPerMTok: 1}}
		a := must.Get(New(Config{Provider: p, Budget: b}, WithConversion(pol)))
		if _, err := a.RunDurable(context.Background(), runner, []types.Message{types.UserMsg(types.Text("read"), doc("notes"))}, ""); err != nil {
			t.Fatal(err)
		}
		return b
	}
	b1 := run()
	// 50+30 conversion tokens and 10+2 call tokens at $1 per million.
	if got := b1.Spent(); got != 92 {
		t.Fatalf("spent %v, want the conversion and the call (92 micro-units)", int64(got))
	}
	b2 := run()
	if m.calls != 1 {
		t.Fatalf("the replay converted again (%d calls)", m.calls)
	}
	if got := b2.Spent(); got != 92 {
		t.Fatalf("replayed budget spent %v, want the recorded 92", int64(got))
	}
}
