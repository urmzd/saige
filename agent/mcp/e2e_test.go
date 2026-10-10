package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/urmzd/saige/agent/types"
)

var objectSchema = json.RawMessage(`{"type":"object"}`)

func boolPtr(b bool) *bool { return &b }

func textResult(s string) *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: s}}}
}

// testServer is an in-process MCP server behind a real streamable HTTP
// endpoint, so tests drive the real Connect path.
type testServer struct {
	t       *testing.T
	server  *mcpsdk.Server
	http    *httptest.Server
	handler atomic.Pointer[http.Handler]
	calls   sync.Map // tool name -> *atomic.Int64
	// done releases hung handlers before the HTTP server closes. The SDK
	// sends notifications/cancelled in the background, so a session closed
	// right after a timeout may never deliver it.
	done chan struct{}
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	ts := &testServer{t: t, done: make(chan struct{})}
	ts.server = mcpsdk.NewServer(&mcpsdk.Implementation{Name: "test", Version: "1"}, nil)
	ts.resetSessions()
	ts.http = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		(*ts.handler.Load()).ServeHTTP(w, r)
	}))
	t.Cleanup(ts.http.Close)
	t.Cleanup(func() { close(ts.done) })

	ts.add(&mcpsdk.Tool{Name: "echo", Description: "echo text", InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
		Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: true}},
		func(_ context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			var args map[string]any
			_ = json.Unmarshal(req.Params.Arguments, &args)
			s, _ := args["text"].(string)
			return textResult(s), nil
		})
	ts.add(&mcpsdk.Tool{Name: "tags", InputSchema: json.RawMessage(`{"type":"object","properties":{"tags":{"type":"array","items":{"type":"string"}}}}`)},
		func(_ context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			return textResult(string(req.Params.Arguments)), nil
		})
	ts.add(&mcpsdk.Tool{Name: "media", InputSchema: objectSchema},
		func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{
				&mcpsdk.TextContent{Text: "here"},
				&mcpsdk.ImageContent{MIMEType: "image/png", Data: []byte{1, 2, 3}},
				&mcpsdk.AudioContent{MIMEType: "audio/wav", Data: []byte{4, 5}},
				&mcpsdk.ResourceLink{URI: "https://example.com/doc", Name: "doc"},
				&mcpsdk.EmbeddedResource{Resource: &mcpsdk.ResourceContents{URI: "file:///notes.txt", Text: "notes"}},
			}}, nil
		})
	ts.add(&mcpsdk.Tool{Name: "structured", InputSchema: objectSchema},
		func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			return &mcpsdk.CallToolResult{StructuredContent: map[string]any{"n": 1}}, nil
		})
	ts.add(&mcpsdk.Tool{Name: "fails", InputSchema: objectSchema},
		func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			return &mcpsdk.CallToolResult{IsError: true, Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "bad input"}}}, nil
		})
	ts.add(&mcpsdk.Tool{Name: "hang", InputSchema: objectSchema},
		func(ctx context.Context, _ *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-ts.done:
				return nil, errors.New("test finished")
			}
		})
	ts.add(&mcpsdk.Tool{Name: "big", InputSchema: objectSchema},
		func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			return textResult(strings.Repeat("a", 10<<20)), nil
		})
	ts.add(&mcpsdk.Tool{Name: "wipe", InputSchema: objectSchema, Annotations: &mcpsdk.ToolAnnotations{DestructiveHint: boolPtr(true)}},
		func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			return textResult("wiped"), nil
		})
	ts.add(&mcpsdk.Tool{Name: "slow", InputSchema: objectSchema, Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			for i := 1; i <= 2; i++ {
				_ = req.Session.NotifyProgress(ctx, &mcpsdk.ProgressNotificationParams{
					ProgressToken: req.Params.GetProgressToken(), Progress: float64(i), Total: 2, Message: "step",
				})
			}
			time.Sleep(20 * time.Millisecond)
			return textResult("done"), nil
		})

	ts.server.AddResource(&mcpsdk.Resource{URI: "file:///readme.md", Name: "readme", MIMEType: "text/markdown"},
		func(_ context.Context, req *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
			return &mcpsdk.ReadResourceResult{Contents: []*mcpsdk.ResourceContents{{URI: req.Params.URI, MIMEType: "text/markdown", Text: "# readme"}}}, nil
		})
	ts.server.AddPrompt(&mcpsdk.Prompt{Name: "greet", Arguments: []*mcpsdk.PromptArgument{{Name: "who", Required: true}}},
		func(_ context.Context, req *mcpsdk.GetPromptRequest) (*mcpsdk.GetPromptResult, error) {
			return &mcpsdk.GetPromptResult{Messages: []*mcpsdk.PromptMessage{{Role: "user", Content: &mcpsdk.TextContent{Text: "hello " + req.Params.Arguments["who"]}}}}, nil
		})
	return ts
}

// add registers a tool and counts its calls.
func (ts *testServer) add(tool *mcpsdk.Tool, h mcpsdk.ToolHandler) {
	n := &atomic.Int64{}
	ts.calls.Store(tool.Name, n)
	ts.server.AddTool(tool, func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		n.Add(1)
		return h(ctx, req)
	})
}

func (ts *testServer) count(tool string) int64 {
	v, _ := ts.calls.Load(tool)
	return v.(*atomic.Int64).Load()
}

// resetSessions swaps in a fresh handler, so every existing session ID is
// unknown to the server: what a client sees after a server restart.
func (ts *testServer) resetSessions() {
	var h http.Handler = mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return ts.server }, nil)
	ts.handler.Store(&h)
}

func (ts *testServer) spec() ServerSpec {
	s := Remote("t", ts.http.URL)
	s.CallTimeout = 5 * time.Second
	return s
}

func connect(t *testing.T, spec ServerSpec) *Client {
	t.Helper()
	c, err := Connect(context.Background(), spec)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func toolNamed(t *testing.T, c *Client, name string) types.RichTool {
	t.Helper()
	tools, err := c.Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		if tool.Definition().Name == name {
			return tool.(types.RichTool)
		}
	}
	t.Fatalf("tool %s not listed", name)
	return nil
}

func TestEndToEndCallsConvertEveryContentKind(t *testing.T) {
	ts := newTestServer(t)
	c := connect(t, ts.spec())
	ctx := context.Background()

	tests := []struct {
		tool      string
		check     func(t *testing.T, r types.ToolResult)
		wantError bool
	}{
		{tool: "t_echo", check: func(t *testing.T, r types.ToolResult) {
			if r.Text != "hi" {
				t.Errorf("text = %q", r.Text)
			}
		}},
		{tool: "t_media", check: func(t *testing.T, r types.ToolResult) {
			kinds := map[types.ToolResultBlockKind]int{}
			for _, b := range r.Blocks {
				kinds[b.Kind]++
			}
			if kinds[types.ToolResultBlockImage] != 1 || kinds[types.ToolResultBlockFile] != 1 || kinds[types.ToolResultBlockText] != 2 {
				t.Errorf("blocks = %v", kinds)
			}
			if len(r.Citations) != 2 || r.Citations[0].URI != "https://example.com/doc" || r.Citations[0].Producer != "t_media" {
				t.Errorf("citations = %+v", r.Citations)
			}
		}},
		{tool: "t_structured", check: func(t *testing.T, r types.ToolResult) {
			if r.Text != `{"n":1}` || len(r.Blocks) != 1 || r.Blocks[0].Kind != types.ToolResultBlockJSON {
				t.Errorf("result = %+v", r)
			}
		}},
		{tool: "t_fails", wantError: true, check: func(t *testing.T, r types.ToolResult) {
			if r.Text != "bad input" {
				t.Errorf("text = %q", r.Text)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.tool, func(t *testing.T) {
			res, err := toolNamed(t, c, tt.tool).ExecuteRich(ctx, map[string]any{"text": "hi"})
			if err != nil {
				t.Fatalf("a tool failure must be a result, not a Go error: %v", err)
			}
			if res.IsError != tt.wantError {
				t.Errorf("IsError = %v, want %v (%s)", res.IsError, tt.wantError, res.Text)
			}
			tt.check(t, res)
		})
	}
}

func TestCallTimeoutBecomesAnErrorResult(t *testing.T) {
	ts := newTestServer(t)
	spec := ts.spec()
	spec.CallTimeout = 100 * time.Millisecond
	c := connect(t, spec)

	start := time.Now()
	res, err := toolNamed(t, c, "t_hang").ExecuteRich(context.Background(), nil)
	if err != nil || !res.IsError {
		t.Fatalf("res=%+v err=%v, want an error result", res, err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("timeout not enforced: took %v", time.Since(start))
	}
}

func TestCloseDuringCallReturnsClosedError(t *testing.T) {
	old := closeTimeout
	closeTimeout = 500 * time.Millisecond
	t.Cleanup(func() { closeTimeout = old })
	ts := newTestServer(t)
	spec := ts.spec()
	spec.CallTimeout = 2 * time.Second
	c := connect(t, spec)
	hang := toolNamed(t, c, "t_hang")
	echo := toolNamed(t, c, "t_echo")

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		res, err := hang.ExecuteRich(context.Background(), nil)
		if err != nil || !res.IsError {
			t.Errorf("in-flight call: res=%+v err=%v", res, err)
		}
	}()
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	_ = c.Close() // may report that the hung server kept its session
	if time.Since(start) > closeTimeout+time.Second {
		t.Errorf("Close took %v; it must be bounded", time.Since(start))
	}
	wg.Wait()

	res, err := echo.ExecuteRich(context.Background(), map[string]any{"text": "x"})
	if err != nil || !res.IsError || !strings.Contains(res.Text, "is closed") {
		t.Errorf("call after Close: res=%+v err=%v", res, err)
	}
}

func TestResultLimits(t *testing.T) {
	ts := newTestServer(t)
	tests := []struct {
		name  string
		spec  func(ServerSpec) ServerSpec
		tool  string
		check func(t *testing.T, r types.ToolResult)
	}{
		{"default text cap", func(s ServerSpec) ServerSpec { return s }, "t_big", func(t *testing.T, r types.ToolResult) {
			if len(r.Text) > DefaultMaxResultBytes+64 || !strings.HasSuffix(r.Text, "[truncated 10223616 bytes]") {
				t.Errorf("len=%d suffix=%q", len(r.Text), r.Text[len(r.Text)-40:])
			}
		}},
		{"unlimited", func(s ServerSpec) ServerSpec { s.MaxResultBytes = -1; return s }, "t_big", func(t *testing.T, r types.ToolResult) {
			if len(r.Text) != 10<<20 {
				t.Errorf("len=%d, want the full payload", len(r.Text))
			}
		}},
		{"binary over cap becomes a note", func(s ServerSpec) ServerSpec { s.MaxBinaryBytes = 2; return s }, "t_media", func(t *testing.T, r types.ToolResult) {
			if !strings.Contains(r.Text, "[dropped image: image/png, 3 bytes]") {
				t.Errorf("text = %q", r.Text)
			}
			noted := false
			for _, b := range r.Blocks {
				if b.Kind == types.ToolResultBlockImage {
					t.Error("oversized image kept")
				}
				if b.Kind == types.ToolResultBlockText && b.Text == "[dropped image: image/png, 3 bytes]" {
					noted = true
				}
			}
			// The result also has a text block, so providers send the blocks
			// and the note must be among them.
			if !noted {
				t.Errorf("blocks carry no dropped-image note: %+v", r.Blocks)
			}
			if r.IsError {
				t.Error("a dropped block is not a tool error")
			}
		}},
		{"structured over cap is dropped whole", func(s ServerSpec) ServerSpec { s.MaxResultBytes = 10; return s }, "t_structured", func(t *testing.T, r types.ToolResult) {
			// {"n":1} is 7 bytes, but as the only content it is also the text
			// projection, so it costs 14.
			if !strings.Contains(r.Text, "dropped structured content") {
				t.Errorf("text = %q", r.Text)
			}
			if len(r.Blocks) != 1 || r.Blocks[0].Kind != types.ToolResultBlockText || !strings.Contains(r.Blocks[0].Text, "dropped structured content") {
				t.Errorf("blocks = %+v, want only the note", r.Blocks)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := connect(t, tt.spec(ts.spec()))
			res, err := toolNamed(t, c, tt.tool).ExecuteRich(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, res)
		})
	}
}

func TestAnnotationsBecomeCapabilities(t *testing.T) {
	ts := newTestServer(t)
	tests := []struct {
		trust bool
		tool  string
		want  types.ToolCapability
	}{
		{false, "t_wipe", types.ToolCapabilityDestructive},
		{true, "t_wipe", types.ToolCapabilityDestructive},
		{false, "t_echo", types.ToolCapabilityUnknown},
		{true, "t_echo", types.ToolCapabilityRead},
		{true, "t_media", types.ToolCapabilityUnknown},
	}
	for _, tt := range tests {
		spec := ts.spec()
		spec.TrustHints = tt.trust
		c := connect(t, spec)
		if got := toolNamed(t, c, tt.tool).Definition().Capability; got != tt.want {
			t.Errorf("trust=%v %s: capability %q, want %q", tt.trust, tt.tool, got, tt.want)
		}
	}
}

func TestCapabilityGate(t *testing.T) {
	gate := CapabilityGate(DefaultCapabilityPolicy())
	tests := []struct {
		capability types.ToolCapability
		want       types.GateOutcome
	}{
		{types.ToolCapabilityRead, types.GateAllow},
		{types.ToolCapabilityWrite, types.GateRequireApproval},
		{types.ToolCapabilityDestructive, types.GateRequireApproval},
		{"", types.GateRequireApproval},
	}
	for _, tt := range tests {
		got := gate.Check(context.Background(), types.ToolDef{Name: "x", Capability: tt.capability}, nil)
		if got.Outcome != tt.want {
			t.Errorf("%q: outcome %v, want %v", tt.capability, got.Outcome, tt.want)
		}
	}
}

func TestArgTransformCoercesScalars(t *testing.T) {
	ts := newTestServer(t)
	spec := ts.spec()
	spec.ArgTransform = CoerceScalarsToArrays
	c := connect(t, spec)
	res, err := toolNamed(t, c, "t_tags").ExecuteRich(context.Background(), map[string]any{"tags": "go", "other": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, `"tags":["go"]`) || !strings.Contains(res.Text, `"other":"x"`) {
		t.Errorf("server received %s", res.Text)
	}
}

func TestMaxConcurrentBoundsInFlightCalls(t *testing.T) {
	ts := newTestServer(t)
	var inFlight, peak atomic.Int64
	ts.add(&mcpsdk.Tool{Name: "work", InputSchema: objectSchema}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		inFlight.Add(-1)
		return textResult("ok"), nil
	})
	spec := ts.spec()
	spec.MaxConcurrent = 2
	c := connect(t, spec)
	work := toolNamed(t, c, "t_work")

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if res, _ := work.ExecuteRich(context.Background(), nil); res.IsError {
				t.Error(res.Text)
			}
		}()
	}
	wg.Wait()
	if got := peak.Load(); got > 2 {
		t.Errorf("peak in-flight = %d, want at most 2", got)
	}
}

func TestProgressIsRoutedToItsCall(t *testing.T) {
	ts := newTestServer(t)
	c := connect(t, ts.spec())
	var mu sync.Mutex
	var got []Progress
	ctx := WithProgress(context.Background(), func(p Progress) {
		mu.Lock()
		got = append(got, p)
		mu.Unlock()
	})
	if res, err := toolNamed(t, c, "t_slow").ExecuteRich(ctx, nil); err != nil || res.IsError {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[1].Progress != 2 || got[1].Total != 2 || got[0].Tool != "slow" || got[0].Server != "t" {
		t.Errorf("progress = %+v", got)
	}
}

func TestResourcesAndPrompts(t *testing.T) {
	ts := newTestServer(t)
	c := connect(t, ts.spec())
	ctx := context.Background()

	resources, err := c.Resources(ctx)
	if err != nil || len(resources) != 1 || resources[0].URI != "file:///readme.md" {
		t.Fatalf("resources=%+v err=%v", resources, err)
	}
	contents, err := c.ReadResource(ctx, "file:///readme.md")
	if err != nil || len(contents) != 1 || contents[0].Text != "# readme" || contents[0].Citation.URI != "file:///readme.md" {
		t.Fatalf("contents=%+v err=%v", contents, err)
	}
	prompts, err := c.Prompts(ctx)
	if err != nil || len(prompts) != 1 || !prompts[0].Arguments[0].Required {
		t.Fatalf("prompts=%+v err=%v", prompts, err)
	}
	rendered, err := c.GetPrompt(ctx, "greet", map[string]string{"who": "ada"})
	if err != nil || len(rendered.Messages) != 1 || rendered.Messages[0].Text != "hello ada" || rendered.Messages[0].Role != "user" {
		t.Fatalf("rendered=%+v err=%v", rendered, err)
	}
}

func TestToolListChangeRemovesTools(t *testing.T) {
	ts := newTestServer(t)
	changed := make(chan string, 4)
	spec := ts.spec()
	spec.OnToolsChanged = func(server string) { changed <- server }
	spec.Gate = types.GateFunc(func(_ context.Context, def types.ToolDef, _ map[string]any) types.GateDecision {
		if def.Name == "t_wipe" {
			return types.Deny("wipe is not allowed")
		}
		return types.Allow()
	})
	waitChange := func() {
		t.Helper()
		select {
		case server := <-changed:
			if server != "t" {
				t.Errorf("OnToolsChanged(%q)", server)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no tools/list_changed notification")
		}
	}

	pool := NewPool()
	t.Cleanup(func() { _ = pool.Close() })
	c, err := pool.Acquire(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	reg := types.NewToolRegistry()
	if _, err := pool.RegisterAll(context.Background(), reg); err != nil {
		t.Fatal(err)
	}
	wipe, _ := reg.Get("t_wipe")
	gate := pool.Gate()
	assertGated := func(when string) {
		t.Helper()
		if d := gate.Check(context.Background(), wipe.Definition(), nil); d.Outcome == types.GateAllow {
			t.Errorf("%s: per-server gate not applied to the stale tool: %+v", when, d)
		}
	}
	assertRefused := func(when string) {
		t.Helper()
		res, _ := wipe.(types.RichTool).ExecuteRich(context.Background(), nil)
		if !res.IsError || !strings.Contains(res.Text, "no longer offers") {
			t.Errorf("%s: stale tool call: %+v", when, res)
		}
	}

	ts.server.RemoveTools("wipe")
	waitChange()
	changes, err := pool.Refresh(context.Background(), reg)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes.Removed) != 1 || changes.Removed[0] != "t_wipe" {
		t.Errorf("changes = %+v", changes)
	}
	// The registry removes the withdrawn tool, so the model no longer sees
	// it. A caller still holding the old instance is refused locally.
	if _, ok := reg.Get("t_wipe"); ok {
		t.Error("the withdrawn tool is still registered")
	}
	assertRefused("after refresh")

	// With no cached listing the withdrawn tool must still not reach the
	// server, and a second refresh must not report it again.
	c.InvalidateCatalog()
	assertRefused("after invalidation")
	if changes, err := pool.Refresh(context.Background(), reg); err != nil || len(changes.Removed) != 0 {
		t.Errorf("second refresh = %+v, %v", changes, err)
	}
	c.InvalidateCatalog()
	assertRefused("after second invalidation")
	if n := ts.count("wipe"); n != 0 {
		t.Errorf("removed tool reached the server %d times", n)
	}

	// Offering the tool again restores it, still behind the gate.
	ts.add(&mcpsdk.Tool{Name: "wipe", InputSchema: objectSchema}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return textResult("wiped"), nil
	})
	waitChange()
	changes, err = pool.Refresh(context.Background(), reg)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes.Added) != 1 || changes.Added[0] != "t_wipe" {
		t.Errorf("changes after re-adding = %+v", changes)
	}
	assertGated("after re-adding")
	if res, _ := wipe.(types.RichTool).ExecuteRich(context.Background(), nil); res.IsError {
		t.Errorf("restored tool call: %s", res.Text)
	}
}

func TestCloseCancelsHungListing(t *testing.T) {
	ts := newTestServer(t)
	c, err := Connect(context.Background(), ts.spec())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	inner := *ts.handler.Load()
	entered := make(chan struct{}, 1)
	// release frees the hung handler at cleanup, before the test server's
	// Close waits for it, so a regression fails instead of hanging.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var hung http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		if strings.Contains(string(body), `"tools/list"`) {
			select {
			case entered <- struct{}{}:
			default:
			}
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		inner.ServeHTTP(w, r)
	})
	ts.handler.Store(&hung)

	listed := make(chan error, 1)
	go func() {
		_, err := c.Catalog(context.Background())
		listed <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("listing never reached the server")
	}

	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Errorf("Close with a hung listing: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close waited on a hung listing")
	}
	select {
	case err := <-listed:
		if err == nil {
			t.Error("hung listing returned no error after Close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hung listing never returned")
	}
}

func TestCatalogFingerprintAndDrift(t *testing.T) {
	ts := newTestServer(t)
	c := connect(t, ts.spec())
	before, err := c.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	ts.server.RemoveTools("wipe")
	ts.add(&mcpsdk.Tool{Name: "echo", Description: "echo text, now different", InputSchema: objectSchema}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return textResult(""), nil
	})
	ts.add(&mcpsdk.Tool{Name: "fresh", InputSchema: objectSchema}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return textResult(""), nil
	})
	c.InvalidateCatalog()
	after, err := c.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	d := DiffCatalogs(before, after)
	if strings.Join(d.Added, ",") != "fresh" || strings.Join(d.Removed, ",") != "wipe" || strings.Join(d.Changed, ",") != "echo" {
		t.Errorf("drift = %+v", d)
	}
	tests := []struct {
		maxDrop  int
		mustKeep []string
		ok       bool
	}{
		{1, nil, true},
		{0, nil, false},
		{1, []string{"media"}, true},
		{1, []string{"echo"}, false},
		{-1, []string{"wipe"}, false},
	}
	for _, tt := range tests {
		if err := d.Within(tt.maxDrop, tt.mustKeep...); (err == nil) != tt.ok {
			t.Errorf("Within(%d, %v) = %v, want ok=%v", tt.maxDrop, tt.mustKeep, err, tt.ok)
		}
	}
}

func TestFingerprintIgnoresKeyOrder(t *testing.T) {
	a := Fingerprint("x", "d", json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}}}`))
	b := Fingerprint("x", "d", json.RawMessage(`{ "properties": {"a": {"type":"string"}}, "type": "object" }`))
	if a != b {
		t.Error("whitespace and key order changed the fingerprint")
	}
	if a == Fingerprint("x", "other", nil) {
		t.Error("a changed description must change the fingerprint")
	}
}

func TestDeadSessionReconnects(t *testing.T) {
	ts := newTestServer(t)
	spec := ts.spec()
	spec.TrustHints = true
	c := connect(t, spec)
	echo := toolNamed(t, c, "t_echo")
	wipe := toolNamed(t, c, "t_wipe")

	ts.resetSessions()
	res, err := echo.ExecuteRich(context.Background(), map[string]any{"text": "again"})
	if err != nil || res.IsError || res.Text != "again" {
		t.Fatalf("read-only call after server restart: res=%+v err=%v", res, err)
	}

	ts.resetSessions()
	before := ts.count("wipe")
	res, err = wipe.ExecuteRich(context.Background(), nil)
	if err != nil || !res.IsError || !strings.Contains(res.Text, "not retried") {
		t.Fatalf("destructive call after restart: res=%+v err=%v", res, err)
	}
	if ts.count("wipe") != before {
		t.Error("a non-idempotent call was repeated after reconnect")
	}
	// The reconnect itself succeeded, so the next call goes through.
	if res, _ := wipe.ExecuteRich(context.Background(), nil); res.IsError {
		t.Errorf("call after reconnect: %s", res.Text)
	}
}

func TestUntrustedReadHintIsNotRetriedAfterReconnect(t *testing.T) {
	tests := []struct {
		name       string
		trust      bool
		idempotent []string
		wantRetry  bool
	}{
		{"untrusted hint", false, nil, false},
		{"trusted hint", true, nil, true},
		{"listed without trust", false, []string{"echo"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := newTestServer(t)
			spec := ts.spec()
			spec.TrustHints = tt.trust
			spec.IdempotentTools = tt.idempotent
			echo := toolNamed(t, connect(t, spec), "t_echo")
			ts.resetSessions()
			before := ts.count("echo")
			res, _ := echo.ExecuteRich(context.Background(), map[string]any{"text": "x"})
			if retried := !res.IsError; retried != tt.wantRetry {
				t.Errorf("retried=%v (%s), want %v", retried, res.Text, tt.wantRetry)
			}
			if got := ts.count("echo") - before; tt.wantRetry != (got == 1) {
				t.Errorf("echo reached the server %d times", got)
			}
		})
	}
}

func TestPoolHealthAndProbe(t *testing.T) {
	ts := newTestServer(t)
	pool := NewPool()
	t.Cleanup(func() { _ = pool.Close() })
	if _, err := pool.Acquire(context.Background(), ts.spec()); err != nil {
		t.Fatal(err)
	}
	if err := pool.Preflight(context.Background()); err != nil {
		t.Errorf("Preflight on a healthy pool: %v", err)
	}

	res := Probe(context.Background(), ts.spec())
	if !res.OK || res.Kind != ProbeOK || res.ToolCount < 5 || len(res.Tools) != res.ToolCount {
		t.Errorf("probe = %+v", res)
	}

	unauthorized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "token expired for tenant", http.StatusUnauthorized)
	}))
	t.Cleanup(unauthorized.Close)
	refused := httptest.NewServer(http.NotFoundHandler())
	refusedURL := refused.URL
	refused.Close()

	tests := []struct {
		name string
		spec ServerSpec
		kind ProbeKind
		msg  string
	}{
		{"auth", Remote("a", unauthorized.URL), ProbeAuthRejected, "token expired for tenant"},
		{"refused", Remote("r", refusedURL), ProbeUnreachable, ""},
		{"blocked", ServerSpec{Name: "b", URL: "http://169.254.169.254/mcp", HTTPClient: SafeHTTPClient(false)}, ProbeBlocked, ""},
		{"missing binary", Local("m", "/nonexistent/saige-test-server"), ProbeUnreachable, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.spec.ConnectTimeout = 3 * time.Second
			got := Probe(context.Background(), tt.spec)
			if got.OK || got.Kind != tt.kind || !strings.Contains(got.Message, tt.msg) {
				t.Errorf("probe = %+v, want kind %s", got, tt.kind)
			}
		})
	}
}

func TestPoolAcquireSharesOneHandshake(t *testing.T) {
	ts := newTestServer(t)
	var inits atomic.Int64
	inner := ts.http.Config.Handler
	ts.http.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A handshake is one initialize request. Clients on the 2026-07-28
		// protocol first try server/discover, which a stateful server
		// rejects, so that probe is not counted.
		if r.Method == http.MethodPost && r.Header.Get("Mcp-Session-Id") == "" {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			if bytes.Contains(body, []byte(`"method":"initialize"`)) {
				inits.Add(1)
			}
		}
		inner.ServeHTTP(w, r)
	})

	pool := NewPool()
	t.Cleanup(func() { _ = pool.Close() })
	var wg sync.WaitGroup
	clients := make([]*Client, 8)
	for i := range clients {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := pool.Acquire(context.Background(), ts.spec())
			if err != nil {
				t.Error(err)
			}
			clients[i] = c
		}(i)
	}
	wg.Wait()
	for _, c := range clients[1:] {
		if c != clients[0] {
			t.Fatal("same identity produced different clients")
		}
	}
	if n := inits.Load(); n != 1 {
		t.Errorf("%d handshakes, want 1", n)
	}

	other := ts.spec()
	other.AllowedTools = []string{"echo"}
	c, err := pool.Acquire(context.Background(), other)
	if err != nil {
		t.Fatal(err)
	}
	if c == clients[0] {
		t.Error("a different allowlist must not share a session")
	}
}

func TestAddAllKeepsEarlierClients(t *testing.T) {
	ts := newTestServer(t)
	pool := NewPool()
	t.Cleanup(func() { _ = pool.Close() })
	first, err := pool.Add(context.Background(), ts.spec())
	if err != nil {
		t.Fatal(err)
	}
	reg := types.NewToolRegistry()
	if _, err := pool.RegisterAll(context.Background(), reg); err != nil {
		t.Fatal(err)
	}

	b := ts.spec()
	b.Name = "b"
	err = pool.AddAll(context.Background(), b, ServerSpec{Name: "invalid"})
	if err == nil {
		t.Fatal("AddAll with an invalid spec must fail")
	}
	clients := pool.Clients()
	if len(clients) != 1 || clients[0] != first {
		t.Fatalf("clients after failed AddAll = %d, want only the earlier one", len(clients))
	}
	if owner, ok := pool.Owner("t_echo"); !ok || owner != first {
		t.Error("the earlier client's gate routing was wiped")
	}
	if res, _ := toolNamed(t, first, "t_echo").ExecuteRich(context.Background(), map[string]any{"text": "ok"}); res.IsError {
		t.Errorf("earlier client was closed: %s", res.Text)
	}
}

func TestRegistrationRefusesToReplaceLocalTools(t *testing.T) {
	ts := newTestServer(t)
	local := &types.ToolFunc{Def: types.ToolDef{Name: "echo"}, Fn: func(context.Context, map[string]any) (string, error) { return "local", nil }}

	spec := ts.spec()
	spec.ToolPrefix = "-"
	tests := []struct {
		name     string
		register func(reg *types.ToolRegistry) error
	}{
		{"pool", func(reg *types.ToolRegistry) error {
			pool := NewPool()
			t.Cleanup(func() { _ = pool.Close() })
			if _, err := pool.Add(context.Background(), spec); err != nil {
				return err
			}
			_, err := pool.RegisterAll(context.Background(), reg)
			if _, owned := pool.Owner("echo"); owned {
				t.Error("pool routes the gate for a name it does not own")
			}
			return err
		}},
		{"client", func(reg *types.ToolRegistry) error {
			_, err := connect(t, spec).Register(context.Background(), reg)
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := types.NewToolRegistry(local)
			err := tt.register(reg)
			if err == nil || !strings.Contains(err.Error(), "already registered") {
				t.Fatalf("err = %v, want a collision error", err)
			}
			if got, _ := reg.Get("echo"); got != types.Tool(local) {
				t.Error("the local tool was replaced")
			}
		})
	}
}

func TestReRegisteringOwnToolsIsAllowed(t *testing.T) {
	ts := newTestServer(t)
	c := connect(t, ts.spec())
	reg := types.NewToolRegistry()
	for i := 0; i < 2; i++ {
		if _, err := c.Register(context.Background(), reg); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	if errors.Is(context.Canceled, context.Canceled) && len(reg.Definitions()) == 0 {
		t.Fatal("nothing registered")
	}
}
