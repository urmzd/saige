package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/durable"
	"github.com/urmzd/saige/durable/ledger"
	dqueue "github.com/urmzd/saige/durable/queue"
	"github.com/urmzd/saige/durable/replay"
	drun "github.com/urmzd/saige/durable/run"
	"github.com/urmzd/saige/durable/worker"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/attachment"
	"github.com/urmzd/saige/flow/definition"
	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/runtime"
	"github.com/urmzd/saige/flow/thread"
	"github.com/urmzd/saige/flow/turn/native"
)

// ── fixtures ────────────────────────────────────────────────────────

// specs is a control plane's definitions, complete enough to be revised and
// to answer a pinned lookup after it has been. Every tenant reads the same
// definitions, as the fixtures here are not about tenancy.
type specs struct {
	*definition.Resolver
	src *anyTenant

	mu       sync.Mutex
	versions map[string]int // agent -> latest major version
}

func newSpecs() *specs {
	src := &anyTenant{Source: definition.NewMemory(), tampered: map[string]string{}}
	return &specs{Resolver: definition.NewResolver(src, definition.Checks{}), src: src, versions: map[string]int{}}
}

// specContent is the definition file every fixture agent is written as.
func specContent(agent, version, prompt string) string {
	return fmt.Sprintf("---\napiVersion: saige/v1\nname: %s\nversion: %s\nmodel: offline/scripted\n---\n%s\n", agent, version, prompt)
}

// revise writes the next major version of an agent and returns it.
func (s *specs) revise(agent, prompt string) native.Revision {
	return s.set(native.Spec{Name: agent, Provider: native.ProviderOffline, Model: "scripted", SystemPrompt: prompt})
}

// set writes spec as the next major version of its agent and returns it. The
// agents it delegates to must already exist.
func (s *specs) set(spec native.Spec) native.Revision {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.versions[spec.Name]++
	old := definition.AgentSpec{
		Name: spec.Name, Description: spec.Description, Model: spec.Model, Provider: spec.Provider,
		SystemPrompt: spec.SystemPrompt, Tools: spec.Tools,
		Access:     definition.AgentSpecAccess{Spawn: spec.Access.Spawn, WorkspaceAreas: spec.Access.WorkspaceAreas},
		FallbackOn: spec.FallbackOn, Approvals: spec.Approvals, Approvers: spec.Approvers,
		SelfApproval: spec.SelfApproval, Answerers: spec.Answerers, TaggedOnly: spec.TaggedOnly,
	}
	for _, f := range spec.Fallbacks {
		old.Fallbacks = append(old.Fallbacks, definition.AgentSpecModel{Provider: f.Provider, Model: f.Model})
	}
	for _, c := range spec.Sources {
		old.Sources = append(old.Sources, definition.Corpus{Name: c.Name, URI: c.URI, Options: c.Options})
	}
	rec, err := definition.FromAgentSpec(old, s.versions[spec.Name])
	if err != nil {
		panic(err)
	}
	rec.Tenant, rec.CreatedBy = "acme", "test"
	if rec, err = s.src.Put(context.Background(), rec); err != nil {
		panic(err)
	}
	a, err := definition.Parse(rec.Content, rec.Policy, definition.Checks{})
	if err != nil {
		panic(err)
	}
	// The digest a run reports is the resolved one, which needs every
	// sub-agent to exist. One written before its sub-agents reports the
	// file's own until then.
	digest := rec.Digest
	if pin, err := s.ResolveAt(context.Background(), fixtureTenant, rec.Name, rec.Version); err == nil {
		digest = pin.Digest
	}
	rev := native.Revision{Name: rec.Name, Version: rec.Version, Digest: digest, Spec: a.Spec}
	fixtures.Store(pinKey(rev), s)
	return rev
}

// latest pins the latest version of agent, as accept would.
func (s *specs) latest(agent string) (native.Revision, error) {
	pin, err := s.Resolve(context.Background(), fixtureTenant, agent)
	if err != nil {
		return native.Revision{}, err
	}
	loaded, err := s.Verify(context.Background(), fixtureTenant, pin)
	if err != nil {
		return native.Revision{}, err
	}
	rev := native.Revision{Name: pin.Name, Version: pin.Version, Digest: pin.Digest, Spec: loaded.Agent.Spec}
	fixtures.Store(pinKey(rev), s)
	return rev, nil
}

// fixtures maps every version a fixture wrote to the fixture, so a test that
// calls an executor directly can send the request accept would have built.
var fixtures sync.Map

func pinKey(rev native.Revision) string { return rev.Name + "@" + rev.Version + "#" + rev.Digest }

// withPin is req for rev, pinned now as accept pins it.
func withPin(req runtime.Request, rev native.Revision) runtime.Request {
	v, ok := fixtures.Load(pinKey(rev))
	if !ok {
		panic("no fixture wrote " + pinKey(rev))
	}
	s := v.(*specs)
	pin, err := s.ResolveAt(context.Background(), fixtureTenant, rev.Name, rev.Version)
	if err != nil {
		panic(err)
	}
	loaded, err := s.Verify(context.Background(), fixtureTenant, pin)
	if err != nil {
		panic(err)
	}
	req.Definition = native.Revision{Name: pin.Name, Version: pin.Version, Digest: pin.Digest, Spec: loaded.Agent.Spec}
	req.Pin, req.Policy = pin, loaded.Agent.Policy
	return req
}

// edit writes the next version of an agent with its latest spec changed by
// mod.
func (s *specs) edit(agent string, mod func(*native.Spec)) native.Revision {
	rec, err := s.src.Latest(context.Background(), fixtureTenant, agent)
	if err != nil {
		panic(err)
	}
	a, err := definition.Parse(rec.Content, rec.Policy, definition.Checks{})
	if err != nil {
		panic(err)
	}
	spec := a.Spec
	mod(&spec)
	return s.set(spec)
}

// corrupt makes a stored version read back with other content, which only a
// broken control plane would do. It exists so a test can prove that flow
// notices.
func (s *specs) corrupt(agent string, major int, prompt string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.src.mu.Lock()
	defer s.src.mu.Unlock()
	s.src.tampered[agent+"@"+fmt.Sprintf("%d.0.0", major)] = prompt
}

// anyTenant is a Source that keeps every tenant's definitions as one, and can
// read a version back tampered with.
type anyTenant struct {
	definition.Source
	mu       sync.Mutex
	tampered map[string]string // name@version -> prompt
}

const fixtureTenant = "any"

func (a *anyTenant) Put(ctx context.Context, r definition.Record) (definition.Record, error) {
	r.Tenant = fixtureTenant
	return a.Source.Put(ctx, r)
}

func (a *anyTenant) Get(ctx context.Context, _, name, version string) (definition.Record, error) {
	r, err := a.Source.Get(ctx, fixtureTenant, name, version)
	a.mu.Lock()
	prompt, ok := a.tampered[name+"@"+version]
	a.mu.Unlock()
	if err == nil && ok {
		r.Content = specContent(name, version, prompt)
	}
	return r, err
}

func (a *anyTenant) Versions(ctx context.Context, _, name string) ([]definition.Record, error) {
	return a.Source.Versions(ctx, fixtureTenant, name)
}

func (a *anyTenant) Latest(ctx context.Context, _, name string) (definition.Record, error) {
	return a.Source.Latest(ctx, fixtureTenant, name)
}

func (a *anyTenant) List(ctx context.Context, _ string, page definition.Page) ([]definition.Record, string, error) {
	return a.Source.List(ctx, fixtureTenant, page)
}

func (a *anyTenant) Retire(ctx context.Context, _, name, by string) error {
	return a.Source.Retire(ctx, fixtureTenant, name, by)
}

// recorder is a bus that keeps every Publish call, deduplication included, so a
// test can assert on what the runtime chose to publish rather than on what a
// broker decided to keep.
type recorder struct {
	mu     sync.Mutex
	events []event.Envelope
	fail   func(event.Envelope) error
}

func (r *recorder) Publish(_ context.Context, e event.Envelope) error {
	r.mu.Lock()
	fail := r.fail
	r.mu.Unlock()
	if fail != nil {
		if err := fail(e); err != nil {
			return err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return nil
}

func (r *recorder) all() []event.Envelope {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]event.Envelope(nil), r.events...)
}

func (r *recorder) kinds() []string {
	out := []string{}
	for _, e := range r.all() {
		out = append(out, e.Type)
	}
	return out
}

func (r *recorder) ofKind(k string) []event.Envelope {
	var out []event.Envelope
	for _, e := range r.all() {
		if e.Type == k {
			out = append(out, e)
		}
	}
	return out
}

func (r *recorder) failWhen(fn func(event.Envelope) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fail = fn
}

// executor is a turn that emits a scripted stream and counts how often it
// actually ran, which is how a test sees whether replay paid for the model twice.
type executor struct {
	mu       sync.Mutex
	runs     int
	requests []runtime.Request
	chunks   []event.Body
	err      error
	failures int
	answer   string
	// hook, when set, runs at the start of every execution, before anything
	// is emitted. A test uses it to change the world between two attempts.
	hook func(runtime.Request)
	// prompt, when set, is what the executor reports having sent the model.
	prompt func(runtime.Request) string
}

func newExecutor(answer string, chunks ...event.Body) *executor {
	return &executor{answer: answer, chunks: chunks}
}

// failNext makes the next n executions fail with err and every execution after
// them succeed. Setting err directly instead makes every execution fail, which
// is what a permanent-failure test wants.
//
// A retry test needs this rather than the obvious handshake of "wait until the
// test sees a failure, then clear err". The retry backoff here is a
// millisecond, so the second attempt can start before the waiting goroutine
// wakes; the observed failure count then goes straight from zero to two, a
// poll for exactly one never holds again, and the test waits forever rather
// than failing fast. Deciding the number of failures up front removes the
// coordination, and with it the race.
func (e *executor) failNext(n int, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.err, e.failures = err, n
}

func (e *executor) Execute(ctx context.Context, req runtime.Request, emit func(context.Context, event.Body) error) (runtime.Outcome, error) {
	e.mu.Lock()
	e.runs++
	e.requests = append(e.requests, req)
	chunks, failure, answer := e.chunks, e.err, e.answer
	hook, prompt := e.hook, e.prompt
	// A budgeted failure is spent here, so the next attempt succeeds without
	// anyone having to heal the executor from outside.
	if e.failures > 0 {
		e.failures--
		if e.failures == 0 {
			e.err = nil
		}
	}
	e.mu.Unlock()

	if hook != nil {
		hook(req)
	}
	for _, c := range chunks {
		if err := emit(ctx, c); err != nil {
			return runtime.Outcome{}, err
		}
	}
	if failure != nil {
		return runtime.Outcome{}, failure
	}
	out := runtime.Outcome{Text: answer, Chunks: len(chunks)}
	if prompt != nil {
		out.Prompt = prompt(req)
	}
	return out, nil
}

func (e *executor) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.runs
}

func (e *executor) seen() []runtime.Request {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]runtime.Request(nil), e.requests...)
}

func textChunk(t *testing.T, s string) event.Body {
	t.Helper()
	return event.Text{Text: s}
}

// harness is a runtime wired to in-memory durable storage.
type harness struct {
	rt     *runtime.Runtime
	bus    *recorder
	specs  *specs
	exec   *executor
	ledger *ledger.Memory
	queue  *dqueue.Memory
}

func newHarness(t *testing.T, exec *executor, opts ...runtime.Option) *harness {
	t.Helper()
	lgr := ledger.NewMemory()
	q := dqueue.NewMemory()
	bus := &recorder{}
	sp := newSpecs()
	rt, err := runtime.New(durable.New(lgr, q), sp, bus, append([]runtime.Option{runtime.WithRuntime(runtime.AsRuntime(exec))}, opts...)...)
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	return &harness{rt: rt, bus: bus, specs: sp, exec: exec, ledger: lgr, queue: q}
}

// work starts a worker for the duration of the test.
func (h *harness) work(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = worker.New(h.ledger, h.queue,
			worker.WithRegistry(h.rt.Registry()),
			worker.WithConcurrency(1),
			worker.WithJanitorEvery(0),
			worker.WithLeaseTTL(2*time.Second),
			worker.WithBackoff(func(int) time.Duration { return time.Millisecond }),
		).Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// fixture is a request a test raises: the fields of the event it posts and
// the agent that answers it.
type fixture struct {
	ID          string
	Tenant      string
	Agent       string
	Origin      string
	Source      address.Address
	Text        string
	Actor       event.Actor
	Meta        map[string]string
	Attachments []attachment.Attachment
	At          time.Time
}

// event is the message.posted.v1 the fixture posts. With a source it
// publishes on that surface; without one it has no surface to answer into.
func (f fixture) event() event.Envelope {
	e := event.Envelope{
		ID:     f.ID,
		Type:   event.TypeMessagePosted,
		Time:   f.At,
		Tenant: f.Tenant,
		Data: event.MessagePosted{Request: event.Request{
			Text: f.Text, Actor: f.Actor, Mentions: []string{f.Agent},
			Attachments: f.Attachments, Context: f.Meta,
		}},
	}
	if f.Source.IsZero() {
		e.Source = f.Origin + ":" + f.Tenant
		e.Channel = f.Origin + "://" + f.Tenant + "/" + f.Origin
	} else {
		e.Source = f.Source.String()
		e.Channel = e.Source
	}
	return e
}

// runID is the run Accept starts for the fixture.
func (f fixture) runID() string { return runtime.RunKey(f.Tenant, f.ID, f.Agent) }

// key is the thread the fixture's run journals into.
func (f fixture) key() thread.Key { return thread.KeyOf(f.event(), f.Agent) }

// accept accepts the fixture's run.
func accept(ctx context.Context, rt *runtime.Runtime, f fixture) (runtime.Acceptance, error) {
	return rt.Accept(ctx, f.event(), f.Agent)
}

// contextOf reads a request's host context, for a message id function.
func contextOf(e event.Envelope, key string) string {
	r, _ := event.RequestOf(e.Data)
	return r.Context[key]
}

func tag(agent, id, text string) fixture {
	return fixture{
		ID:     id,
		Tenant: "acme",
		Agent:  agent,
		Origin: "github",
		Source: address.MustParse("github://urmzd/fiber/issues/42"),
		Text:   text,
		At:     time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC),
	}
}

// ── tests ───────────────────────────────────────────────────────────

// A run's identity is a function of the tenant and the tag id, and of nothing
// else: a redelivery lands on the run it duplicated, any tag id becomes a
// usable topic segment, and distinct inputs never share a run.
func TestRunIDIsADeterministicTopicSafeFunctionOfTenantAndTagID(t *testing.T) {
	t.Parallel()

	ids := []string{"Ev01ABCDEF", "72d3162e-cc78-11e3-81ab-4c9367dc0958", "slack:1699123456.001", "a/b", "spaces here", "üñî", ""}
	seen := map[string]string{}
	for _, tenant := range []string{"acme", "globex"} {
		for _, id := range ids {
			got := runtime.RunID(tenant, id)
			if err := runtime.CheckRun("docs-bot", got); err != nil {
				t.Errorf("RunID(%q, %q) is %q, which is not a usable topic segment: %v", tenant, id, got, err)
			}
			if runtime.RunID(tenant, id) != got {
				t.Errorf("RunID(%q, %q) is not deterministic", tenant, id)
			}
			if other, clash := seen[got]; clash {
				t.Errorf("RunID(%q, %q) and RunID(%s) both produced %q", tenant, id, other, got)
			}
			seen[got] = fmt.Sprintf("%q, %q", tenant, id)
		}
	}
}

// The tenant and the tag id are separate fields, not one string. Moving a
// character from one to the other must change the run, or a tenant could pick a
// tag id that lands on a neighbour's.
func TestRunIDKeepsTheTenantAndTheTagIDApart(t *testing.T) {
	t.Parallel()

	pairs := [][2][2]string{
		{{"a_b", "c"}, {"a", "b_c"}},
		{{"ab", "c"}, {"a", "bc"}},
		{{"acme", ""}, {"", "acme"}},
		{{"acme-globex", "Ev1"}, {"acme", "globex-Ev1"}},
	}
	for _, p := range pairs {
		if a, b := runtime.RunID(p[0][0], p[0][1]), runtime.RunID(p[1][0], p[1][1]); a == b {
			t.Errorf("RunID(%q, %q) and RunID(%q, %q) are both %q", p[0][0], p[0][1], p[1][0], p[1][1], a)
		}
	}
}

// Two tenants are free to raise tags with the same id: a tag id is whatever
// the caller sent. Each gets its own run, on its own topic, carrying its own
// tenant and its own request. Neither joins the other's, and neither learns
// from its acceptance that the other exists.
func TestTheSameTagIDInTwoTenantsIsTwoRuns(t *testing.T) {
	t.Parallel()

	h := newHarness(t, newExecutor("ok"))
	h.specs.revise("docs-bot", "v1")

	first := tag("docs-bot", "Ev-shared", "acme's question")
	second := tag("docs-bot", "Ev-shared", "globex's question")
	second.Tenant = "globex"

	a, err := accept(context.Background(), h.rt, first)
	if err != nil {
		t.Fatalf("accept for acme: %v", err)
	}
	b, err := accept(context.Background(), h.rt, second)
	if err != nil {
		t.Fatalf("accept for globex: %v", err)
	}
	if a.RunID == b.RunID {
		t.Fatalf("both tenants were given run %q", a.RunID)
	}
	if a.Tenant == b.Tenant {
		t.Fatalf("both tenants publish under tenant %q", a.Tenant)
	}

	for _, want := range []struct {
		runID  string
		tenant string
		text   string
	}{{a.RunID, "acme", "acme's question"}, {b.RunID, "globex", "globex's question"}} {
		rec, err := h.ledger.GetRun(context.Background(), want.runID)
		if err != nil {
			t.Fatalf("run %s: %v", want.runID, err)
		}
		var in runtime.Input
		if err := json.Unmarshal(rec.Input, &in); err != nil {
			t.Fatalf("decode run %s: %v", want.runID, err)
		}
		req, _ := event.RequestOf(in.Trigger.Data)
		if in.Trigger.Tenant != want.tenant || req.Text != want.text {
			t.Errorf("run %s holds tenant %q text %q, want %q %q", want.runID, in.Trigger.Tenant, req.Text, want.tenant, want.text)
		}
	}
	for _, e := range h.bus.all() {
		switch e.Run {
		case a.RunID:
			if e.Tenant != "acme" {
				t.Errorf("acme's run published an event for tenant %q", e.Tenant)
			}
		case b.RunID:
			if e.Tenant != "globex" {
				t.Errorf("globex's run published an event for tenant %q", e.Tenant)
			}
		default:
			t.Errorf("an event was published for unknown run %q", e.Run)
		}
	}

	// A redelivery within a tenant still joins that tenant's run.
	again, err := accept(context.Background(), h.rt, first)
	if err != nil || again.RunID != a.RunID {
		t.Errorf("acme's redelivery got run %q, %v; want %q", again.RunID, err, a.RunID)
	}
}

// The lifecycle a subscriber sees on a healthy run, in order, with the answer on
// the completed event so a sink that wants only the outcome needs nothing else.
func TestLifecycleOrderOnSuccess(t *testing.T) {
	t.Parallel()

	h := newHarness(t, newExecutor("the docs are updated", event.Text{}))
	h.exec.chunks = nil // replaced below, once t is available for encoding
	h.exec.chunks = []event.Body{textChunk(t, "the docs "), textChunk(t, "are updated")}
	h.specs.revise("docs-bot", "you document things")
	h.work(t)

	ctx := context.Background()
	acc, err := accept(ctx, h.rt, tag("docs-bot", "Ev1", "update the docs"))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	out, err := h.rt.Result(ctx, acc.RunID)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if out.Text != "the docs are updated" {
		t.Errorf("run output is %q, want the agent's answer", out.Text)
	}

	want := []string{
		event.TypeAccepted,
		event.TypeStarted,
		event.TypeText,
		event.TypeText,
		event.TypeCompleted,
	}
	if got := h.bus.kinds(); !sameKinds(got, want) {
		t.Fatalf("lifecycle is %v, want %v", got, want)
	}

	events := h.bus.all()
	for i := 1; i < len(events); i++ {
		if events[i].Seq <= events[i-1].Seq {
			t.Fatalf("sequences are not increasing: %d then %d", events[i-1].Seq, events[i].Seq)
		}
	}
	if events[0].Seq != runtime.SeqAccepted {
		t.Errorf("accepted is seq %d, want %d", events[0].Seq, runtime.SeqAccepted)
	}
	last := events[len(events)-1]
	if last.Seq != out.LastSeq {
		t.Errorf("Output.LastSeq is %d, want the completed event's seq %d", out.LastSeq, last.Seq)
	}
	body, err := decode[lifecycle](last)
	if err != nil {
		t.Fatalf("decode completed: %v", err)
	}
	if body.Text != "the docs are updated" {
		t.Errorf("completed event carries %q, want the answer", body.Text)
	}
	if body.Attempt != 1 {
		t.Errorf("completed event reports attempt %d, want 1", body.Attempt)
	}
}

// Every event of a run carries the run's identity, because a subscriber filters
// on those fields instead of on a topic per revision or origin.
func TestEveryEventCarriesRunIdentity(t *testing.T) {
	t.Parallel()

	h := newHarness(t, newExecutor("done"))
	h.exec.chunks = []event.Body{textChunk(t, "done")}
	h.specs.revise("docs-bot", "v1")
	rev := h.specs.revise("docs-bot", "v2")
	h.work(t)

	ctx := context.Background()
	acc, err := accept(ctx, h.rt, tag("docs-bot", "Ev2", "hello"))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if _, err := h.rt.Result(ctx, acc.RunID); err != nil {
		t.Fatalf("Result: %v", err)
	}

	if acc.Channel == "" || !strings.HasPrefix(acc.Channel, "github://") {
		t.Fatalf("accepted on channel %q, want the trigger's github channel", acc.Channel)
	}
	for _, e := range h.bus.all() {
		if e.Channel != acc.Channel {
			t.Errorf("%s published on %s, want the trigger's channel %s", e.Type, e.Channel, acc.Channel)
		}
		if e.Run != acc.RunID {
			t.Errorf("%s carries run %q, want %q", e.Type, e.Run, acc.RunID)
		}
		if e.Tenant != "acme" {
			t.Errorf("%s carries tenant %q, want acme", e.Type, e.Tenant)
		}
		if e.Agent != "docs-bot" {
			t.Errorf("%s carries agent %q, want docs-bot", e.Type, e.Agent)
		}
		if e.Version != rev.Version || e.Digest != rev.Digest {
			t.Errorf("%s carries version %q digest %q, want the pinned %s %q", e.Type, e.Version, e.Digest, rev.Version, rev.Digest)
		}
		if e.Time.IsZero() {
			t.Errorf("%s has no timestamp", e.Type)
		}
	}
}

// THE property this package exists to protect: a run pins its revision at accept
// time and executes that revision forever, even after the agent has been revised
// past it. The assertion is on the definition actually handed to the executor,
// not merely on a number.
func TestARunPinnedToARevisionStillExecutesItAfterARevise(t *testing.T) {
	t.Parallel()

	h := newHarness(t, newExecutor("answered"))
	h.exec.chunks = []event.Body{textChunk(t, "answered")}
	for i := 1; i <= 5; i++ {
		h.specs.revise("docs-bot", fmt.Sprintf("revision %d instructions", i))
	}
	pinned := h.specs.revise("docs-bot", "revision 6 instructions")
	if pinned.Version != "6.0.0" {
		t.Fatalf("fixture pinned version %s, want 6.0.0", pinned.Version)
	}

	ctx := context.Background()
	acc, err := accept(ctx, h.rt, tag("docs-bot", "Ev6", "hello"))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if acc.Version != "6.0.0" {
		t.Fatalf("accepted at version %s, want 6.0.0", acc.Version)
	}

	// The agent is revised while the run sits in the queue, which is exactly
	// the race a real deployment has: a tag arrives, someone ships a prompt
	// change, and the worker picks the run up afterwards.
	seven := h.specs.revise("docs-bot", "revision 7 instructions")
	if seven.Version != "7.0.0" {
		t.Fatalf("fixture revised to %s, want 7.0.0", seven.Version)
	}

	h.work(t)
	if _, err := h.rt.Result(ctx, acc.RunID); err != nil {
		t.Fatalf("Result: %v", err)
	}

	seen := h.exec.seen()
	if len(seen) != 1 {
		t.Fatalf("the executor ran %d times, want 1", len(seen))
	}
	req := seen[0]
	if req.Version() != "6.0.0" {
		t.Errorf("the turn executed version %s, want the pinned 6.0.0", req.Version())
	}
	if got := req.Definition.Spec.SystemPrompt; got != "revision 6 instructions" {
		t.Errorf("the turn executed the prompt %q, want revision 6's", got)
	}
	if got := req.Definition.Digest; got != pinned.Digest {
		t.Errorf("the turn executed digest %q, want the pinned %q", got, pinned.Digest)
	}
	for _, e := range h.bus.all() {
		if e.Version != "6.0.0" {
			t.Errorf("%s reports version %q, want the pinned 6", e.Type, e.Version)
		}
	}
}

// A redelivery joins the existing run rather than starting a second one, and
// cannot re-pin it: the revision a run executes is whatever was current the first
// time the tag arrived.
func TestARedeliveredTagJoinsTheRunItDuplicated(t *testing.T) {
	t.Parallel()

	h := newHarness(t, newExecutor("answered"))
	h.exec.chunks = []event.Body{textChunk(t, "answered")}
	h.specs.revise("docs-bot", "revision 1 instructions")

	ctx := context.Background()
	first, err := accept(ctx, h.rt, tag("docs-bot", "Ev-dup", "hello"))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	h.specs.revise("docs-bot", "revision 2 instructions")

	second, err := accept(ctx, h.rt, tag("docs-bot", "Ev-dup", "hello"))
	if err != nil {
		t.Fatalf("Accept (redelivery): %v", err)
	}
	if second.RunID != first.RunID {
		t.Fatalf("the redelivery started run %q, want it to join %q", second.RunID, first.RunID)
	}

	h.work(t)
	if _, err := h.rt.Result(ctx, first.RunID); err != nil {
		t.Fatalf("Result: %v", err)
	}
	if n := h.exec.count(); n != 1 {
		t.Fatalf("the turn executed %d times, want once", n)
	}
	if v := h.exec.seen()[0].Version(); v != "1.0.0" {
		t.Fatalf("the run executed version %s, want the version pinned by the first delivery", v)
	}
	// Both deliveries announce, and the redelivery resolves the revision that
	// is current when IT arrives — it cannot know the run already exists,
	// because the announcement has to precede the submission that would tell
	// it. What collapses the two into one is the sequence: both land on
	// SeqAccepted, so the broker's first-write-wins rule keeps the first and
	// discards the redelivery's, revision and all. This recorder is a spy on
	// Publish rather than a broker, so it sees both.
	accepted := h.bus.ofKind(event.TypeAccepted)
	if len(accepted) != 2 {
		t.Fatalf("got %d accepted events, want 2, one per delivery", len(accepted))
	}
	for _, e := range accepted {
		if e.Seq != runtime.SeqAccepted {
			t.Errorf("an accepted event landed at seq %d, want %d: reusing the sequence is the only thing that lets the broker collapse the redelivery", e.Seq, runtime.SeqAccepted)
		}
	}
	if accepted[0].Version != "1.0.0" {
		t.Errorf("the accepted event the broker keeps reports version %q; the run pinned 1.0.0", accepted[0].Version)
	}
	// Everything the run itself produced — which is everything a subscriber
	// will actually see, since the redelivery's announcement never survives —
	// reports the pinned revision.
	for _, e := range h.bus.all() {
		if e.Type == event.TypeAccepted {
			continue
		}
		if e.Version != "1.0.0" {
			t.Errorf("%s reports version %q; the run pinned 1.0.0", e.Type, e.Version)
		}
	}
}

// A failure has to reach the bus with enough information to tell a sink whether
// to give up: the attempt, and whether anything will try again.
func TestLifecycleOrderOnFailure(t *testing.T) {
	t.Parallel()

	exec := newExecutor("")
	exec.err = durable.NonRetryable(errors.New("the model refused"))
	h := newHarness(t, exec)
	h.exec.chunks = []event.Body{textChunk(t, "partial ")}
	h.specs.revise("docs-bot", "v1")
	h.work(t)

	ctx := context.Background()
	acc, err := accept(ctx, h.rt, tag("docs-bot", "Ev-fail", "hello"))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if _, err := h.rt.Result(ctx, acc.RunID); err == nil {
		t.Fatal("the run succeeded, want a terminal failure")
	}

	want := []string{
		event.TypeAccepted,
		event.TypeStarted,
		event.TypeText,
		event.TypeFailed,
	}
	if got := h.bus.kinds(); !sameKinds(got, want) {
		t.Fatalf("lifecycle is %v, want %v", got, want)
	}
	failed := h.bus.ofKind(event.TypeFailed)
	body, err := decode[lifecycle](failed[0])
	if err != nil {
		t.Fatalf("decode failed event: %v", err)
	}
	if !body.Terminal {
		t.Error("a non-retryable failure was reported as non-terminal")
	}
	if !strings.Contains(body.Error, "the model refused") {
		t.Errorf("failure reports %q, want the underlying reason", body.Error)
	}
	if body.Attempt != 1 {
		t.Errorf("failure reports attempt %d, want 1", body.Attempt)
	}
	// The partial answer stays on the stream: a sink has already rendered it,
	// and retracting it would make the thread disagree with the run.
	if len(h.bus.ofKind(event.TypeText)) != 1 {
		t.Errorf("the partial output was not published")
	}
}

// A retryable failure is NOT terminal, because the retry budget belongs to
// durable. Reporting it as terminal would tell a sink to give up on a run that
// is about to succeed.
func TestARetryableFailureIsReportedAsNonTerminalAndTheRunResumes(t *testing.T) {
	t.Parallel()

	exec := newExecutor("second time lucky")
	// Exactly one attempt fails; the retry succeeds on its own. Healing the
	// executor from the test instead would race the retry backoff.
	exec.failNext(1, errors.New("provider timed out"))
	h := newHarness(t, exec)
	h.specs.revise("docs-bot", "v1")
	h.work(t)

	ctx := context.Background()
	acc, err := accept(ctx, h.rt, tag("docs-bot", "Ev-retry", "hello"))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}

	out, err := h.rt.Result(ctx, acc.RunID)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if out.Text != "second time lucky" {
		t.Errorf("run output is %q, want the second attempt's answer", out.Text)
	}

	failed := h.bus.ofKind(event.TypeFailed)
	body, err := decode[lifecycle](failed[0])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Terminal {
		t.Error("a retryable failure was reported as terminal")
	}
	// The second attempt announces itself as a resumption, and everything it
	// publishes sits above everything the first attempt published.
	resumed := h.bos(event.TypeResumed)
	if len(resumed) != 1 {
		t.Fatalf("got %d resumed events, want 1", len(resumed))
	}
	if resumed[0].Seq <= failed[0].Seq {
		t.Errorf("the resumed event is seq %d, at or below the failed attempt's %d", resumed[0].Seq, failed[0].Seq)
	}
	completed := h.bus.ofKind(event.TypeCompleted)
	if len(completed) != 1 {
		t.Fatalf("got %d completed events, want 1", len(completed))
	}
	if completed[0].Seq <= resumed[0].Seq {
		t.Errorf("completed is seq %d, at or below resumed at %d", completed[0].Seq, resumed[0].Seq)
	}
	if n := h.exec.count(); n != 2 {
		t.Errorf("the executor ran %d times, want 2 (a retry re-executes)", n)
	}
}

// bos is ofKind under a shorter name, for the assertions that read better in one
// line.
func (h *harness) bos(k string) []event.Envelope { return h.bus.ofKind(k) }

// Replay must not pay for the model twice: the agent's activity is memoized, so a
// re-execution of the same attempt publishes the identical sequence — which the
// bus deduplicates — and never re-runs the turn.
func TestReplayingAnAttemptRepublishesTheIdenticalSequence(t *testing.T) {
	t.Parallel()

	h := newHarness(t, newExecutor("answered"))
	h.exec.chunks = []event.Body{textChunk(t, "ans"), textChunk(t, "wered")}
	h.specs.revise("docs-bot", "v1")

	ctx := context.Background()
	acc, err := accept(ctx, h.rt, tag("docs-bot", "Ev-replay", "hello"))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	body, _, err := h.ledger.Load(ctx, acc.RunID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	fn, ok := h.rt.Registry().Lookup(runtime.ActivityTurn)
	if !ok {
		t.Fatalf("activity %q is not registered", runtime.ActivityTurn)
	}

	// Attempt 1, executing for real. This is what the worker does on a claim.
	execute := func(attempt int) {
		t.Helper()
		_, records, err := h.ledger.Load(ctx, acc.RunID)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		frame := replay.NewFrame(acc.RunID, attempt, h.ledger, drun.JSONCodec{}, records, nil)
		if _, err := fn(replay.WithFrame(ctx, frame), body.Input); err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
	}
	execute(1)
	first := fingerprint(h.bus.all())
	if n := h.exec.count(); n != 1 {
		t.Fatalf("the executor ran %d times on the first attempt, want 1", n)
	}

	// The same attempt, replayed from the ledger.
	execute(1)
	if n := h.exec.count(); n != 1 {
		t.Fatalf("the executor ran %d times after a replay, want 1: the memoized turn must not re-execute", n)
	}
	replayed := h.bus.all()[len(first):]

	// The turn's own lifecycle is published again, because the turn body
	// re-executes; the agent's deltas are not, because the agent is a nested
	// activity whose result is memoized and whose emits therefore never happen
	// a second time. That asymmetry is the point of the split.
	if got := kindsOf(replayed); !sameKinds(got, []string{event.TypeStarted, event.TypeCompleted}) {
		t.Errorf("the replay published %v, want the turn's lifecycle only", got)
	}
	// And the replay is invisible to a subscriber: every event it publishes
	// reuses a sequence the first execution already used, carrying the same
	// kind and the same body, so the broker's first-write-wins rule discards
	// all of it. That is what makes replay safe without a dedupe table.
	seen := make(map[string]bool, len(first))
	for _, f := range first {
		seen[f] = true
	}
	for _, f := range fingerprint(replayed) {
		if !seen[f] {
			t.Errorf("the replay published %s, which the first execution never did: the broker cannot collapse it", f)
		}
	}

	// A later attempt keeps the memo but moves into its own band, so the
	// events it re-publishes cannot be mistaken for the earlier ones — and the
	// agent still does not re-run.
	before := len(first) + len(replayed)
	execute(2)
	if n := h.exec.count(); n != 1 {
		t.Fatalf("the executor ran %d times on a later attempt, want 1", n)
	}
	third := h.bus.all()[before:]
	for _, e := range third {
		if e.Type == event.TypeText {
			t.Errorf("a memoized replay re-published the agent's output: seq %d", e.Seq)
		}
	}
	highest := uint64(0)
	for _, e := range h.bus.all()[:before] {
		if e.Seq > highest {
			highest = e.Seq
		}
	}
	for _, e := range third {
		if e.Seq <= highest {
			t.Errorf("attempt 2 published %s at seq %d, at or below attempt 1's high-water mark %d", e.Type, e.Seq, highest)
		}
	}
}

// A run that parks is waiting, not failing: it must say so, and it must not be
// reported as terminal.
func TestAParkedRunPublishesParked(t *testing.T) {
	t.Parallel()

	exec := newExecutor("")
	exec.err = drun.ErrParked
	h := newHarness(t, exec)
	h.specs.revise("docs-bot", "v1")

	ctx := context.Background()
	acc, err := accept(ctx, h.rt, tag("docs-bot", "Ev-park", "hello"))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	h.work(t)
	waitFor(t, func() bool { return len(h.bus.ofKind(event.TypeParked)) == 1 })

	if got := len(h.bus.ofKind(event.TypeFailed)); got != 0 {
		t.Errorf("a parked run published %d failed events, want none", got)
	}
	parked := h.bus.ofKind(event.TypeParked)[0]
	body, err := decode[lifecycle](parked)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Reason == "" {
		t.Error("the parked event carries no reason")
	}
	r, err := h.ledger.GetRun(ctx, acc.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if r.Status.Terminal() {
		t.Errorf("the run is %s, want it still pending: parking is waiting", r.Status)
	}
}

// A version is immutable by contract. One whose content moved under a pinned
// version is corruption, and executing it would silently run a definition
// the run never agreed to.
func TestAPinnedRevisionWhoseHashMovedFailsTerminally(t *testing.T) {
	t.Parallel()

	h := newHarness(t, newExecutor("answered"))
	h.specs.revise("docs-bot", "v1")

	ctx := context.Background()
	acc, err := accept(ctx, h.rt, tag("docs-bot", "Ev-hash", "hello"))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	h.specs.corrupt("docs-bot", 1, "sha256:something-else")
	h.work(t)

	if _, err := h.rt.Result(ctx, acc.RunID); err == nil {
		t.Fatal("the run succeeded against a changed revision")
	}
	if n := h.exec.count(); n != 0 {
		t.Errorf("the executor ran %d times, want 0: the mismatch must be caught before the model", n)
	}
	failed := h.bus.ofKind(event.TypeFailed)
	if len(failed) == 0 {
		t.Fatal("no failure was published")
	}
	body, err := decode[lifecycle](failed[0])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Terminal {
		t.Error("a changed revision was reported as retryable")
	}
	if !strings.Contains(body.Error, "pin does not hold") || !strings.Contains(body.Error, "content changed under its version") {
		t.Errorf("failure reports %q, want it to name the mismatch", body.Error)
	}
}

// A tag naming an agent that does not exist is refused synchronously, rather than
// accepted and then failed: the caller is still on the phone.
func TestAnUnknownAgentIsRefusedAtAcceptTime(t *testing.T) {
	t.Parallel()

	h := newHarness(t, newExecutor(""))
	if _, err := accept(context.Background(), h.rt, tag("nobody", "Ev-x", "hello")); err == nil {
		t.Fatal("Accept succeeded for an unknown agent")
	}
	if got := len(h.bus.all()); got != 0 {
		t.Errorf("%d events were published for a refused tag, want none", got)
	}
	if got, err := h.ledger.GetRun(context.Background(), runtime.RunKey("acme", "Ev-x", "nobody")); err == nil {
		t.Errorf("a run %+v was accepted for an unknown agent", got)
	}
}

// A malformed trigger never becomes a run.
func TestAMalformedTriggerIsRefused(t *testing.T) {
	t.Parallel()

	h := newHarness(t, newExecutor(""))
	h.specs.revise("docs-bot", "v1")
	tests := []struct {
		name string
		tag  fixture
	}{
		{"no id", fixture{Tenant: "acme", Agent: "docs-bot", Origin: "github", At: time.Now()}},
		{"no agent", fixture{ID: "Ev1", Tenant: "acme", Origin: "github", At: time.Now()}},
		{"no time", fixture{ID: "Ev1", Tenant: "acme", Agent: "docs-bot", Origin: "github"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := accept(context.Background(), h.rt, tc.tag); !errors.Is(err, runtime.ErrInvalid) {
				t.Fatalf("Accept returned %v, want a runtime.ErrInvalid", err)
			}
		})
	}
}

// The journal is the run's own durable record of its stream, and it has to give
// events back: an envelope as it was published, and an entry written before
// the event union read through the legacy table.
func TestJournalEntriesRoundTripBackIntoEvents(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	want := event.Envelope{
		ID: "run_1:1048577", Source: "fiber://run/run_1", Type: event.TypeText,
		Channel: "github://acme/fiber", Time: at, Hops: 1, Tenant: "acme",
		Run: "run_1", Seq: 1048577, Agent: "docs-bot", Version: "6",
		Data: event.Text{Text: "hi"},
	}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	old := []byte(`{"seq":1048577,"topic":"agent:docs-bot:run_1","run_id":"run_1","tenant":"acme",` +
		`"agent":"docs-bot","rev":6,"origin":"github","kind":"delta.text","payload":{"text":"hi"},"at":"2026-07-26T12:00:00Z"}`)
	for name, entry := range map[string][]byte{"envelope": encoded, "before the union": old} {
		got, err := runtime.DecodeEntry(entry)
		if err != nil {
			t.Fatalf("%s: DecodeEntry: %v", name, err)
		}
		body, ok := got.Data.(event.Text)
		if !ok || body.Text != "hi" {
			t.Errorf("%s: body is %#v, want the text", name, got.Data)
		}
		if got.Type != want.Type || got.Seq != want.Seq || got.Run != want.Run || got.Tenant != want.Tenant ||
			got.Agent != want.Agent || got.Version != want.Version || !got.Time.Equal(want.Time) {
			t.Errorf("%s: event is %+v, want %+v", name, got, want)
		}
	}
}

// A failure to publish is a failure of the turn: a run whose events nobody can
// see has not done its job, and the attempt must be retried rather than quietly
// completed.
func TestAPublishFailureFailsTheAttempt(t *testing.T) {
	t.Parallel()

	h := newHarness(t, newExecutor("answered"))
	h.specs.revise("docs-bot", "v1")
	var once sync.Once
	h.bus.failWhen(func(e event.Envelope) error {
		var err error
		if e.Type == event.TypeCompleted {
			once.Do(func() { err = errors.New("broker unreachable") })
		}
		return err
	})
	h.work(t)

	ctx := context.Background()
	acc, err := accept(ctx, h.rt, tag("docs-bot", "Ev-pub", "hello"))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if _, err := h.rt.Result(ctx, acc.RunID); err != nil {
		t.Fatalf("Result: %v", err)
	}
	// The attempt that could not announce completion was retried; the agent
	// itself was memoized and must not have run twice.
	if n := h.exec.count(); n != 1 {
		t.Errorf("the executor ran %d times, want 1: the model must not be paid for twice", n)
	}
	if got := len(h.bus.ofKind(event.TypeCompleted)); got != 1 {
		t.Errorf("%d completed events landed, want 1", got)
	}
}

// ── helpers ─────────────────────────────────────────────────────────

func sameKinds(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func fingerprint(events []event.Envelope) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, fmt.Sprintf("%d:%s:%s", e.Seq, e.Type, raw(e)))
	}
	return out
}

func kindsOf(events []event.Envelope) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.Type)
	}
	return out
}

// waitFor polls cond until it holds or the test times out. The runtime hands work
// to a worker goroutine, so a test that asserts on an intermediate state has to
// wait for it rather than assume it.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the run to reach the expected state")
		}
		time.Sleep(time.Millisecond)
	}
}

// A run key is "r" and 32 hex digits, stable for one (tenant, event,
// subscriber), different for every other subscriber, and never a RunID.
func TestRunKeyIsDomainSeparatedPerSubscriber(t *testing.T) {
	a := runtime.RunKey("acme", "Ev-1", "docs-bot")
	if len(a) != 33 || a[0] != 'r' || strings.Trim(a[1:], "0123456789abcdef") != "" {
		t.Fatalf("RunKey = %q; want r and 32 hex digits", a)
	}
	if runtime.RunKey("acme", "Ev-1", "docs-bot") != a {
		t.Fatal("RunKey is not deterministic")
	}
	for _, other := range []string{
		runtime.RunKey("acme", "Ev-1", "reviewer"),
		runtime.RunKey("globex", "Ev-1", "docs-bot"),
		runtime.RunKey("acme", "Ev-1d", "ocs-bot"),
		runtime.RunID("acme", "Ev-1"),
	} {
		if other == a {
			t.Fatalf("%q collides with RunKey(acme, Ev-1, docs-bot)", other)
		}
	}
}
