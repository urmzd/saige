package provider

import (
	"context"
	"errors"
	"testing"

	"github.com/urmzd/saige/agent/provider/anthropic"
	"github.com/urmzd/saige/agent/provider/wrapper"
	"github.com/urmzd/saige/agent/types"
)

// Build puts every adapter behind the conversion decorator, with the
// configured policy and the modality dial of its dial layers; the adapter
// itself never sees the modality dial.
func TestBuildPlansConversions(t *testing.T) {
	audio := types.AudioPart{Source: types.Bytes(types.MediaWAV, []byte("wav"))}
	req := types.Request{Messages: []types.Message{types.UserMsg(types.Text("hi"), audio)}}

	p, err := Build(context.Background(), Config{Provider: Anthropic, Model: "claude-haiku-5-5", APIKey: "test"})
	if err != nil {
		t.Fatal(err)
	}
	planner, ok := wrapper.As[types.ConversionPlanner](p)
	if !ok {
		t.Fatalf("%T plans no conversions", p)
	}
	if _, _, err := planner.PlanConversions(context.Background(), req); !errors.Is(err, types.ErrModalityUnsupported) {
		t.Fatalf("audio to a model that takes none: err = %v, want a rejection by default", err)
	}

	omit := types.ModalityDial{Per: map[types.Modality][]types.ModalityAction{types.ModalityAudio: {types.ActOmit}}}
	p, err = Build(context.Background(), Config{Provider: Anthropic, Model: "claude-haiku-5-5", APIKey: "test",
		Dials: types.Dials{Modality: &omit}})
	if err != nil {
		t.Fatal(err)
	}
	planner, _ = wrapper.As[types.ConversionPlanner](p)
	rep, _, err := planner.PlanConversions(context.Background(), req)
	if err != nil || rep.Decisions[0].Action != types.DecisionOmitted || rep.Decisions[0].Scope != types.DialScopeGlobal {
		t.Fatalf("report %+v, err %v; want audio omitted at global scope", rep, err)
	}
	if rep.Offering != "anthropic/claude-haiku-5-5@anthropic" {
		t.Errorf("offering = %q", rep.Offering)
	}
	a, _ := wrapper.As[*anthropic.Adapter](p)
	if o := a.EffectiveOptions(); o.HasDials() || len(o.ModalityLayers()) > 0 {
		t.Errorf("the adapter was given the modality dial: %+v", o.DialLayers)
	}

	p, _ = Build(context.Background(), Config{Provider: Anthropic, Model: "claude-haiku-5-5", APIKey: "test",
		Conversion: types.ConversionPolicy{Dial: omit}})
	planner, _ = wrapper.As[types.ConversionPlanner](p)
	if rep, _, err := planner.PlanConversions(context.Background(), req); err != nil || rep.Decisions[0].Scope != "policy" {
		t.Fatalf("Config.Conversion: report %+v, err %v", rep, err)
	}
}
