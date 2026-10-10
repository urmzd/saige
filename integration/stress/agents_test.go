//go:build stress

package stress

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
)

// echoTool returns a result derived from its arguments after a short random
// pause, so parallel calls finish out of order.
func echoTool() *types.ToolFunc {
	return &types.ToolFunc{
		Def: types.ToolDef{
			Name: "echo",
			Parameters: types.ParameterSchema{
				Type:     "object",
				Required: []string{"id"},
				Properties: map[string]types.PropertyDef{
					"id": {Type: "string"},
				},
			},
		},
		Fn: func(ctx context.Context, args map[string]any) (string, error) {
			select {
			case <-time.After(time.Duration(rand.IntN(2000)) * time.Microsecond): //nolint:gosec // jitter only
			case <-ctx.Done():
				return "", ctx.Err()
			}
			return fmt.Sprintf("result-of-%v", args["id"]), nil
		},
	}
}

// parallelCalls is one model turn that calls echo n times at once.
func parallelCalls(agentIdx, turn, n int) []types.Delta {
	var ds []types.Delta
	for j := range n {
		id := fmt.Sprintf("a%d-t%d-c%d", agentIdx, turn, j)
		ds = append(ds, agenttest.ToolCallResponse(id, "echo", map[string]any{"id": id})...)
	}
	return ds
}

// toolResults returns the tool results in messages, automatic (system) or
// human-provided (user), keyed by call ID. A call ID seen twice is reported
// under dup.
func toolResults(messages []types.Message) (results map[string]string, dup []string) {
	results = map[string]string{}
	add := func(c any) {
		if tr, ok := c.(types.ToolResultPart); ok {
			if _, seen := results[tr.CallID]; seen {
				dup = append(dup, tr.CallID)
			}
			results[tr.CallID] = tr.Text()
		}
	}
	for _, m := range messages {
		switch v := m.(type) {
		case types.SystemMessage:
			for _, c := range v.Parts {
				add(c)
			}
		case types.UserMessage:
			for _, c := range v.Parts {
				add(c)
			}
		}
	}
	return results, dup
}

// TestConcurrentAgents runs many agents at once, each holding a two-turn
// conversation in which every model turn makes parallel tool calls. Each
// request the model receives must carry exactly the results of the calls it
// made, paired by ID. Run it with -race.
func TestConcurrentAgents(t *testing.T) {
	n := envInt(t, "SAIGE_STRESS_AGENTS", 200)
	const (
		turnOneCalls = 4
		turnTwoCalls = 3
	)
	ctx := testContext(t, 2*time.Minute)
	before := runtime.NumGoroutine()

	tools := types.NewToolRegistry(echoTool())
	type outcome struct {
		err     error
		latency time.Duration
	}
	outcomes := make([]outcome, n)
	providers := make([]*agenttest.ScriptedProvider, n)
	start := time.Now()
	var wg sync.WaitGroup
	for i := range n {
		providers[i] = &agenttest.ScriptedProvider{Responses: [][]types.Delta{
			parallelCalls(i, 1, turnOneCalls),
			agenttest.TextResponse(fmt.Sprintf("agent %d turn 1 done", i)),
			parallelCalls(i, 2, turnTwoCalls),
			agenttest.TextResponse(fmt.Sprintf("agent %d turn 2 done", i)),
		}}
		wg.Add(1)
		go func() {
			defer wg.Done()
			began := time.Now()
			a := agent.NewAgent(agent.AgentConfig{Provider: providers[i], Tools: tools, SystemPrompt: "stress"})
			for turn := 1; turn <= 2; turn++ {
				text, err := agent.CollectText(a.Invoke(ctx, []types.Message{types.UserMsg(types.Text(fmt.Sprintf("agent %d turn %d", i, turn)))}))
				if err == nil && text != fmt.Sprintf("agent %d turn %d done", i, turn) {
					err = fmt.Errorf("turn %d text = %q", turn, text)
				}
				if err != nil {
					outcomes[i].err = err
					return
				}
			}
			outcomes[i].latency = time.Since(began)
		}()
	}
	wg.Wait()
	wall := time.Since(start)

	latencies := make([]time.Duration, 0, n)
	for i, o := range outcomes {
		if o.err != nil {
			t.Fatalf("agent %d: %v", i, o.err)
		}
		latencies = append(latencies, o.latency)
		reqs := providers[i].Requests()
		if len(reqs) != 4 {
			t.Fatalf("agent %d made %d model calls, want 4", i, len(reqs))
		}
		// Request 1 answers turn one's calls; request 3 must carry turn one's
		// and turn two's calls, and nothing else.
		for _, check := range []struct {
			req   int
			turns []int
		}{{1, []int{1}}, {3, []int{1, 2}}} {
			got, dup := toolResults(reqs[check.req].Messages)
			if len(dup) > 0 {
				t.Fatalf("agent %d request %d: duplicate results for %v", i, check.req, dup)
			}
			want := map[string]string{}
			for _, turn := range check.turns {
				calls := turnOneCalls
				if turn == 2 {
					calls = turnTwoCalls
				}
				for j := range calls {
					id := fmt.Sprintf("a%d-t%d-c%d", i, turn, j)
					want[id] = "result-of-" + id
				}
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("agent %d request %d: tool results\n got %v\nwant %v", i, check.req, got, want)
			}
		}
	}
	logLatency(t, fmt.Sprintf("conversations (2 turns, %d tool calls each)", turnOneCalls+turnTwoCalls), latencies, wall)
	t.Logf("model turns/s=%.0f tool calls/s=%.0f",
		float64(4*n)/wall.Seconds(), float64((turnOneCalls+turnTwoCalls)*n)/wall.Seconds())
	checkGoroutines(t, before)
}

// gateProvider answers every call with text after a short pause and records
// the largest number of calls in flight at once.
type gateProvider struct {
	inFlight, peak, calls atomic.Int32
}

func (p *gateProvider) Stream(ctx context.Context, _ types.Request) (<-chan types.Delta, error) {
	n := p.inFlight.Add(1)
	p.calls.Add(1)
	for {
		peak := p.peak.Load()
		if n <= peak || p.peak.CompareAndSwap(peak, n) {
			break
		}
	}
	ch := make(chan types.Delta, 4)
	go func() {
		defer close(ch)
		select {
		case <-time.After(time.Millisecond):
		case <-ctx.Done():
		}
		// The call ends before its last delta, so the count drops before
		// the run can start its next call.
		p.inFlight.Add(-1)
		for _, d := range agenttest.TextResponse("ok") {
			ch <- d
		}
	}()
	return ch, nil
}

// TestSameBranchContention sends many Invoke and Submit calls to one branch
// at once. One run holds the branch at a time: a refused Invoke gets
// ErrRunActive and leaves no trace, and every accepted message is appended
// exactly once.
func TestSameBranchContention(t *testing.T) {
	const callers = 256
	ctx := testContext(t, time.Minute)
	before := runtime.NumGoroutine()
	provider := &gateProvider{}
	a := agent.NewAgent(agent.AgentConfig{Provider: provider, SystemPrompt: "stress"})
	branch := a.Tree().Active()

	var (
		mu       sync.Mutex
		accepted = map[string]bool{}
		refused  = map[string]bool{}
		streams  = map[*agent.EventStream]bool{}
		errs     []error
	)
	var wg sync.WaitGroup
	release := make(chan struct{})
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-release
			// Spread arrivals over a few runs' lifetimes, so callers meet
			// runs that are starting, running and finishing.
			time.Sleep(time.Duration(rand.IntN(50)) * time.Millisecond) //nolint:gosec // jitter only
			if i%2 == 0 {
				text := fmt.Sprintf("invoke %d", i)
				_, err := agent.CollectText(a.Invoke(ctx, []types.Message{types.UserMsg(types.Text(text))}, branch))
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					accepted[text] = true
				case errors.Is(err, agent.ErrRunActive):
					refused[text] = true
				default:
					errs = append(errs, fmt.Errorf("%s: %w", text, err))
				}
				return
			}
			text := fmt.Sprintf("submit %d", i)
			s, _, err := a.Submit(ctx, branch, types.UserMsg(types.Text(text)), agent.SubmitQueue)
			if err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("%s: %w", text, err))
				mu.Unlock()
				return
			}
			// Several callers may share one stream; each drains it until
			// it closes.
			for range s.Deltas() {
			}
			err = s.Wait()
			mu.Lock()
			defer mu.Unlock()
			streams[s] = true
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", text, err))
				return
			}
			accepted[text] = true
		}()
	}
	start := time.Now()
	close(release)
	wg.Wait()
	wall := time.Since(start)
	for _, err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		t.FailNow()
	}

	if peak := provider.peak.Load(); peak != 1 {
		t.Fatalf("peak concurrent model calls on one branch = %d, want 1", peak)
	}
	for s := range streams {
		if u := s.Undelivered(); len(u) > 0 {
			t.Fatalf("stream left %d submissions undelivered", len(u))
		}
	}
	msgs, err := a.Tree().FlattenBranch(branch)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, m := range msgs {
		if um, ok := m.(types.UserMessage); ok {
			for _, c := range um.Parts {
				if tc, ok := c.(types.TextPart); ok {
					seen[tc.Text]++
				}
			}
		}
	}
	for text := range accepted {
		if seen[text] != 1 {
			t.Errorf("accepted %q appears %d times", text, seen[text])
		}
	}
	for text := range refused {
		if seen[text] != 0 {
			t.Errorf("refused %q appears %d times", text, seen[text])
		}
	}
	t.Logf("callers=%d accepted=%d refused=%d runs=%d model calls=%d wall=%v",
		callers, len(accepted), len(refused), len(streams), provider.calls.Load(), wall.Round(time.Millisecond))
	checkGoroutines(t, before)
}
