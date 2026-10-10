package convert

import (
	"context"
	"errors"
	"testing"

	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

func TestProviderConvertsBeforeDispatch(t *testing.T) {
	inner := &stubProvider{name: "chat", offering: visionChat(), opts: true}
	c := &fake{action: types.ActTranscribe, media: types.ModalityAudio, text: "spoken words"}
	p := must.Get(New(inner, Config{Policy: types.ConversionPolicy{Converters: []types.Converter{c}}, Layers: []types.DialLayer{layer(types.DialScopePreset, per(types.ModalityAudio, types.ActTranscribe))}}))
	msgs := []types.Message{types.UserMsg(types.Text("q"), wav("clip"))}
	temp := 0.5
	ch, err := p.Stream(context.Background(), types.Request{Messages: msgs, Options: &types.RequestOptions{Temperature: &temp}})
	if err != nil {
		t.Fatal(err)
	}
	deltas, err := collect(ch)
	if err != nil {
		t.Fatal(err)
	}
	cd, ok := deltas[0].(types.ConversionDelta)
	if !ok || cd.Report.Decisions[0].Action != types.DecisionTranscribed || cd.Report.Decisions[0].Scope != types.DialScopePreset {
		t.Fatalf("first delta = %#v, want the executed conversion report", deltas[0])
	}
	sent := inner.last()[0].(types.UserMessage).Parts
	if len(sent) != 2 || sent[1].(types.TextPart).Text != "spoken words" {
		t.Fatalf("sent %#v, want the transcript", sent)
	}
	if inner.reqs[0].Options == nil || *inner.reqs[0].Options.Temperature != 0.5 {
		t.Error("request options were not forwarded")
	}
	if _, ok := msgs[0].(types.UserMessage).Parts[1].(types.AudioPart); !ok {
		t.Error("the caller's messages were modified")
	}
}

func TestProviderSpendsTheModalityDial(t *testing.T) {
	inner := &stubProvider{name: "chat", offering: visionChat()} // takes no options
	p := must.Get(New(inner, Config{Policy: types.ConversionPolicy{}}))
	d := per(types.ModalityAudio, types.ActOmit)
	ch, err := p.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(wav("a"))},
		Options: &types.RequestOptions{Dials: types.Dials{Modality: &d}}})
	if err != nil {
		t.Fatalf("a request whose only option was the modality dial was refused: %v", err)
	}
	if _, err := collect(ch); err != nil {
		t.Fatal(err)
	}
	if inner.reqs[0].Options != nil {
		t.Errorf("options %+v reached the adapter, want none", inner.reqs[0].Options)
	}
	if txt := inner.last()[0].(types.UserMessage).Parts[0].(types.TextPart).Text; txt == "" {
		t.Error("the omitted part left no notice")
	}
}

func TestProviderRejectsBeforeDispatch(t *testing.T) {
	inner := &stubProvider{name: "chat", offering: textOnly()}
	p := must.Get(New(inner, Config{Policy: types.ConversionPolicy{}}))
	_, err := p.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(pngPart("x"))}})
	if !errors.Is(err, types.ErrModalityUnsupported) {
		t.Fatalf("err = %v, want ErrModalityUnsupported", err)
	}
	if inner.calls() != 0 {
		t.Error("a rejected request reached the provider")
	}
	if _, _, err := p.PlanConversions(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(pngPart("x"))}}); !errors.Is(err, types.ErrModalityUnsupported) {
		t.Errorf("PlanConversions err = %v", err)
	}
}

func TestProviderNativeRequestPassesThrough(t *testing.T) {
	inner := &stubProvider{name: "chat", offering: visionChat()}
	p := must.Get(New(inner, Config{Policy: types.ConversionPolicy{}}))
	msgs := []types.Message{types.UserMsg(pngPart("x"))}
	ch, err := p.Stream(context.Background(), types.Request{Messages: msgs})
	if err != nil {
		t.Fatal(err)
	}
	deltas, _ := collect(ch)
	for _, d := range deltas {
		if _, ok := d.(types.ConversionDelta); ok {
			t.Fatal("a native request emitted a conversion report")
		}
	}
	if _, ok := inner.last()[0].(types.UserMessage).Parts[0].(types.ImagePart); !ok {
		t.Error("the native image did not reach the provider")
	}
}

// A runtime policy (the agent's) applies above the decorator's: its dial at
// agent scope, its converters first.
func TestProviderRuntimePolicy(t *testing.T) {
	inner := &stubProvider{name: "chat", offering: visionChat()}
	c := &fake{action: types.ActTranscribe, media: types.ModalityAudio, text: "agent transcript"}
	p := must.Get(New(inner, Config{Policy: types.ConversionPolicy{Dial: per(types.ModalityAudio, types.ActReject)}}))
	ctx := WithRuntime(context.Background(), Runtime{Policy: types.ConversionPolicy{
		Dial: per(types.ModalityAudio, types.ActTranscribe), Converters: []types.Converter{c}}})
	ch, err := p.Stream(ctx, types.Request{Messages: []types.Message{types.UserMsg(wav("a"))}})
	if err != nil {
		t.Fatal(err)
	}
	deltas, _ := collect(ch)
	if cd := deltas[0].(types.ConversionDelta); cd.Report.Decisions[0].Scope != types.DialScopeAgent {
		t.Errorf("scope = %q, want agent", cd.Report.Decisions[0].Scope)
	}
	// A converter's own model call is not converted again.
	if _, ok := RuntimeFrom(converterContext(ctx)); ok {
		if rt, _ := RuntimeFrom(converterContext(ctx)); len(rt.Policy.Converters) > 0 {
			t.Error("the runtime leaked into a converter's call")
		}
	}
}

func TestProviderWithoutCapabilitiesPassesThrough(t *testing.T) {
	inner := bareProvider{}
	p := must.Get(New(inner, Config{Policy: types.ConversionPolicy{}}))
	ch, err := p.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(wav("a"))}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := collect(ch); err != nil {
		t.Fatal(err)
	}
}

type bareProvider struct{}

func (bareProvider) Stream(context.Context, types.Request) (<-chan types.Delta, error) {
	ch := make(chan types.Delta)
	close(ch)
	return ch, nil
}

// Transcribe asks a model that hears the audio, with deterministic
// options, and is priced at that model's rates.
func TestTranscribeCallsAModelThatTakesTheAudio(t *testing.T) {
	off := vertexLike()
	yes := true
	temp, maxOut := types.StandardParam(types.ParamTemperature), types.StandardParam(types.ParamMaxOutputTokens)
	temp.Allowed, maxOut.Allowed = &yes, &yes
	off.Params = types.ParamSpace{Params: map[types.ParamName]types.ParamSpec{types.ParamTemperature: temp, types.ParamMaxOutputTokens: maxOut}}
	hearer := &stubProvider{name: "google", offering: off, reply: "hello there", opts: true,
		pricing: types.Pricing{InputPerMTok: 1, OutputPerMTok: 2}}
	c := Transcribe(hearer)
	if !c.Accepts(wav("w")) || c.Accepts(pngPart("x")) {
		t.Fatal("transcribe accepts the wrong parts")
	}
	if c.Accepts(types.AudioPart{Source: types.Bytes("audio/ogg", []byte("o"))}) {
		t.Error("transcribe accepted audio its model cannot take")
	}
	est, _ := c.Estimate(wav("w"), textOnly())
	if est.OutputTokens != DefaultTranscribeTokens || est.Cost == 0 {
		t.Errorf("estimate = %+v", est)
	}
	parts, usage, err := c.Convert(context.Background(), wav("w"), types.ConvertEnv{})
	if err != nil {
		t.Fatal(err)
	}
	if txt := parts[0].(types.TextPart).Text; txt != "[transcript of audio audio/wav]\nhello there" {
		t.Errorf("text = %q", txt)
	}
	if usage.Usage.PromptTokens != 1000 || usage.Pricing.InputPerMTok != 1 || usage.Model != "g" {
		t.Errorf("usage = %+v", usage)
	}
	req := hearer.reqs[0]
	if req.Options == nil || req.Options.MaxOutputTokens == nil || *req.Options.MaxOutputTokens != DefaultTranscribeTokens ||
		req.Options.Temperature == nil || *req.Options.Temperature != 0 {
		t.Errorf("options = %+v, want temperature 0 and the output cap", req.Options)
	}
	if v1, v2 := c.Version(), Transcribe(hearer, WithPrompt("other")).Version(); v1 == v2 {
		t.Error("a different prompt has the same version")
	}
}
