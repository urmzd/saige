package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/batch"
	"github.com/urmzd/saige/agent/types"
)

// scriptedModel answers a judge prompt with a verdict and anything else
// with the reversed question.
type scriptedModel struct{}

func (scriptedModel) Name() string  { return "scripted" }
func (scriptedModel) Model() string { return "scripted-1" }

func (m scriptedModel) ChatStream(_ context.Context, msgs []types.Message, _ []types.ToolDef) (<-chan types.Delta, error) {
	var prompt string
	if u, ok := msgs[len(msgs)-1].(types.UserMessage); ok {
		prompt = u.Content[0].(types.TextContent).Text
	}
	answer := "answer:" + prompt
	if strings.Contains(prompt, "impartial evaluator") {
		answer = `{"reasoning":"fine","score":0.75}`
	}
	out := make(chan types.Delta, 4)
	out <- types.TextStartDelta{}
	out <- types.TextContentDelta{Content: answer}
	out <- types.TextEndDelta{}
	close(out)
	return out, nil
}

func (m scriptedModel) ChatStreamWithSchema(ctx context.Context, msgs []types.Message, tools []types.ToolDef, _ *types.ParameterSchema) (<-chan types.Delta, error) {
	return m.ChatStream(ctx, msgs, tools)
}

// countingBatches counts the batches submitted and their sizes.
type countingBatches struct {
	*batch.Local
	mu    sync.Mutex
	sizes []int
}

func (c *countingBatches) Submit(ctx context.Context, reqs []types.BatchRequest, opts types.BatchSubmitOptions) (types.BatchHandle, error) {
	c.mu.Lock()
	c.sizes = append(c.sizes, len(reqs))
	c.mu.Unlock()
	return c.Local.Submit(ctx, reqs, opts)
}

func (c *countingBatches) Results(ctx context.Context, h types.BatchHandle) iter.Seq2[types.BatchResult, error] {
	return c.Local.Results(ctx, h)
}

func newBatchedModel() (*batch.Coalescer, *countingBatches) {
	cb := &countingBatches{Local: batch.NewLocal(scriptedModel{}, 8)}
	r := batch.NewRunner(cb, batch.NewMemoryStore(), batch.WithPollInterval(time.Millisecond, 5*time.Millisecond))
	return batch.NewCoalescer(r, batch.WithMaxWait(10*time.Second)), cb
}

func generatorSubject(gen Generator) Subject {
	return func(ctx context.Context, obs *Observation) error {
		var q string
		_ = json.Unmarshal(obs.Input, &q)
		out, err := gen.Generate(ctx, q)
		if err != nil {
			return err
		}
		obs.Output, _ = json.Marshal(out)
		return nil
	}
}

func batchDataset(n int) []Observation {
	obs := make([]Observation, n)
	for i := range obs {
		in, _ := json.Marshal(fmt.Sprintf("q%d", i))
		obs[i] = Observation{ID: fmt.Sprintf("case-%d", i), Input: in}
	}
	return obs
}

// TestWithBatchPopulateAndRun checks that a run's subject calls go out as
// one batch, its judge calls as another, and that each case gets its own
// answer back.
func TestWithBatchPopulateAndRun(t *testing.T) {
	c, cb := newBatchedModel()
	obs := batchDataset(5)
	ctx := context.Background()
	if err := PopulateAll(ctx, obs, generatorSubject(c), WithBatch(c)); err != nil {
		t.Fatal(err)
	}
	for i, o := range obs {
		var got string
		_ = json.Unmarshal(o.Output, &got)
		if got != fmt.Sprintf("answer:q%d", i) {
			t.Fatalf("case %d output = %q", i, got)
		}
	}
	suite, err := Run(ctx, "batched", obs, []Scorer{NewJudgeScorer(c)}, WithBatch(c))
	if err != nil {
		t.Fatal(err)
	}
	if suite.Aggregate["judge_score"] != 0.75 {
		t.Fatalf("aggregate = %v", suite.Aggregate)
	}
	if fmt.Sprint(cb.sizes) != "[5 5]" {
		t.Fatalf("batch sizes = %v, want [5 5]", cb.sizes)
	}
}

// TestWithBatchCompare checks that both arms of a comparison share a batch
// for their subjects and another for their judges.
func TestWithBatchCompare(t *testing.T) {
	c, cb := newBatchedModel()
	obs := batchDataset(3)
	cmp, err := Compare(context.Background(), obs, generatorSubject(c), generatorSubject(c), []Scorer{NewJudgeScorer(c)}, WithBatch(c))
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(cb.sizes) != "[6 6]" {
		t.Fatalf("batch sizes = %v, want [6 6]", cb.sizes)
	}
	if cmp == nil || len(cmp.Cases) == 0 {
		t.Fatalf("comparison = %+v", cmp)
	}
}
