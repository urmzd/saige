package router

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/urmzd/saige/agent/convert"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

// member is a profile's adapter: it reports an offering and records the
// messages it is sent.
type member struct {
	name     string
	offering types.Offering
	fail     error

	mu   sync.Mutex
	sent [][]types.Message
}

func (m *member) Name() string                          { return m.name }
func (m *member) Model() string                         { return m.name }
func (m *member) Offering() types.Offering              { return m.offering }
func (m *member) Capabilities() types.ModelCapabilities { return m.offering.Capabilities() }
func (m *member) Stream(_ context.Context, req types.Request) (<-chan types.Delta, error) {
	m.mu.Lock()
	m.sent = append(m.sent, req.Messages)
	m.mu.Unlock()
	if m.fail != nil {
		return deltas(types.ErrorDelta{Error: m.fail}), nil
	}
	return deltas(types.PartStart{Index: 0, Kind: types.KindText}, types.PartDelta{Index: 0, Text: "seen by " + m.name}, types.PartEnd{Index: 0}), nil
}

func (m *member) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent)
}

func vision(name string) types.Offering {
	return types.Offering{ID: name, Model: types.ModelInfo{Vendor: types.ProviderName(name), Prefix: types.ModelID(name), Known: true},
		Endpoint: types.EndpointInfo{Name: name},
		Modalities: types.Modalities{In: map[types.Modality]types.ModalityLimit{
			types.ModalityImage: {Media: []types.MediaType{types.MediaPNG}, Sources: []types.SourceKind{types.SourceInline}}}}}
}

func textOnly(name string) types.Offering {
	return types.Offering{ID: name, Model: types.ModelInfo{Vendor: types.ProviderName(name), Prefix: types.ModelID(name), Known: true},
		Endpoint: types.EndpointInfo{Name: name}}
}

// describer replaces an image with fixed text.
type describer struct{ calls int }

func (*describer) Name() string                         { return "describe" }
func (*describer) Version() string                      { return "1" }
func (*describer) Action() types.ModalityAction         { return types.ActDescribe }
func (*describer) Produces(types.Part) []types.Modality { return []types.Modality{types.ModalityText} }
func (*describer) Accepts(p types.Part) bool            { _, ok := p.(types.ImagePart); return ok }
func (*describer) Estimate(types.Part, types.Offering) (types.ConversionEstimate, error) {
	return types.ConversionEstimate{OutputTokens: 10}, nil
}
func (d *describer) Convert(context.Context, types.Part, types.ConvertEnv) ([]types.Part, types.ConversionUsage, error) {
	d.calls++
	return []types.Part{types.TextPart{Text: "a red square"}}, types.ConversionUsage{}, nil
}

func imageRequest() []types.Message {
	return []types.Message{types.UserMsg(types.Text("what is this?"), types.Image(types.Bytes(types.MediaPNG, []byte("png"))))}
}

func streamAll(t *testing.T, s *Session, msgs []types.Message) ([]types.Delta, error) {
	t.Helper()
	ch, err := s.Stream(context.Background(), types.Request{Messages: msgs})
	if err != nil {
		return nil, err
	}
	var out []types.Delta
	for d := range ch {
		if e, ok := d.(types.ErrorDelta); ok {
			err = e.Error
			continue
		}
		out = append(out, d)
	}
	return out, err
}

// A member whose plan rejects is removed from the request; a member whose
// plan converts is never removed.
func TestMemberRemovedOnRejectNeverOnConvert(t *testing.T) {
	text := &member{name: "texty", offering: textOnly("texty")}
	see := &member{name: "seer", offering: vision("seer")}
	r, err := New(Config{Profiles: []Profile{
		{ID: "texty", Provider: must.Get(convert.New(text, convert.Config{Policy: types.ConversionPolicy{}}))},
		{ID: "seer", Provider: must.Get(convert.New(see, convert.Config{Policy: types.ConversionPolicy{}}))},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ds, err := streamAll(t, r.Session(), imageRequest())
	if err != nil {
		t.Fatal(err)
	}
	if text.calls() != 0 || see.calls() != 1 {
		t.Fatalf("calls texty %d, seer %d; want the rejecting member skipped", text.calls(), see.calls())
	}
	if rd := ds[0].(types.RouteDelta); rd.Profile != "seer" || rd.Conversions != nil {
		t.Errorf("route = %+v, want seer with no conversions", rd)
	}

	// With describe permitted, the text-only member keeps its place.
	d := &describer{}
	pol := types.ConversionPolicy{Dial: types.ModalityDial{Per: map[types.Modality][]types.ModalityAction{types.ModalityImage: {types.ActDescribe}}},
		Converters: []types.Converter{d}}
	text2 := &member{name: "texty", offering: textOnly("texty")}
	r, _ = New(Config{Profiles: []Profile{
		{ID: "texty", Provider: must.Get(convert.New(text2, convert.Config{Policy: pol}))},
		{ID: "seer", Provider: must.Get(convert.New(see, convert.Config{Policy: types.ConversionPolicy{}}))},
	}})
	ds, err = streamAll(t, r.Session(), imageRequest())
	if err != nil {
		t.Fatal(err)
	}
	if text2.calls() != 1 {
		t.Fatal("a member that converts was removed")
	}
	rd := ds[0].(types.RouteDelta)
	if rd.Profile != "texty" || rd.Conversions == nil || rd.Conversions.Decisions[0].Action != types.DecisionDescribed {
		t.Fatalf("route = %+v, want texty with the planned description", rd)
	}
	cd, ok := ds[1].(types.ConversionDelta)
	if !ok || cd.Profile != "texty" || cd.Report.Decisions[0].Via != "describe@1" {
		t.Fatalf("second delta = %#v, want the executed report for texty", ds[1])
	}
	if got := text2.sent[0][0].(types.UserMessage).Parts[1].(types.TextPart).Text; got != "a red square" {
		t.Errorf("texty saw %q", got)
	}
}

func TestEveryMemberRejectingNamesEachReason(t *testing.T) {
	r, _ := New(Config{Profiles: []Profile{
		{ID: "a", Provider: must.Get(convert.New(&member{name: "a", offering: textOnly("a")}, convert.Config{Policy: types.ConversionPolicy{}}))},
		{ID: "b", Provider: must.Get(convert.New(&member{name: "b", offering: textOnly("b")}, convert.Config{Policy: types.ConversionPolicy{}}))},
	}})
	_, err := streamAll(t, r.Session(), imageRequest())
	if !errors.Is(err, types.ErrModalityUnsupported) || !strings.Contains(err.Error(), "profile a") || !strings.Contains(err.Error(), "profile b") {
		t.Fatalf("err = %v, want each member's rejection", err)
	}
}

// Failover re-plans for the member that serves: the vision member fails, and
// the text-only member describes the image before it is sent.
func TestFailoverReplansForTheNextMember(t *testing.T) {
	see := &member{name: "seer", offering: vision("seer"), fail: transient()}
	text := &member{name: "texty", offering: textOnly("texty")}
	d := &describer{}
	pol := types.ConversionPolicy{Dial: types.ModalityDial{Per: map[types.Modality][]types.ModalityAction{types.ModalityImage: {types.ActDescribe}}},
		Converters: []types.Converter{d}}
	r, _ := New(Config{Profiles: []Profile{
		{ID: "seer", Provider: must.Get(convert.New(see, convert.Config{Policy: pol}))},
		{ID: "texty", Provider: must.Get(convert.New(text, convert.Config{Policy: pol}))},
	}})
	ds, err := streamAll(t, r.Session(), imageRequest())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := see.sent[0][0].(types.UserMessage).Parts[1].(types.ImagePart); !ok {
		t.Error("the vision member was not sent the image")
	}
	if got := text.sent[0][0].(types.UserMessage).Parts[1].(types.TextPart).Text; got != "a red square" {
		t.Errorf("the text-only member saw %q, want the description", got)
	}
	var routes []types.RouteDelta
	var convs []types.ConversionDelta
	for _, x := range ds {
		switch v := x.(type) {
		case types.RouteDelta:
			routes = append(routes, v)
		case types.ConversionDelta:
			convs = append(convs, v)
		}
	}
	if len(routes) != 2 || routes[0].Conversions != nil || routes[1].Conversions == nil || routes[1].Reason != ReasonFailover {
		t.Fatalf("routes = %+v, want seer native then texty with a planned description", routes)
	}
	if len(convs) != 1 || convs[0].Profile != "texty" || d.calls != 1 {
		t.Fatalf("conversions = %+v (describer calls %d)", convs, d.calls)
	}
}
