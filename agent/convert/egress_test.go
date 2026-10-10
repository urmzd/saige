package convert

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/privacy"
	"github.com/urmzd/saige/agent/types"
)

// egressFake is a priced converter that reports the offering it sends the
// part to.
type egressFake struct {
	pricedFake
	off types.Offering
}

func (e egressFake) EgressOffering() (types.Offering, bool) { return e.off, true }

// ctxFake records whether its context still carried a privacy boundary.
type ctxFake struct {
	*fake
	sawEgress bool
}

func (c *ctxFake) Convert(ctx context.Context, p types.Part, env types.ConvertEnv) ([]types.Part, types.ConversionUsage, error) {
	_, c.sawEgress = types.EgressFrom(ctx)
	return c.fake.Convert(ctx, p, env)
}

func describeImages(c types.Converter) (types.ConversionPolicy, types.DialLayer) {
	return types.ConversionPolicy{Converters: []types.Converter{c}},
		layer(types.DialScopeAgent, per(types.ModalityImage, types.ActDescribe, types.ActOmit))
}

func sentText(t *testing.T, s *stubProvider) string {
	t.Helper()
	var b strings.Builder
	for _, m := range s.last() {
		for _, p := range types.PartsOf(m) {
			if tp, ok := p.(types.TextPart); ok {
				b.WriteString(tp.Text + "\n")
			}
		}
	}
	return b.String()
}

func TestEgressTokenizesConverterOutput(t *testing.T) {
	inner := &stubProvider{name: "texty", offering: textOnly()}
	c := &fake{action: types.ActDescribe, media: types.ModalityImage, text: "a badge reading ada@example.com"}
	cache := NewMemoryCache(0)
	pol, l := describeImages(c)
	pol.Cache = cache
	p := New(inner, pol, l)
	v := privacy.NewVault(nil)
	ctx := types.WithEgress(context.Background(), types.Egress{Vault: v})
	msgs := []types.Message{types.UserMsg(types.Text("what is this?"), pngPart("badge"))}
	ch, err := p.Stream(ctx, types.Request{Messages: msgs})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := collect(ch); err != nil {
		t.Fatal(err)
	}
	got := sentText(t, inner)
	if strings.Contains(got, "ada@example.com") {
		t.Fatalf("raw personal data reached the provider:\n%s", got)
	}
	if !strings.Contains(got, "<<EMAIL_1>>") {
		t.Fatalf("converter output was not tokenized:\n%s", got)
	}
	if v.Restore(got) == got {
		t.Error("placeholders are not the vault's")
	}
	// The cache keeps what the converter made, so another session's vault
	// tokenizes it with its own placeholders.
	found := false
	for _, el := range cache.items {
		for _, part := range el.Value.(*cacheItem).entry.Parts {
			if strings.Contains(part.(types.TextPart).Text, "ada@example.com") {
				found = true
			}
		}
	}
	if !found {
		t.Error("the cache holds tokenized text; it must hold the converter's output")
	}
}

func TestEgressTokenizesOmittedNotice(t *testing.T) {
	inner := &stubProvider{name: "texty", offering: textOnly()}
	p := New(inner, types.ConversionPolicy{}, layer(types.DialScopeAgent, per(types.ModalityImage, types.ActOmit)))
	img := types.ImagePart{Source: types.Bytes(types.MediaPNG, []byte("png"))}
	img.Source.Filename = "bob@example.com.png"
	ctx := types.WithEgress(context.Background(), types.Egress{Vault: privacy.NewVault(nil)})
	ch, err := p.Stream(ctx, types.Request{Messages: []types.Message{types.UserMsg(img)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := collect(ch); err != nil {
		t.Fatal(err)
	}
	if got := sentText(t, inner); strings.Contains(got, "bob@example.com") || !strings.Contains(got, "omitted") {
		t.Fatalf("notice = %q, want the file name tokenized", got)
	}
}

func TestEgressRequireTextRejectsMediaThatWouldLeave(t *testing.T) {
	ctx := types.WithEgress(context.Background(), types.Egress{Vault: privacy.NewVault(nil), RequireText: true})
	msgs := []types.Message{types.UserMsg(types.Text("look"), pngPart("x"))}

	// A vision model would receive the image natively.
	inner := &stubProvider{name: "chat", offering: visionChat()}
	p := New(inner, types.ConversionPolicy{})
	if _, _, err := p.PlanConversions(ctx, types.Request{Messages: msgs}); !errors.Is(err, types.ErrModalityUnsupported) {
		t.Fatalf("plan err = %v, want a rejection a router removes the member for", err)
	}
	if _, err := p.Stream(ctx, types.Request{Messages: msgs}); !errors.Is(err, types.ErrModalityUnsupported) {
		t.Fatalf("stream err = %v, want a rejection", err)
	}
	if inner.calls() != 0 {
		t.Fatal("the request was sent")
	}

	// Outside the boundary the same request is native.
	if _, _, err := p.PlanConversions(context.Background(), types.Request{Messages: msgs}); err != nil {
		t.Fatalf("plan without boundary: %v", err)
	}

	// A text model that describes the image serves it.
	text := &stubProvider{name: "texty", offering: textOnly()}
	pol, l := describeImages(&fake{action: types.ActDescribe, media: types.ModalityImage, text: "a cat"})
	ch, err := New(text, pol, l).Stream(ctx, types.Request{Messages: msgs})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := collect(ch); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sentText(t, text), "a cat") {
		t.Fatal("the description was not sent")
	}
}

func TestEgressRequireTextRefusesAnUnclearedConverter(t *testing.T) {
	ctx := types.WithEgress(context.Background(), types.Egress{Vault: privacy.NewVault(nil), RequireText: true})
	msgs := []types.Message{types.UserMsg(pngPart("x"))}
	base := &fake{action: types.ActDescribe, media: types.ModalityImage, text: "a cat"}

	cases := []struct {
		name string
		conv types.Converter
		want string // decision action, or "" for a rejection
	}{
		{"unknown endpoint", pricedFake{base}, types.DecisionOmitted},
		{"endpoint without pii_ok", egressFake{pricedFake{base}, textOnly()}, types.DecisionOmitted},
		{"endpoint with pii_ok", egressFake{pricedFake{base}, func() types.Offering {
			o := textOnly()
			o.Endpoint.Data.PIIOK = true
			return o
		}()}, types.DecisionDescribed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inner := &stubProvider{name: "texty", offering: textOnly()}
			pol, l := describeImages(tc.conv)
			ch, err := New(inner, pol, l).Stream(ctx, types.Request{Messages: msgs})
			if err != nil {
				t.Fatal(err)
			}
			deltas, err := collect(ch)
			if err != nil {
				t.Fatal(err)
			}
			if got := deltas[0].(types.ConversionDelta).Report.Decisions[0].Action; got != tc.want {
				t.Fatalf("action = %s, want %s", got, tc.want)
			}
		})
	}

	// Without a fallback the refusal rejects the request.
	inner := &stubProvider{name: "texty", offering: textOnly()}
	p := New(inner, types.ConversionPolicy{Converters: []types.Converter{pricedFake{base}}},
		layer(types.DialScopeAgent, per(types.ModalityImage, types.ActDescribe)))
	if _, err := p.Stream(ctx, types.Request{Messages: msgs}); !errors.Is(err, types.ErrModalityUnsupported) {
		t.Fatalf("err = %v, want a rejection", err)
	}
	// Under a boundary that does not require text, the converter runs.
	pass := types.WithEgress(context.Background(), types.Egress{Vault: privacy.NewVault(nil)})
	ch, err := p.Stream(pass, types.Request{Messages: msgs})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := collect(ch); err != nil {
		t.Fatal(err)
	}
}

func TestEgressRequireTextChecksTheWholeView(t *testing.T) {
	ctx := types.WithEgress(context.Background(), types.Egress{RequireText: true})
	out := types.ImageOutPart{Source: types.Bytes(types.MediaPNG, []byte("generated"))}
	msgs := []types.Message{types.UserMsg(types.Text("draw")), types.AssistantMsg(out), types.UserMsg(types.Text("again"))}
	inner := &stubProvider{name: "texty", offering: textOnly()}
	if _, err := New(inner, types.ConversionPolicy{}).Stream(ctx, types.Request{Messages: msgs}); !errors.Is(err, types.ErrModalityUnsupported) {
		t.Fatalf("err = %v, want media in history refused", err)
	}
	// A provider with nothing to plan against is checked too.
	bare := &fakeProviderNoCaps{}
	if _, err := New(bare, types.ConversionPolicy{}).Stream(ctx, types.Request{Messages: []types.Message{types.UserMsg(pngPart("x"))}}); !errors.Is(err, types.ErrModalityUnsupported) {
		t.Fatalf("err = %v, want media refused without an offering", err)
	}
}

type fakeProviderNoCaps struct{}

func (fakeProviderNoCaps) Stream(context.Context, types.Request) (<-chan types.Delta, error) {
	ch := make(chan types.Delta)
	close(ch)
	return ch, nil
}

func TestConverterRunsOutsideTheBoundary(t *testing.T) {
	c := &ctxFake{fake: &fake{action: types.ActDescribe, media: types.ModalityImage, text: "a cat"}}
	inner := &stubProvider{name: "texty", offering: textOnly()}
	pol, l := describeImages(c)
	ctx := types.WithEgress(context.Background(), types.Egress{Vault: privacy.NewVault(nil), RequireText: true})
	ch, err := New(inner, pol, l).Stream(ctx, types.Request{Messages: []types.Message{types.UserMsg(pngPart("x"))}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := collect(ch); err != nil {
		t.Fatal(err)
	}
	if c.sawEgress {
		t.Error("the converter's own model call ran inside the boundary and would refuse its media")
	}
}

func TestURIConversionsAreMemoizedOnlyUnderAScope(t *testing.T) {
	part := types.Image(types.URL("https://example.com/a.png", types.MediaPNG))
	for _, tc := range []struct {
		scope string
		calls int32
	}{{"", 2}, {"tenant-a", 1}} {
		c := &fake{action: types.ActDescribe, media: types.ModalityImage, text: "a cat"}
		inner := &stubProvider{name: "texty", offering: textOnly()}
		pol, l := describeImages(c)
		pol.Scope = tc.scope
		p := New(inner, pol, l)
		for range 2 {
			ch, err := p.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(part)}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := collect(ch); err != nil {
				t.Fatal(err)
			}
		}
		if got := c.calls.Load(); got != tc.calls {
			t.Errorf("scope %q: converter ran %d times, want %d", tc.scope, got, tc.calls)
		}
	}
}

// The privacy decorator over a conversion decorator: the image leaves only
// as a tokenized description, and restored text comes back.
func TestPrivacyOverConversion(t *testing.T) {
	inner := &stubProvider{name: "texty", offering: textOnly(), reply: "it shows <<EMAIL_1>>"}
	pol, l := describeImages(&fake{action: types.ActDescribe, media: types.ModalityImage, text: "a badge: ada@example.com"})
	v := privacy.NewVault(nil)
	p := privacy.NewProvider(New(inner, pol, l), v)
	p.Media = privacy.MediaRequireText
	ch, err := p.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("who?"), pngPart("x"))}})
	if err != nil {
		t.Fatal(err)
	}
	deltas, err := collect(ch)
	if err != nil {
		t.Fatal(err)
	}
	if got := sentText(t, inner); strings.Contains(got, "ada@") || !strings.Contains(got, "<<EMAIL_1>>") {
		t.Fatalf("sent %q", got)
	}
	var out string
	for _, d := range deltas {
		if pd, ok := d.(types.PartDelta); ok {
			out += pd.Text
		}
	}
	if out != "it shows ada@example.com" {
		t.Fatalf("restored %q", out)
	}

	// A vision model behind the same policy is refused, not sent the image.
	vision := &stubProvider{name: "chat", offering: visionChat()}
	pv := privacy.NewProvider(New(vision, types.ConversionPolicy{}), v)
	pv.Media = privacy.MediaRequireText
	if _, err := pv.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(pngPart("x"))}}); !errors.Is(err, types.ErrModalityUnsupported) {
		t.Fatalf("err = %v", err)
	}
	if vision.calls() != 0 {
		t.Fatal("the image was sent")
	}
}
