package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/urmzd/saige/agent/batch"
	"github.com/urmzd/saige/agent/convert"
	"github.com/urmzd/saige/agent/provider/wrapper"
	"github.com/urmzd/saige/agent/types"
)

// BatchConfig configures RunBatch and AIFunction.Batch.
//
// Batches are single-turn: each input is one model call, and its answer is
// the result. The agent's tools are not offered, because a tool call would
// need another turn, and in a batch each turn waits for the whole batch,
// which takes minutes to hours. Run multi-turn loops interactively.
type BatchConfig struct {
	// Runner runs the job. It decides the provider, the job store and the
	// budget. Nil builds one over Provider with an in-memory store, charged
	// to the agent's budget.
	Runner *batch.Runner
	// Provider serves the batch when Runner is nil. Nil finds a
	// types.BatchProvider behind the agent's provider (an adapter, through
	// any decorators), else runs the calls locally with batch.NewLocal.
	Provider types.BatchProvider
	// JobID is the job's idempotency key: submitting the same ID and inputs
	// again resumes the job. Empty derives it from the requests.
	JobID string
	// Concurrency bounds the local fallback. Zero means 4.
	Concurrency int
}

// BatchInput is one input of RunBatch.
type BatchInput struct {
	// ID keys the result. Empty uses the input's index.
	ID       string
	Messages []types.Message
}

// RunBatch answers each input with one model call, submitted together as
// one batch, and returns the results in input order. The system prompt,
// dials, dial policy and response schema of the agent apply to every call.
// See BatchConfig for why only single-turn work is batched.
func (a *Agent) RunBatch(ctx context.Context, inputs []BatchInput, cfg BatchConfig) ([]types.BatchResult, error) {
	opts := types.RequestOptions{DialPolicy: a.dialPolicy(ctx)}
	if !a.cfg.Dials.IsZero() {
		opts.DialLayers = []types.DialLayer{{Scope: types.DialScopeAgent, Dials: a.cfg.Dials.Clone()}}
	}
	schema := a.cfg.ResponseSchema
	if schema != nil && a.cfg.OutputMode != OutputAuto && a.cfg.OutputMode != OutputNative {
		return nil, fmt.Errorf("%w: a batch sends a response schema natively; output mode %q needs the agent loop", types.ErrInvalidModelConfig, a.cfg.OutputMode)
	}
	reqs := make([]types.BatchRequest, len(inputs))
	for i, in := range inputs {
		id := in.ID
		if id == "" {
			id = strconv.Itoa(i)
		}
		msgs := in.Messages
		if a.cfg.SystemPrompt != "" {
			msgs = append([]types.Message{types.SystemMsg(types.Text(a.cfg.SystemPrompt))}, msgs...)
		}
		reqs[i] = types.BatchRequest{CustomID: id, Messages: msgs, Schema: schema, Options: opts.Clone()}
	}
	// The agent's conversion policy applies to every request, planned when
	// the batch is submitted; converters that call a model are charged to
	// the agent's budget.
	ctx = convert.WithRuntime(ctx, convert.Runtime{Policy: a.cfg.Conversion, Budget: a.cfg.Budget})
	return runBatch(ctx, a.cfg.Provider, a.cfg.Budget, cfg, "agent-"+a.cfg.Name, reqs)
}

// runBatch runs reqs as one job on the runner cfg names or builds.
func runBatch(ctx context.Context, p types.Provider, budget *types.Budget, cfg BatchConfig, prefix string, reqs []types.BatchRequest) ([]types.BatchResult, error) {
	r := cfg.Runner
	if r == nil {
		bp := cfg.Provider
		if bp == nil {
			bp = BatchProviderFor(p, cfg.Concurrency)
		}
		if bp == nil {
			return nil, errors.New("agent: batch needs a provider")
		}
		var ropts []batch.RunnerOption
		if budget != nil {
			ropts = append(ropts, batch.WithBudget(budget))
		}
		r = batch.NewRunner(bp, batch.NewMemoryStore(), ropts...)
	}
	id := cfg.JobID
	if id == "" {
		provider, model := types.NameOf(p), types.ProviderModel(p)
		m, err := batch.Manifest(provider, model, reqs)
		if err != nil {
			return nil, err
		}
		id = prefix + "-" + m[:20]
	}
	return r.Run(ctx, id, reqs)
}

// BatchProviderFor returns the types.BatchProvider behind p: the first
// provider in its decorator chain that implements one, else a local batch
// over p with the given concurrency. It returns nil for a nil p.
//
// A vendor batch provider is returned behind the conversion decorator
// (convert.Batch), so each request is planned against the offering at
// submit and a request the offering cannot take fails before upload: with
// the policy of the conversion decorator in p's chain when there is one,
// else with the default policy, which rejects media the offering does not
// take. A local batch makes ordinary calls through p, which converts them.
func BatchProviderFor(p types.Provider, concurrency int) types.BatchProvider {
	if p == nil {
		return nil
	}
	if bp, ok := wrapper.As[types.BatchProvider](p); ok {
		if cp, ok := wrapper.As[*convert.Provider](p); ok {
			return cp.Batch(bp)
		}
		cb, err := convert.NewBatch(bp, convert.Config{})
		if err != nil {
			return bp
		}
		return cb
	}
	return batch.NewLocal(p, concurrency)
}

// AIBatchItem is one answer of AIFunction.Batch.
type AIBatchItem[Out any] struct {
	Out Out
	// Result is the raw batch result, with usage and the answer text.
	Result types.BatchResult
	// Err is the request's error, or an error wrapping ErrSchemaInvalid when
	// the answer did not decode into Out. Answers are not repaired: a
	// repair is a second turn.
	Err error
}

// Batch computes the function for every input as one batch and returns the
// answers in input order. The output schema is sent natively, so the model
// must support structured output. Item errors are per input; the returned
// error is for the batch as a whole.
func (f *AIFunction[In, Out]) Batch(ctx context.Context, inputs []In, cfg BatchConfig) ([]AIBatchItem[Out], error) {
	schema, err := outputSchema[Out](nil)
	if err != nil {
		return nil, err
	}
	provider := f.cfg.Provider
	if f.cfg.Preset != nil {
		provider = f.cfg.Preset.Provider()
	}
	reqs := make([]types.BatchRequest, len(inputs))
	for i, in := range inputs {
		prompt, err := f.Render(in)
		if err != nil {
			return nil, fmt.Errorf("input %d: %w", i, err)
		}
		var msgs []types.Message
		if f.cfg.System != "" {
			msgs = append(msgs, types.SystemMsg(types.Text(f.cfg.System)))
		}
		reqs[i] = types.BatchRequest{CustomID: strconv.Itoa(i), Messages: append(msgs, types.UserMsg(types.Text(prompt))), Schema: schema}
	}
	results, err := runBatch(ctx, provider, nil, cfg, "aifunc-"+f.name+"-"+f.version, reqs)
	if err != nil {
		return nil, err
	}
	items := make([]AIBatchItem[Out], len(results))
	for i, res := range results {
		items[i].Result = res
		if res.Err != nil {
			items[i].Err = res.Err
			continue
		}
		raw, err := ExtractJSON(res.Text())
		if err == nil {
			err = json.Unmarshal([]byte(raw), &items[i].Out)
		}
		if err != nil {
			items[i].Err = fmt.Errorf("%w: %w", ErrSchemaInvalid, err)
		}
	}
	return items, nil
}
