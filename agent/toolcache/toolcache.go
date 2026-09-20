// Package toolcache memoizes tool results according to a types.CachePolicy.
//
// It is the tool-side counterpart of agent/provider/cache, which memoizes LLM
// responses. The two solve different problems: an LLM cache saves tokens on
// repeated prompts, a tool cache saves the round trip and the rate limit on
// repeated lookups. A long agent run calls the same search, the same file read,
// and the same schema fetch many times, because the model does not remember
// that it already did.
//
// The policy comes from the tool (types.Cacheable) unless the caller overrides
// it, so the decision about what is safe to cache stays with the code that
// knows.
package toolcache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// Entry is a cached tool result plus the metadata staleness decisions need.
type Entry struct {
	Result    types.ToolResult
	StoredAt  time.Time
	ExpiresAt time.Time
	Err       string // non-empty when a cached failure was stored
}

// Fresh reports whether the entry is within its TTL.
func (e Entry) Fresh(now time.Time) bool { return now.Before(e.ExpiresAt) }

// Servable reports whether an expired entry may still be served under the
// policy's staleness rules.
func (e Entry) Servable(now time.Time, p types.CachePolicy) bool {
	if e.Fresh(now) {
		return true
	}
	if !p.ServeStaleOnError || p.MaxStale <= 0 {
		return false
	}
	return now.Before(e.ExpiresAt.Add(p.MaxStale))
}

// Config wires a cached tool.
type Config struct {
	// Cache is the backing store. Required; without it the decorator is a
	// transparent passthrough rather than a silent no-cache surprise.
	Cache types.Cache[Entry]
	// ConfigKey identifies tool implementation, deployment and policy revision.
	// Empty creates a private wrapper namespace.
	ConfigKey string
	// Policy overrides the tool's own declaration. Use the zero value to take
	// the tool's, which is the intended path.
	Policy *types.CachePolicy
	// Scope keys separate namespaces per policy scope. Supply whichever apply:
	// "session", "agent", "user". A scope the policy asks for but that is not
	// supplied is an error at construction, because falling back to a wider
	// scope is how one user reads another's results.
	Scope map[types.CacheScope]string
	// Now overrides the clock, for tests.
	Now func() time.Time
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Metrics records hits and misses. Defaults to noop.
	Metrics types.Metrics
}

// Tool wraps a tool with policy-driven caching. It implements types.RichTool so
// rich results survive the cache, and forwards types.Cacheable so the policy
// stays visible through the decorator.
type Tool struct {
	inner  types.Tool
	policy types.CachePolicy
	cfg    Config
	prefix string

	// inflight collapses concurrent identical calls onto one execution. The
	// agent fans tool calls out in parallel, so without this a cold cache runs
	// the same expensive lookup several times at once and then caches it
	// several times over.
	mu       sync.Mutex
	inflight map[string]*call
}

type call struct {
	done chan struct{}
	res  types.ToolResult
	err  error
}

var (
	_ types.RichTool  = (*Tool)(nil)
	_ types.Cacheable = (*Tool)(nil)
)

// New wraps a tool with caching. It returns the tool unchanged when the
// resolved policy is disabled, so wiring code can wrap unconditionally.
func New(inner types.Tool, cfg Config) (types.Tool, error) {
	// A cache wrapped OUTSIDE markers hides them: the agent loop finds markers
	// with a *types.MarkedTool type assertion, and a decorator in front of one
	// makes that assertion fail, so the human-approval prompt is silently
	// skipped. The composition that works is markers outside, cache inside;
	// WrapAll does that rewrap automatically.
	if _, ok := inner.(*types.MarkedTool); ok {
		return nil, fmt.Errorf("toolcache: tool %q is marked for human approval; wrap the cache INSIDE the markers (types.WithMarkers(cached, markers...)), not around them, or the approval prompt is skipped", inner.Definition().Name)
	}

	policy := types.PolicyFor(inner)
	if cfg.Policy != nil {
		policy = *cfg.Policy
	}
	if !policy.Enabled {
		return inner, nil
	}
	if err := policy.Validate(); err != nil {
		return nil, fmt.Errorf("toolcache: tool %q: %w", inner.Definition().Name, err)
	}
	if cfg.Cache == nil {
		return nil, fmt.Errorf("toolcache: tool %q declares a cache policy but no Cache was supplied", inner.Definition().Name)
	}

	policy.KeyArgs = append([]string(nil), policy.KeyArgs...)
	policy.IgnoreArgs = append([]string(nil), policy.IgnoreArgs...)
	policy.VaryOnContext = append([]string(nil), policy.VaryOnContext...)
	if policy.MaxEntries != 0 {
		return nil, fmt.Errorf("toolcache: MaxEntries requires a dedicated bounded store; set its capacity and leave MaxEntries zero")
	}
	scope := policy.EffectiveScope()
	prefix := inner.Definition().Name
	if scope != types.CacheScopeGlobal {
		id, ok := cfg.Scope[scope]
		if !ok || id == "" {
			return nil, fmt.Errorf("toolcache: tool %q needs a %q scope key; caching without it would share results across %ss",
				inner.Definition().Name, scope, scope)
		}
		prefix = string(scope) + ":" + id + ":" + prefix
	}

	identity := cfg.ConfigKey
	if identity == "" {
		identity = types.NewID()
	}
	identityRaw, err := json.Marshal(struct {
		Scope, Config string
		Policy        types.CachePolicy
	}{prefix, identity, policy})
	if err != nil {
		return nil, err
	}
	identityHash := sha256.Sum256(identityRaw)
	prefix = hex.EncodeToString(identityHash[:])

	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Metrics == nil {
		cfg.Metrics = types.NoopMetrics{}
	}

	return &Tool{
		inner:    inner,
		policy:   policy,
		cfg:      cfg,
		prefix:   prefix,
		inflight: map[string]*call{},
	}, nil
}

// Definition delegates to the wrapped tool: caching is invisible to the model.
func (t *Tool) Definition() types.ToolDef { return t.inner.Definition() }

// CachePolicy implements types.Cacheable so a wrapped tool still reports what
// it does.
func (t *Tool) CachePolicy() types.CachePolicy {
	p := t.policy
	p.KeyArgs = append([]string(nil), p.KeyArgs...)
	p.IgnoreArgs = append([]string(nil), p.IgnoreArgs...)
	p.VaryOnContext = append([]string(nil), p.VaryOnContext...)
	return p
}

// Execute runs the tool through the cache and returns the text projection.
func (t *Tool) Execute(ctx context.Context, args map[string]any) (string, error) {
	res, err := t.ExecuteRich(ctx, args)
	return res.Text, err
}

// ExecuteRich returns a cached result when one is fresh, and otherwise runs the
// tool and stores the outcome.
func (t *Tool) ExecuteRich(ctx context.Context, args map[string]any) (types.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return types.ToolResult{}, err
	}
	key, err := t.key(ctx, args)
	if err != nil {
		return types.ToolResult{}, err
	}
	now := t.cfg.Now()

	if entry, found, err := t.cfg.Cache.Get(ctx, key); err == nil && found {
		if entry.Fresh(now) {
			t.cfg.Metrics.RecordToolCall(ctx, t.Definition().Name+".cache_hit", 0, nil)
			return t.replay(entry)
		}
		// An expired entry still counts as a miss: the round trip it exists to
		// save was made anyway, and a hit rate that counted it would hide a TTL
		// set too short to ever serve anything.
		t.cfg.Metrics.RecordToolCall(ctx, t.Definition().Name+".cache_miss", 0, nil)
		// The expired entry is kept in hand: if the refresh fails and the policy
		// allows stale reads, it is better than an error.
		res, err := t.refresh(ctx, key, args)
		if (err != nil || res.IsError) && ctx.Err() == nil && entry.Err == "" && !entry.Result.IsError && entry.Servable(t.cfg.Now(), t.policy) {
			t.cfg.Logger.Warn("toolcache: serving stale result after refresh failure",
				"tool", t.Definition().Name, "error", err, "age", now.Sub(entry.StoredAt))
			return t.replay(entry)
		}
		return res, err
	}

	t.cfg.Metrics.RecordToolCall(ctx, t.Definition().Name+".cache_miss", 0, nil)
	return t.refresh(ctx, key, args)
}

// replay returns a cached entry, restoring a cached error as an error result.
func (t *Tool) replay(e Entry) (types.ToolResult, error) {
	if e.Err != "" {
		res, err := cloneResult(e.Result)
		if err != nil {
			return types.ToolResult{}, err
		}
		return res, fmt.Errorf("%s", e.Err)
	}
	return cloneResult(e.Result)
}

// refresh executes the tool, collapsing concurrent identical calls, and stores
// the outcome when the policy permits.
func (t *Tool) refresh(ctx context.Context, key string, args map[string]any) (types.ToolResult, error) {
	for {
		t.mu.Lock()
		if c, ok := t.inflight[key]; ok {
			t.mu.Unlock()
			select {
			case <-c.done:
				// The leader's cancellation is the leader's alone. Adopting it
				// would fail a follower whose own context is still live, which
				// turns one cancelled call into several, so retry instead: the
				// next pass either leads the execution or joins a new leader.
				if isContextErr(c.err) && ctx.Err() == nil {
					continue
				}
				return detachedResult(c.res, c.err)
			case <-ctx.Done():
				return types.ToolResult{}, ctx.Err()
			}
		}
		// A previous leader may have committed after this caller's first Get.
		if entry, found, err := t.cfg.Cache.Get(ctx, key); err == nil && found && entry.Fresh(t.cfg.Now()) {
			t.mu.Unlock()
			return t.replay(entry)
		}
		c := &call{done: make(chan struct{})}
		t.inflight[key] = c
		t.mu.Unlock()

		c.res, c.err = t.execute(ctx, args)
		if detached, cloneErr := cloneResult(c.res); cloneErr != nil {
			c.res, c.err = types.ToolResult{}, cloneErr
		} else {
			c.res = detached
		}
		t.store(ctx, key, c.res, c.err, t.cfg.Now())

		// Deregister before waking followers, so a follower that retries a
		// cancelled execution finds no stale leader to attach to.
		t.mu.Lock()
		delete(t.inflight, key)
		t.mu.Unlock()
		close(c.done)

		return detachedResult(c.res, c.err)
	}
}

// isContextErr reports whether an error came from a context ending rather than
// from the tool itself.
func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (t *Tool) execute(ctx context.Context, args map[string]any) (result types.ToolResult, err error) {
	defer func() {
		if p := recover(); p != nil {
			result = types.ToolResult{}
			err = fmt.Errorf("toolcache: tool panic: %v", p)
		}
	}()
	if rt, ok := t.inner.(types.RichTool); ok {
		return rt.ExecuteRich(ctx, args)
	}
	text, err := t.inner.Execute(ctx, args)
	return types.ToolResult{Text: text, IsError: err != nil}, err
}

// store writes the outcome, honouring CacheErrors. A failure to write is
// logged, never propagated: a broken cache must not break a working tool.
func (t *Tool) store(ctx context.Context, key string, res types.ToolResult, execErr error, now time.Time) {
	failed := execErr != nil || res.IsError
	if failed && (!t.policy.CacheErrors || t.policy.ServeStaleOnError) {
		return
	}
	detached, err := cloneResult(res)
	if err != nil {
		return
	}
	entry := Entry{
		Result:    detached,
		StoredAt:  now,
		ExpiresAt: now.Add(t.policy.TTL),
	}
	if execErr != nil {
		entry.Err = execErr.Error()
	}
	retention := t.policy.TTL
	if t.policy.ServeStaleOnError {
		retention += t.policy.MaxStale
	}
	if err := t.cfg.Cache.Set(ctx, key, entry, retention); err != nil {
		t.cfg.Logger.Warn("toolcache: store failed", "tool", t.Definition().Name, "error", err)
	}
}

// key builds the cache key from the scoped prefix, the policy's chosen
// arguments, and the context knobs the policy says the result varies on.
//
// Hashing rather than concatenating keeps keys bounded for tools whose
// arguments are large, and the prefix stays readable so a store can be
// inspected by tool name.
func (t *Tool) key(ctx context.Context, args map[string]any) (string, error) {
	selected := map[string]any{}
	for _, name := range t.policy.KeyArguments(args) {
		if v, ok := args[name]; ok {
			selected[name] = v
		}
	}
	varying := map[string]any{}
	tc := types.ToolContextFrom(ctx)
	for _, name := range t.policy.VaryOnContext {
		if v, ok := tc.Value(name); ok {
			varying[name] = v
		}
	}
	raw, err := json.Marshal(struct{ Arguments, Context map[string]any }{selected, varying})
	if err != nil {
		return "", fmt.Errorf("toolcache: unserializable cache identity: %w", err)
	}
	hash := sha256.Sum256(raw)
	return t.prefix + ":" + hex.EncodeToString(hash[:]), nil
}

func detachedResult(res types.ToolResult, execErr error) (types.ToolResult, error) {
	copy, err := cloneResult(res)
	if err != nil {
		return types.ToolResult{}, err
	}
	return copy, execErr
}

func cloneResult(res types.ToolResult) (types.ToolResult, error) {
	res.Blocks = append([]types.ToolResultBlock(nil), res.Blocks...)
	for i := range res.Blocks {
		res.Blocks[i].Data = append([]byte(nil), res.Blocks[i].Data...)
		res.Blocks[i].JSON = append(json.RawMessage(nil), res.Blocks[i].JSON...)
	}
	res.Citations = append([]types.Citation(nil), res.Citations...)
	for i := range res.Citations {
		if res.Citations[i].Meta == nil {
			continue
		}
		raw, err := json.Marshal(res.Citations[i].Meta)
		if err != nil {
			return types.ToolResult{}, fmt.Errorf("toolcache: citation metadata: %w", err)
		}
		var meta map[string]any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&meta); err != nil {
			return types.ToolResult{}, err
		}
		res.Citations[i].Meta = meta
	}
	return res, nil
}

// WrapAll wraps every tool in a registry that declares a cache policy, and
// returns a new registry. Tools with no policy pass through untouched, so this
// is safe to call on a mixed set.
//
// A marked tool is unwrapped, cached, and re-marked, so the human-approval
// prompt still fires on every call and only the underlying work is memoized.
// Caching outside the markers would skip the prompt entirely.
func WrapAll(src *types.ToolRegistry, cfg Config) (*types.ToolRegistry, error) {
	out := types.NewToolRegistry()
	for _, tool := range src.All() {
		if mt, ok := tool.(*types.MarkedTool); ok {
			cached, err := New(mt.Inner, cfg)
			if err != nil {
				return nil, err
			}
			out.Register(types.WithMarkers(cached, mt.Markers...))
			continue
		}
		wrapped, err := New(tool, cfg)
		if err != nil {
			return nil, err
		}
		out.Register(wrapped)
	}
	return out, nil
}
