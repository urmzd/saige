package eval

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/urmzd/saige/agent/types"
	topeval "github.com/urmzd/saige/eval"
)

// offeringProvider reports an offering, so the conversion decorator plans
// against it, and records what it is sent.
type offeringProvider struct {
	offering types.Offering
	mu       sync.Mutex
	got      []types.Request
}

func (p *offeringProvider) Capabilities() types.ModelCapabilities {
	o := p.offering
	return types.ModelCapabilities{Offering: &o}
}

func (p *offeringProvider) Stream(_ context.Context, req types.Request) (<-chan types.Delta, error) {
	p.mu.Lock()
	p.got = append(p.got, req)
	p.mu.Unlock()
	ch := make(chan types.Delta, 4)
	for _, d := range types.PartDeltas(0, types.TextPart{Text: `{"reasoning":"ok","score":1}`}) {
		ch <- d
	}
	close(ch)
	return ch, nil
}

func textOffering() types.Offering {
	return types.Offering{ID: "test/text@test", Model: types.ModelInfo{Vendor: "test", Prefix: "text", Known: true},
		Endpoint: types.EndpointInfo{Name: "test", Surface: types.SurfaceOpenAIChat}}
}

func visionOffering() types.Offering {
	o := textOffering()
	o.ID = "test/vision@test"
	o.Modalities.In = map[types.Modality]types.ModalityLimit{types.ModalityImage: {
		Media: []types.MediaType{types.MediaPNG}, Sources: []types.SourceKind{types.SourceInline}}}
	return o
}

func imageParts() []types.UserPart {
	return []types.UserPart{types.Text("judge this"), types.Image(types.Bytes(types.MediaPNG, []byte("png")))}
}

func TestGeneratorSendsNativeMedia(t *testing.T) {
	p := &offeringProvider{offering: visionOffering()}
	g := NewGenerator(p)
	if _, err := g.GenerateParts(context.Background(), imageParts(), nil); err != nil {
		t.Fatal(err)
	}
	msg := p.got[0].Messages[0].(types.UserMessage)
	if len(msg.Parts) != 2 || msg.Parts[1].Kind() != types.KindImage {
		t.Fatalf("sent %#v", msg.Parts)
	}
}

func TestGeneratorRejectsMediaTheModelCannotTake(t *testing.T) {
	p := &offeringProvider{offering: textOffering()}
	_, err := NewGenerator(p).GenerateParts(context.Background(), imageParts(), nil)
	if !errors.Is(err, types.ErrModalityUnsupported) {
		t.Fatalf("err = %v, want ErrModalityUnsupported", err)
	}
	if len(p.got) != 0 {
		t.Fatal("a rejected request reached the provider")
	}
}

func TestGeneratorConversionPolicyApplies(t *testing.T) {
	p := &offeringProvider{offering: textOffering()}
	g := NewGenerator(p, WithConversion(types.ConversionPolicy{Dial: types.ModalityDial{Default: types.ActOmit}}))
	if _, err := g.GenerateParts(context.Background(), imageParts(), nil); err != nil {
		t.Fatal(err)
	}
	msg := p.got[0].Messages[0].(types.UserMessage)
	for _, part := range msg.Parts {
		if types.IsMedia(part) {
			t.Fatalf("the image reached a text model: %#v", msg.Parts)
		}
	}
}

func TestJudgeWithAgentGeneratorScoresMediaCase(t *testing.T) {
	p := &offeringProvider{offering: visionOffering()}
	scorer := topeval.NewJudgeScorer(NewGenerator(p))
	raw, err := topeval.EncodeInput(imageParts(), topeval.InputOptions{InlineMax: 64})
	if err != nil {
		t.Fatal(err)
	}
	sc, err := scorer.Score(context.Background(), topeval.Observation{ID: "i", Input: raw, Output: json.RawMessage(`"red"`)})
	if err != nil || sc.Value != 1 {
		t.Fatalf("score = %+v, %v", sc, err)
	}
	if msg := p.got[0].Messages[0].(types.UserMessage); len(msg.Parts) != 2 || msg.Parts[1].Kind() != types.KindImage {
		t.Fatalf("judge sent %#v", msg.Parts)
	}
}
