package agent

import (
	"context"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

// imageRuleProvider serves a scripted reply and reports an offering whose
// images cost a fixed number of tokens.
type imageRuleProvider struct {
	agenttest.ScriptedProvider
	perImage int
}

func (p *imageRuleProvider) Offering() types.Offering {
	return types.Offering{ID: "test/vision@test", Model: types.ModelInfo{Vendor: "test", Prefix: "vision", Known: true},
		Modalities: types.Modalities{In: map[types.Modality]types.ModalityLimit{
			types.ModalityImage: {Media: []types.MediaType{types.MediaPNG}, Tokens: types.TokenRule{PerImage: p.perImage}},
		}}}
}

// TestPressureEstimatePricesMediaByTheOffering checks that the local
// estimate behind input pressure prices an image by the token rule of the
// offering the turn goes to.
func TestPressureEstimatePricesMediaByTheOffering(t *testing.T) {
	a := must.Get(New(Config{Name: "a", Provider: &agenttest.ScriptedProvider{}}))
	msgs := []types.Message{types.UserMsg(types.Image(types.URL("https://x/a.png")))}
	st := &overflowState{}
	flat := a.inputSize(context.Background(), st, msgs)
	st.target(&imageRuleProvider{perImage: 5000})
	if got := a.inputSize(context.Background(), st, msgs); got != flat-1000+5000 {
		t.Fatalf("input size = %d, want the image at 5000 tokens (flat estimate %d)", got, flat)
	}
	st.target(&agenttest.ScriptedProvider{})
	if got := a.inputSize(context.Background(), st, msgs); got != flat {
		t.Fatalf("input size = %d, want the flat estimate %d without an offering", got, flat)
	}
}

// TestContinuedTurnPromptIsNotTheHistorySize checks that a turn served by
// several requests does not set the reported input size: its prompts are
// summed, which is not the size of the history.
func TestContinuedTurnPromptIsNotTheHistorySize(t *testing.T) {
	st := &overflowState{}
	st.turnSucceeded(&types.UsageDelta{PromptTokens: 900, Requests: 2})
	if st.lastPromptTokens != 0 {
		t.Fatalf("last prompt = %d, want it left to the estimate", st.lastPromptTokens)
	}
	st.turnSucceeded(&types.UsageDelta{PromptTokens: 400})
	if st.lastPromptTokens != 400 {
		t.Fatalf("last prompt = %d, want 400", st.lastPromptTokens)
	}
}
