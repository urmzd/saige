package eval

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/provider/router"
	"github.com/urmzd/saige/agent/types"
	topeval "github.com/urmzd/saige/eval"
)

// catalogModel is a scripted provider that declares a catalog model.
type catalogModel struct {
	*agenttest.ScriptedProvider
	caps types.ModelCapabilities
}

func (p catalogModel) Model() string                         { return p.caps.Model }
func (p catalogModel) Capabilities() types.ModelCapabilities { return p.caps }
func (p catalogModel) EffectiveOptions() types.RequestOptions {
	return types.RequestOptions{}
}

// dialSubject runs an agent that holds creativity "focused" on one model
// behind a router, and records each observation's provenance.
func dialSubject(t *testing.T, vendor, model string, prov *topeval.Provenance, mu *sync.Mutex) topeval.Subject {
	return func(ctx context.Context, obs *topeval.Observation) error {
		p := catalogModel{ScriptedProvider: &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("ok")}},
			caps: catalog.MustLookup(vendor, model)}
		r, err := router.New(router.Config{Profiles: []router.Profile{{ID: vendor + "/" + model, Provider: p}}})
		if err != nil {
			t.Fatal(err)
		}
		focused := types.CreativityFocused
		a := agent.NewAgent(agent.AgentConfig{Provider: r.Session(), SystemPrompt: "s"}, agent.WithDials(types.Dials{Creativity: &focused}))
		stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("hi"))})
		run := CollectAgentRun(stream.Deltas())
		mu.Lock()
		run.AddProvenance(prov)
		mu.Unlock()
		if err := stream.Wait(); err != nil {
			return err
		}
		return AnnotateObservation(obs, run)
	}
}

// Scenario (g): an eval holds creativity constant across gpt-4.1 and
// claude-haiku-5-5. Under StrictDials the Haiku attempt fails, because its
// model would drop the dial. Under the default policy both run, provenance
// records the drop, and the comparison warns that the held dial was sent
// differently.
func TestEvalHoldsDialsAcrossModels(t *testing.T) {
	inputs := []topeval.Observation{{ID: "c1", Input: []byte(`"hi"`)}}
	var mu sync.Mutex

	var gptProv, haikuProv topeval.Provenance
	obs := []topeval.Observation{inputs[0]}
	if err := topeval.PopulateAll(context.Background(), obs, dialSubject(t, "openai", "gpt-4.1", &gptProv, &mu),
		topeval.WithDialPolicy(types.StrictDials)); err != nil {
		t.Fatalf("gpt-4.1 under strict: %v", err)
	}
	obs = []topeval.Observation{inputs[0]}
	err := topeval.PopulateAll(context.Background(), obs, dialSubject(t, "anthropic", "claude-haiku-5-5", &haikuProv, &mu),
		topeval.WithDialPolicy(types.StrictDials))
	if err == nil || !strings.Contains(err.Error(), "dial creativity") {
		t.Fatalf("haiku under strict: %v", err)
	}

	gptProv, haikuProv = topeval.Provenance{}, topeval.Provenance{}
	c, err := topeval.Compare(context.Background(), inputs,
		dialSubject(t, "openai", "gpt-4.1", &gptProv, &mu), dialSubject(t, "anthropic", "claude-haiku-5-5", &haikuProv, &mu), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.ExpSubjectErrors != 0 || c.BaseSubjectErrors != 0 {
		t.Fatalf("subject errors under the default policy: %+v", c)
	}
	if got := haikuProv.Dials["creativity"]; len(got) != 1 || got[0].Requested != "focused" || got[0].Sent[0] != "dropped" {
		t.Fatalf("haiku provenance: %+v", haikuProv.Dials)
	}
	if got := gptProv.Dials["creativity"]; len(got) != 1 || got[0].Sent[0] != "temperature=0.3 top_p=0.9" {
		t.Fatalf("gpt provenance: %+v", gptProv.Dials)
	}
	drift := topeval.ConfigDrift(gptProv, haikuProv)
	if len(drift) != 1 || !strings.Contains(drift[0], "dial creativity focused") {
		t.Fatalf("drift: %v", drift)
	}
}

// A single-provider subject has no router, yet its provenance records what
// each dial was sent as.
func TestSingleProviderSubjectRecordsDials(t *testing.T) {
	p := catalogModel{ScriptedProvider: &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("ok")}},
		caps: catalog.MustLookup("anthropic", "claude-haiku-5-5")}
	focused := types.CreativityFocused
	a := agent.NewAgent(agent.AgentConfig{Provider: p, SystemPrompt: "s"}, agent.WithDials(types.Dials{Creativity: &focused}))
	stream := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("hi"))})
	run := CollectAgentRun(stream.Deltas())
	if err := stream.Wait(); err != nil {
		t.Fatal(err)
	}
	if len(run.Routes) != 1 || run.Routes[0].Dials == nil || run.Routes[0].Model != "claude-haiku-5-5" {
		t.Fatalf("routes: %+v", run.Routes)
	}
	var prov topeval.Provenance
	run.AddProvenance(&prov)
	if got := prov.Dials["creativity"]; len(got) != 1 || got[0].Sent[0] != "dropped" {
		t.Fatalf("provenance: %+v", prov.Dials)
	}
}
