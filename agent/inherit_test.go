package agent

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// namedProvider is a minimal provider that can be identified by name, so a test
// can tell which provider a child agent ended up with.
type namedProvider struct{ id string }

func (n *namedProvider) ChatStream(context.Context, []types.Message, []types.ToolDef) (<-chan types.Delta, error) {
	ch := make(chan types.Delta)
	close(ch)
	return ch, nil
}
func (n *namedProvider) Name() string { return n.id }

// childConfig builds the sub-agent and returns the config it was constructed
// with, by reaching through the registered delegate tool.
func childConfig(t *testing.T, parent AgentConfig, sa SubAgentDef) AgentConfig {
	t.Helper()
	parent.SubAgents = []SubAgentDef{sa}
	a := NewAgent(parent)

	tool, ok := a.tools.Get("delegate_to_" + sa.Name)
	if !ok {
		t.Fatalf("sub-agent %q was not registered as a delegate tool", sa.Name)
	}
	st, ok := tool.(*subAgentTool)
	if !ok {
		t.Fatalf("delegate tool is %T, want *subAgentTool", tool)
	}
	return st.factory(nil).cfg
}

// A sub-agent is a full agent, so it must run with the same operational
// guarantees as the parent that delegated to it. Before inheritance, a
// delegated child silently ran with no LLM timeout, no tool timeout, no
// metrics, no logger, no compaction and no file resolvers, whatever the parent
// was configured with.
func TestSubAgentInheritsOperationalConfig(t *testing.T) {
	logger := slog.Default()
	compact := &types.CompactConfig{}
	resolvers := map[string]types.Resolver{"file": nil}
	extractors := map[types.MediaType]types.Extractor{types.MediaPDF: nil}

	parent := AgentConfig{
		Name:             "parent",
		Provider:         &namedProvider{id: "parent-provider"},
		Logger:           logger,
		LLMTimeout:       7 * time.Second,
		ToolTimeout:      3 * time.Second,
		MaxParallelTools: 4,
		CompactCfg:       compact,
		Resolvers:        resolvers,
		Extractors:       extractors,
	}

	child := childConfig(t, parent, SubAgentDef{Name: "worker", Description: "does work"})

	if child.LLMTimeout != 7*time.Second {
		t.Errorf("LLMTimeout = %v, want the parent's 7s", child.LLMTimeout)
	}
	if child.ToolTimeout != 3*time.Second {
		t.Errorf("ToolTimeout = %v, want the parent's 3s", child.ToolTimeout)
	}
	if child.MaxParallelTools != 4 {
		t.Errorf("MaxParallelTools = %d, want the parent's 4", child.MaxParallelTools)
	}
	if child.CompactCfg != compact {
		t.Error("CompactCfg must be inherited")
	}
	if len(child.Resolvers) != 1 || len(child.Extractors) != 1 {
		t.Error("the file resolver/extractor pipeline must be inherited, or attachments break inside delegated work")
	}
	if child.Logger != logger {
		t.Error("Logger must be inherited")
	}
	if child.Metrics == nil {
		t.Error("Metrics must be inherited (the parent's default is set by NewAgent)")
	}
}

func TestSubAgentInheritsProviderWhenUnset(t *testing.T) {
	parent := AgentConfig{Name: "parent", Provider: &namedProvider{id: "parent-provider"}}

	child := childConfig(t, parent, SubAgentDef{Name: "worker"})
	if got := types.ProviderName(child.Provider); got != "parent-provider" {
		t.Errorf("provider = %q, want the parent's when the def sets none", got)
	}
}

func TestSubAgentProviderOverridesParent(t *testing.T) {
	parent := AgentConfig{Name: "parent", Provider: &namedProvider{id: "parent-provider"}}

	child := childConfig(t, parent, SubAgentDef{Name: "worker", Provider: &namedProvider{id: "child-provider"}})
	if got := types.ProviderName(child.Provider); got != "child-provider" {
		t.Errorf("provider = %q, want the sub-agent's own", got)
	}
}

func TestSubAgentInheritsMaxIterWhenUnset(t *testing.T) {
	parent := AgentConfig{Name: "parent", Provider: &namedProvider{id: "p"}, MaxIter: 25}

	if got := childConfig(t, parent, SubAgentDef{Name: "a"}).MaxIter; got != 25 {
		t.Errorf("MaxIter = %d, want the parent's 25", got)
	}
	if got := childConfig(t, parent, SubAgentDef{Name: "b", MaxIter: 3}).MaxIter; got != 3 {
		t.Errorf("MaxIter = %d, want the sub-agent's own 3", got)
	}
}

// Inheriting the parent's ResponseSchema would constrain the child's working
// output to the shape of the parent's final answer, and inheriting the Store
// would write a fresh root into it on every delegation, since sub-agents build
// a new tree per call.
func TestSubAgentDoesNotInheritParentOnlyConfig(t *testing.T) {
	schema := &types.ParameterSchema{Type: "object"}
	parent := AgentConfig{
		Name:           "parent",
		Provider:       &namedProvider{id: "p"},
		ResponseSchema: schema,
	}

	child := childConfig(t, parent, SubAgentDef{Name: "worker"})
	if child.ResponseSchema != nil {
		t.Error("ResponseSchema constrains the parent's final answer and must not be inherited")
	}
	if child.Store != nil {
		t.Error("Store must not be inherited: the child rebuilds its tree on every delegation")
	}
	if len(child.Handoffs) != 0 {
		t.Error("a handoff group belongs to the entry agent that owns the shared tree")
	}
}

func TestSubAgentOptionsOverrideInheritance(t *testing.T) {
	parent := AgentConfig{
		Name:       "parent",
		Provider:   &namedProvider{id: "p"},
		LLMTimeout: 30 * time.Second,
	}

	child := childConfig(t, parent, SubAgentDef{
		Name:    "worker",
		Options: []AgentOption{WithLLMTimeout(2 * time.Second)},
	})
	if child.LLMTimeout != 2*time.Second {
		t.Errorf("LLMTimeout = %v, want the option's 2s: Options are the escape hatch and must win", child.LLMTimeout)
	}
}

// ── Handoffs ────────────────────────────────────────────────────────

func TestHandoffMemberInheritsEntryProvider(t *testing.T) {
	entry := &namedProvider{id: "entry-provider"}
	a := NewAgent(AgentConfig{
		Name:     "entry",
		Provider: entry,
		Handoffs: []HandoffDef{
			{Name: "specialist", Description: "handles the hard part"},
		},
	})

	m := a.activeMember("specialist")
	if m == nil {
		t.Fatal("specialist must be a member of the group")
	}
	if got := types.ProviderName(m.provider); got != "entry-provider" {
		t.Errorf("provider = %q, want the entry agent's when the def sets none", got)
	}
}

func TestHandoffMemberProviderOverridesEntry(t *testing.T) {
	a := NewAgent(AgentConfig{
		Name:     "entry",
		Provider: &namedProvider{id: "entry-provider"},
		Handoffs: []HandoffDef{
			{Name: "specialist", Provider: &namedProvider{id: "specialist-provider"}},
		},
	})

	if got := types.ProviderName(a.activeMember("specialist").provider); got != "specialist-provider" {
		t.Errorf("provider = %q, want the member's own", got)
	}
}

func TestHandoffMemberInheritsEntryMaxIter(t *testing.T) {
	a := NewAgent(AgentConfig{
		Name:     "entry",
		Provider: &namedProvider{id: "p"},
		MaxIter:  17,
		Handoffs: []HandoffDef{{Name: "specialist"}},
	})

	if got := a.activeMember("specialist").maxIter; got != 17 {
		t.Errorf("maxIter = %d, want the entry agent's 17", got)
	}
}

// With no provider anywhere the group genuinely cannot run, and the error must
// name the real problem: the entry agent, which is the inheritance root.
func TestHandoffGroupWithNoProviderAnywhereIsRejected(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("a handoff group with no provider anywhere must be rejected")
		}
	}()
	NewAgent(AgentConfig{
		Name:     "entry",
		Handoffs: []HandoffDef{{Name: "specialist"}},
	})
}
