package convert

import (
	"errors"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func per(m types.Modality, as ...types.ModalityAction) types.ModalityDial {
	return types.ModalityDial{Per: map[types.Modality][]types.ModalityAction{m: as}}
}

func layer(scope string, d types.ModalityDial) types.DialLayer {
	return types.DialLayer{Scope: scope, Dials: types.Dials{Modality: &d}}
}

func TestPlanDecisions(t *testing.T) {
	transcriber := &fake{action: types.ActTranscribe, media: types.ModalityAudio, text: "hello", cost: 7}
	gs := types.Image(types.URL("gs://bucket/a.png", types.MediaPNG))
	tests := []struct {
		name     string
		target   types.Offering
		part     types.UserPart
		policy   types.ConversionPolicy
		layers   []types.DialLayer
		action   string
		reason   string // substring
		via      string
		scope    string
		rejected bool
	}{
		{name: "native image", target: visionChat(), part: pngPart("a"), action: types.DecisionNative},
		{name: "image in a tool result is lowered", target: visionChat(), part: types.ToolOK("c1", pngPart("b").(types.ToolOutputPart)),
			action: types.DecisionLowered},
		{name: "pdf in a tool result is not native on chat", target: visionChat(), part: types.ToolOK("c1", pdf([]byte("%PDF"))),
			action: types.DecisionRejected, reason: "inside a tool result", rejected: true},
		{name: "audio is rejected by default", target: visionChat(), part: wav("w"), action: types.DecisionRejected,
			reason: "audio/wav is not an input", scope: ScopeDefault, rejected: true},
		{name: "a permitted action without a converter rejects", target: visionChat(), part: wav("w"),
			policy: types.ConversionPolicy{Dial: per(types.ModalityAudio, types.ActTranscribe)},
			action: types.DecisionRejected, reason: "no converter for transcribe", scope: ScopePolicy, rejected: true},
		{name: "transcribe with a converter", target: visionChat(), part: wav("w"),
			policy: types.ConversionPolicy{Dial: per(types.ModalityAudio, types.ActTranscribe), Converters: []types.Converter{transcriber}},
			action: types.DecisionTranscribed, via: "fake-transcribe@1", scope: ScopePolicy},
		{name: "omit", target: visionChat(), part: wav("w"), layers: []types.DialLayer{layer(types.DialScopeAgent, per(types.ModalityAudio, types.ActOmit))},
			action: types.DecisionOmitted, scope: types.DialScopeAgent},
		{name: "a later layer wins", target: visionChat(), part: wav("w"),
			policy: types.ConversionPolicy{Dial: per(types.ModalityAudio, types.ActOmit), Converters: []types.Converter{transcriber}},
			layers: []types.DialLayer{layer(types.DialScopeGlobal, per(types.ModalityAudio, types.ActReject)),
				layer(types.DialScopeRequest, per(types.ModalityAudio, types.ActTranscribe))},
			action: types.DecisionTranscribed, scope: types.DialScopeRequest},
		{name: "unavailable media", target: visionChat(), part: types.Image(types.URL("s3://b/a.png", types.MediaPNG).Unavailable("denied")),
			action: types.DecisionRejected, reason: "unavailable: denied", rejected: true},
		{name: "gs on vertex is native", target: vertexLike(), part: gs, action: types.DecisionNative},
		{name: "gs on the gemini API is not", target: geminiLike(), part: gs, action: types.DecisionRejected,
			reason: "no locator the endpoint reads", rejected: true},
		{name: "over the byte limit", target: visionChat(), part: types.Image(types.Source{MediaType: types.MediaPNG, Inline: []byte("x"), Size: 2 << 20}),
			action: types.DecisionRejected, reason: "over the 1048576 byte limit", rejected: true},
		{name: "a file URI the endpoint cannot fetch", target: visionChat(), part: types.Image(types.URL("file:///a.png", types.MediaPNG)),
			action: types.DecisionRejected, reason: "a file: URI", rejected: true},
		{name: "another endpoint's file", target: visionChat(), part: types.Image(types.VendorFileID("anthropic", "anthropic", "f1", types.MediaPNG)),
			action: types.DecisionRejected, reason: "another endpoint's file", rejected: true},
		{name: "own vendor file", target: visionChat(), part: types.Image(types.VendorFileID("openai", "openai-chat", "f1", types.MediaPNG)),
			action: types.DecisionNative},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pl, err := PlanConversions(tt.target, []types.Message{types.UserMsg(types.Text("q"), tt.part)}, tt.policy, tt.layers...)
			if tt.rejected != (err != nil) {
				t.Fatalf("err = %v, want rejected %v", err, tt.rejected)
			}
			if err != nil && (!errors.Is(err, types.ErrModalityUnsupported) || !errors.Is(err, types.ErrInvalidModelConfig)) {
				t.Errorf("err = %v, want it to match ErrModalityUnsupported and ErrInvalidModelConfig", err)
			}
			if len(pl.Decisions) != 1 {
				t.Fatalf("decisions = %+v, want one", pl.Decisions)
			}
			d := pl.Decisions[0]
			if d.Action != tt.action {
				t.Errorf("action = %s, want %s (reason %q)", d.Action, tt.action, d.Reason)
			}
			if !strings.Contains(d.Reason, tt.reason) {
				t.Errorf("reason = %q, want it to contain %q", d.Reason, tt.reason)
			}
			if tt.via != "" && d.Via != tt.via {
				t.Errorf("via = %q, want %q", d.Via, tt.via)
			}
			if tt.scope != "" && d.Scope != tt.scope {
				t.Errorf("scope = %q, want %q", d.Scope, tt.scope)
			}
			if d.Path.Message != 0 || d.Path.Part != 1 {
				t.Errorf("path = %s, want 0.1[.n]", d.Path)
			}
		})
	}
}

// The planner does no I/O and runs no converter: it only asks for estimates.
func TestPlanIsPure(t *testing.T) {
	c := &fake{action: types.ActTranscribe, media: types.ModalityAudio, cost: 40}
	msgs := []types.Message{types.UserMsg(wav("a")), types.UserMsg(wav("b"))}
	before := types.CloneParts(msgs[0].(types.UserMessage).Parts)
	pol := types.ConversionPolicy{Dial: per(types.ModalityAudio, types.ActTranscribe), Converters: []types.Converter{c}}
	pl, err := PlanConversions(visionChat(), msgs, pol)
	if err != nil {
		t.Fatal(err)
	}
	if c.calls.Load() != 0 {
		t.Fatalf("planning ran the converter %d times", c.calls.Load())
	}
	if pl.Estimate.Cost != 80 || pl.Estimate.InputTokens != 200 || pl.Estimate.OutputTokens != 100 {
		t.Errorf("estimate = %+v, want the sum of both", pl.Estimate)
	}
	if got := msgs[0].(types.UserMessage).Parts; len(got) != len(before) || got[0].(types.AudioPart).Source.Digest != before[0].(types.AudioPart).Source.Digest {
		t.Error("planning changed the messages")
	}
	again, _ := PlanConversions(visionChat(), msgs, pol)
	if again.Report().Hash != pl.Report().Hash {
		t.Error("the same request planned twice hashed differently")
	}
}

func TestPlanCostCapRejects(t *testing.T) {
	c := &fake{action: types.ActTranscribe, media: types.ModalityAudio, cost: 40}
	pol := types.ConversionPolicy{Dial: per(types.ModalityAudio, types.ActTranscribe), Converters: []types.Converter{c}, MaxCost: 50}
	_, err := PlanConversions(visionChat(), []types.Message{types.UserMsg(wav("a"), wav("b"))}, pol)
	if !errors.Is(err, types.ErrModalityUnsupported) || !strings.Contains(err.Error(), "exceeds the policy cap") {
		t.Fatalf("err = %v, want the cost cap", err)
	}
}

func TestPlanMaxCount(t *testing.T) {
	o := visionChat()
	l := o.Modalities.In[types.ModalityImage]
	l.MaxCount = 1
	o.Modalities.In[types.ModalityImage] = l
	pl, err := PlanConversions(o, []types.Message{types.UserMsg(pngPart("a"), pngPart("b"))}, types.ConversionPolicy{})
	if err == nil || pl.Decisions[0].Action != types.DecisionNative || !strings.Contains(pl.Decisions[1].Reason, "more than 1") {
		t.Fatalf("decisions = %+v, err %v; want the second image over the count", pl.Decisions, err)
	}
}

func TestUnavailableMatchesErrMediaUnavailable(t *testing.T) {
	_, err := PlanConversions(visionChat(), []types.Message{types.UserMsg(types.Image(types.Source{MediaType: types.MediaPNG}))}, types.ConversionPolicy{})
	if !errors.Is(err, types.ErrMediaUnavailable) {
		t.Fatalf("err = %v, want ErrMediaUnavailable for media whose bytes were not persisted", err)
	}
}

// Reasoning another provider signed is never replayed to Anthropic.
func TestPlanThinkingForAnthropic(t *testing.T) {
	own := types.ThinkingPart{Text: "mine", Signature: "sig-a", Origin: "anthropic"}
	legacy := types.ThinkingPart{Text: "old", Signature: "sig-?"}
	foreign := types.ThinkingPart{Text: "theirs", Signature: "sig-g", Origin: "google"}
	unsigned := types.ThinkingPart{Text: "bare", Origin: "anthropic"}
	redacted := types.ThinkingPart{Redacted: true, Signature: "enc", Origin: "openai"}
	msgs := []types.Message{types.UserMsg(types.Text("q")), types.AssistantMsg(own, legacy, foreign, unsigned, redacted, types.Text("a"))}

	pl, err := PlanConversions(textOnly(), msgs, types.ConversionPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	var paths []int
	for _, d := range pl.Decisions {
		if d.Action != types.DecisionOmitted {
			t.Errorf("part %s: %s, want omitted", d.Path, d.Action)
		}
		paths = append(paths, d.Path.Part)
	}
	if len(paths) != 3 || paths[0] != 2 || paths[1] != 3 || paths[2] != 4 {
		t.Fatalf("dropped parts %v, want the foreign, unsigned and foreign redacted ones (2, 3, 4)", paths)
	}
	view, rep, err := pl.Apply(t.Context(), msgs, Runtime{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := view[1].(types.AssistantMessage).Parts
	if len(got) != 3 || got[0].(types.ThinkingPart).Text != "mine" || got[1].(types.ThinkingPart).Text != "old" {
		t.Fatalf("view = %#v, want own and legacy reasoning kept, then the text", got)
	}
	if len(msgs[1].(types.AssistantMessage).Parts) != 6 {
		t.Error("the record was changed")
	}
	if len(rep.Decisions) != 3 || rep.Hash == "" {
		t.Errorf("report = %+v", rep)
	}

	// Per policy, readable reasoning is sent as text instead.
	pl, _ = PlanConversions(textOnly(), msgs, types.ConversionPolicy{Thinking: types.ThinkingAsText})
	view, _, err = pl.Apply(t.Context(), msgs, Runtime{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got = view[1].(types.AssistantMessage).Parts
	if len(got) != 5 || got[2].(types.TextPart).Text != "[reasoning] theirs" || got[3].(types.TextPart).Text != "[reasoning] bare" {
		t.Fatalf("view = %#v, want foreign and unsigned reasoning as text, the redacted part dropped", got)
	}

	// Other targets do not verify signatures: nothing is planned.
	if pl, _ := PlanConversions(visionChat(), msgs, types.ConversionPolicy{}); len(pl.Decisions) != 0 {
		t.Errorf("decisions for a non-verifying target = %+v", pl.Decisions)
	}
}

func TestModalityDialValidate(t *testing.T) {
	for _, d := range []types.ModalityDial{
		{Default: "explode"},
		per(types.ModalityAudio, types.ActOmit, types.ActTranscribe),
		per("smell", types.ActOmit),
	} {
		if err := (types.Dials{Modality: &d}).Validate(); !errors.Is(err, types.ErrInvalidModelConfig) {
			t.Errorf("dial %+v validated: %v", d, err)
		}
	}
	ok := per(types.ModalityAudio, types.ActTranscribe, types.ActOmit)
	if err := (types.Dials{Modality: &ok}).Validate(); err != nil {
		t.Errorf("valid dial rejected: %v", err)
	}
}
