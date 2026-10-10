package batch

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// Coalescer is a types.Provider that gathers concurrent calls into batches.
// Each Stream call blocks until its batch ends and then replays the
// result as a stream, so code written for a streaming provider, such as an
// eval subject or a judge, runs its calls through a vendor batch unchanged.
//
// A batch is flushed when every participant (see Join) is waiting on a
// call, when no new call arrived for the idle window and nobody joined, or
// when it reaches the request limit. Request IDs come from the key on the
// call's context (WithKey) and an ordinal per key, so results map back to
// the case and sample that asked. With stable keys a restarted run sends the
// same batches, and the Runner resumes them instead of submitting again.
//
// Calls inside one batch must be independent: a caller that needs one
// answer before asking the next waits for a batch per step.
type Coalescer struct {
	runner  *Runner
	window  time.Duration
	maxWait time.Duration
	limit   int
	prefix  string

	mu       sync.Mutex
	pending  []*call
	holders  int
	waiting  int
	ordinals map[string]int
	timer    *time.Timer
	timerGen int
}

type call struct {
	req  types.BatchRequest
	done chan types.BatchResult
}

var (
	_ types.Provider                 = (*Coalescer)(nil)
	_ types.StructuredOutputProvider = (*Coalescer)(nil)
	_ types.OptionsProvider          = (*Coalescer)(nil)
	_ types.NamedProvider            = (*Coalescer)(nil)
	_ types.ModelProvider            = (*Coalescer)(nil)
)

// CoalescerOption configures a Coalescer.
type CoalescerOption func(*Coalescer)

// WithWindow sets the idle window: with no participants, a batch is sent
// once no call arrived for this long. Default 2s.
func WithWindow(d time.Duration) CoalescerOption {
	return func(c *Coalescer) {
		if d > 0 {
			c.window = d
		}
	}
}

// WithMaxWait bounds how long a call waits to be sent while participants are
// still working, in case one of them blocks on something other than a model
// call. Default 1m.
func WithMaxWait(d time.Duration) CoalescerOption {
	return func(c *Coalescer) {
		if d > 0 {
			c.maxWait = d
		}
	}
}

// WithMaxRequests sends a batch as soon as it holds n calls. Default 10000.
func WithMaxRequests(n int) CoalescerOption {
	return func(c *Coalescer) {
		if n > 0 {
			c.limit = n
		}
	}
}

// WithJobPrefix prefixes the job IDs of the batches, such as an eval run
// name. Default "coalesce".
func WithJobPrefix(p string) CoalescerOption {
	return func(c *Coalescer) {
		if p != "" {
			c.prefix = p
		}
	}
}

// NewCoalescer returns a coalescer that runs its batches on r.
func NewCoalescer(r *Runner, opts ...CoalescerOption) *Coalescer {
	c := &Coalescer{runner: r, window: 2 * time.Second, maxWait: time.Minute, limit: 10000,
		prefix: "coalesce", ordinals: map[string]int{}}
	for _, o := range opts {
		o(c)
	}
	return c
}

type keyCtx struct{}

// WithKey names the calls made under ctx, such as an eval case and sample.
// Request IDs are the key plus an ordinal.
func WithKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, keyCtx{}, key)
}

// KeyFrom returns the key set by WithKey.
func KeyFrom(ctx context.Context) string {
	k, _ := ctx.Value(keyCtx{}).(string)
	return k
}

// Join registers a participant, a worker that will make calls. While every
// participant is waiting on a call, the batch is sent at once. Call the
// returned function when the participant is done; it is safe to call twice.
func (c *Coalescer) Join() (leave func()) {
	c.mu.Lock()
	c.holders++
	c.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			c.holders--
			c.checkLocked()
			c.mu.Unlock()
		})
	}
}

// Flush sends whatever is pending now.
func (c *Coalescer) Flush() {
	c.mu.Lock()
	c.flushLocked()
	c.mu.Unlock()
}

// Name implements types.NamedProvider.
func (c *Coalescer) Name() string { p, _ := c.runner.identity(); return p }

// Model implements types.ModelProvider.
func (c *Coalescer) Model() string { _, m := c.runner.identity(); return m }

// Stream implements types.Provider.
func (c *Coalescer) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	br := types.BatchRequest{Messages: req.Messages, Tools: req.Tools, Schema: req.Schema}
	if req.Options != nil {
		br.Options = *req.Options
	}
	return c.do(ctx, br)
}

// SupportsSchema implements types.StructuredOutputProvider.
func (c *Coalescer) SupportsSchema() bool { return true }

// SupportsOptions implements types.OptionsProvider.
func (c *Coalescer) SupportsOptions() bool { return true }

// Generate sends one user prompt and returns the answer text, the seam eval
// judges use.
func (c *Coalescer) Generate(ctx context.Context, prompt string) (string, error) {
	return c.text(ctx, types.BatchRequest{Messages: []types.Message{types.UserMsg(types.Text(prompt))}})
}

// GenerateStructured is Generate with the answer constrained to a JSON
// Schema, as eval.StructuredGenerator asks.
func (c *Coalescer) GenerateStructured(ctx context.Context, prompt string, schema json.RawMessage) (string, error) {
	var ps types.ParameterSchema
	if err := json.Unmarshal(schema, &ps); err != nil {
		return "", fmt.Errorf("%w: schema: %w", types.ErrInvalidModelConfig, err)
	}
	return c.text(ctx, types.BatchRequest{Messages: []types.Message{types.UserMsg(types.Text(prompt))}, Schema: &ps})
}

func (c *Coalescer) text(ctx context.Context, req types.BatchRequest) (string, error) {
	res, err := c.wait(ctx, req)
	if err != nil {
		return "", err
	}
	if res.Err != nil {
		return "", res.Err
	}
	return res.Text(), nil
}

func (c *Coalescer) do(ctx context.Context, req types.BatchRequest) (<-chan types.Delta, error) {
	res, err := c.wait(ctx, req)
	if err != nil {
		return nil, err
	}
	return Stream(res), nil
}

func (c *Coalescer) wait(ctx context.Context, req types.BatchRequest) (types.BatchResult, error) {
	cl := &call{req: req, done: make(chan types.BatchResult, 1)}
	c.mu.Lock()
	key := sanitizeKey(KeyFrom(ctx))
	n := c.ordinals[key]
	c.ordinals[key] = n + 1
	cl.req.CustomID = key + "#" + strconv.Itoa(n)
	c.pending = append(c.pending, cl)
	c.waiting++
	c.checkLocked()
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.waiting--
		c.mu.Unlock()
	}()
	select {
	case res := <-cl.done:
		return res, nil
	case <-ctx.Done():
		return types.BatchResult{}, ctx.Err()
	}
}

func sanitizeKey(k string) string {
	if k == "" {
		return "call"
	}
	return strings.ReplaceAll(k, "#", "_")
}

// checkLocked flushes when the batch is full or every participant waits,
// and otherwise arms the timer.
func (c *Coalescer) checkLocked() {
	if len(c.pending) == 0 {
		return
	}
	if len(c.pending) >= c.limit || (c.holders > 0 && c.waiting >= c.holders) {
		c.flushLocked()
		return
	}
	d := c.window
	if c.holders > 0 {
		d = c.maxWait
	}
	c.armLocked(d)
}

func (c *Coalescer) armLocked(d time.Duration) {
	if c.timer != nil {
		c.timer.Stop()
	}
	c.timerGen++
	gen := c.timerGen
	c.timer = time.AfterFunc(d, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if gen == c.timerGen {
			c.flushLocked()
		}
	})
}

func (c *Coalescer) flushLocked() {
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
		c.timerGen++
	}
	if len(c.pending) == 0 {
		return
	}
	calls := c.pending
	c.pending = nil
	sort.Slice(calls, func(a, b int) bool { return calls[a].req.CustomID < calls[b].req.CustomID })
	go c.dispatch(calls)
}

// dispatch runs one batch to its end and hands each call its result.
func (c *Coalescer) dispatch(calls []*call) {
	ctx := context.Background()
	reqs := make([]types.BatchRequest, len(calls))
	for i, cl := range calls {
		reqs[i] = cl.req
	}
	fail := func(err error) {
		for _, cl := range calls {
			cl.done <- types.BatchResult{CustomID: cl.req.CustomID, Outcome: types.BatchErrored,
				Err: &types.BatchRequestError{Outcome: types.BatchErrored, Message: err.Error(), Err: err}}
		}
	}
	provider, model := c.runner.identity()
	manifest, err := Manifest(provider, model, reqs)
	if err != nil {
		fail(err)
		return
	}
	results, err := c.runner.Run(ctx, c.prefix+"-"+manifest[:20], reqs)
	if err != nil {
		fail(err)
		return
	}
	for i, cl := range calls {
		cl.done <- results[i]
	}
}
