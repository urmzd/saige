package types

import (
	"errors"
	"reflect"
	"testing"
)

func TestModalityDial(t *testing.T) {
	var zero ModalityDial
	if got := zero.Actions(ModalityAudio); !reflect.DeepEqual(got, []ModalityAction{ActReject}) {
		t.Errorf("default actions = %v, want reject", got)
	}
	base := ModalityDial{Per: map[Modality][]ModalityAction{ModalityAudio: {ActTranscribe}, ModalityImage: {ActDescribe}}}
	over := ModalityDial{Default: ActOmit, Per: map[Modality][]ModalityAction{ModalityAudio: {ActConvert, ActTranscribe}}}
	m := base.Merge(over)
	if got := m.Actions(ModalityAudio); !reflect.DeepEqual(got, []ModalityAction{ActConvert, ActTranscribe}) {
		t.Errorf("merged audio = %v", got)
	}
	if got := m.Actions(ModalityImage); !reflect.DeepEqual(got, []ModalityAction{ActDescribe}) {
		t.Errorf("merged image = %v", got)
	}
	if got := m.Actions(ModalityVideo); !reflect.DeepEqual(got, []ModalityAction{ActOmit}) {
		t.Errorf("merged default = %v", got)
	}
	m.Per[ModalityAudio][0] = ActReject
	if over.Per[ModalityAudio][0] != ActConvert {
		t.Error("Merge shares its input's slices")
	}
}

func TestConversionReportHash(t *testing.T) {
	cost := Cost(5)
	r := ConversionReport{Offering: "o", Decisions: []ConversionDecision{
		{Path: PartPath{Message: 1, Part: 0, Nested: -1}, Kind: KindAudio, Digest: "d", Action: DecisionTranscribed, Via: "t@1",
			Cost: &cost, Produced: []string{"saige-artifact://x"}},
	}}
	cached := r.Clone()
	cached.Decisions[0].Cached, cached.Decisions[0].Cost = true, nil
	if r.ComputeHash() != cached.ComputeHash() {
		t.Error("a cache hit changed the report hash")
	}
	other := r.Clone()
	other.Decisions[0].Via = "t@2"
	if r.ComputeHash() == other.ComputeHash() {
		t.Error("a converter version change kept the report hash")
	}
	cp := r.Clone()
	*cp.Decisions[0].Cost = 9
	cp.Decisions[0].Produced[0] = "changed"
	if *r.Decisions[0].Cost != 5 || r.Decisions[0].Produced[0] != "saige-artifact://x" {
		t.Error("Clone shares memory")
	}
	if got := (PartPath{Message: 2, Part: 1, Nested: 3}).String(); got != "2.1.3" {
		t.Errorf("PartPath = %s", got)
	}
	if got := (PartPath{Message: 2, Part: 1, Nested: -1}).String(); got != "2.1" {
		t.Errorf("PartPath = %s", got)
	}
}

func TestMediaErrorsAreInvalidConfig(t *testing.T) {
	for _, err := range []error{ErrMediaUnavailable, ErrModalityUnsupported} {
		if !errors.Is(err, ErrInvalidModelConfig) {
			t.Errorf("%v does not match ErrInvalidModelConfig", err)
		}
		codes := ErrorCodes(err)
		if len(codes) == 0 {
			t.Errorf("%v has no wire code", err)
		}
	}
}
