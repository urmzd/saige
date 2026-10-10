package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

type sseFrame struct {
	id   uint64
	kind string
	env  types.Envelope
}

// serveFixture runs a server whose sessions use a scripted provider and one
// marked tool that counts its executions.
type serveFixture struct {
	t     *testing.T
	srv   *httptest.Server
	app   *server
	calls *atomic.Int32
}

func newServeFixture(t *testing.T, responses [][]types.Delta, opts serveOptions) *serveFixture {
	t.Helper()
	calls := &atomic.Int32{}
	tool := countedDanger(calls)
	opts.newAgent = func() (*agentsdk.Agent, error) {
		return must.Get(agentsdk.New(agentsdk.Config{
			Name:     "test",
			Provider: &agenttest.ScriptedProvider{Responses: responses},
			Tools:    types.NewToolRegistry(tool),
		})), nil
	}
	return newServeFixtureWith(t, opts, calls)
}

// newServeFixtureWith runs a server with the caller's newAgent; calls is
// the counter the caller's tools report to.
func newServeFixtureWith(t *testing.T, opts serveOptions, calls *atomic.Int32) *serveFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	app := newServer(ctx, opts)
	srv := httptest.NewServer(app.handler())
	t.Cleanup(srv.Close)
	return &serveFixture{t: t, srv: srv, app: app, calls: calls}
}

// countedDanger is a tool behind a human_approval marker that counts its
// executions.
func countedDanger(calls *atomic.Int32) types.Tool {
	return types.WithMarkers(&types.ToolFunc{
		Def: types.ToolDef{Name: "danger", Parameters: types.ParameterSchema{Type: "object"}},
		Fn: func(context.Context, map[string]any) (string, error) {
			calls.Add(1)
			return "did it", nil
		},
	}, types.Marker{Kind: "human_approval", Message: "approve danger"})
}

func (f *serveFixture) post(path string, body any, wantStatus int) map[string]any {
	f.t.Helper()
	return f.do(http.MethodPost, path, body, wantStatus)
}

func (f *serveFixture) do(method, path string, body any, wantStatus int) map[string]any {
	f.t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, f.srv.URL+path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != wantStatus {
		f.t.Fatalf("%s %s = %d %v, want %d", method, path, resp.StatusCode, out, wantStatus)
	}
	return out
}

func (f *serveFixture) startTurn(message string) (sid, tid string) {
	f.t.Helper()
	sid = f.post("/v1/sessions", map[string]any{}, http.StatusCreated)["session_id"].(string)
	tid = f.post("/v1/sessions/"+sid+"/turns", map[string]any{"message": message}, http.StatusAccepted)["turn_id"].(string)
	return sid, tid
}

// events reads the SSE stream until it ends or until stop returns true.
func (f *serveFixture) events(sid, tid, lastEventID string, stop func(sseFrame) bool) []sseFrame {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, f.srv.URL+"/v1/sessions/"+sid+"/turns/"+tid+"/events", nil)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		f.t.Fatalf("content type = %q", ct)
	}
	var frames []sseFrame
	var cur sseFrame
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			cur.id, _ = strconv.ParseUint(strings.TrimPrefix(line, "id: "), 10, 64)
		case strings.HasPrefix(line, "event: "):
			cur.kind = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			env, err := types.UnmarshalEnvelope([]byte(strings.TrimPrefix(line, "data: ")))
			if err != nil {
				f.t.Fatalf("bad envelope: %v", err)
			}
			cur.env = env
		case line == "" && cur.kind != "":
			frames = append(frames, cur)
			if stop != nil && stop(cur) {
				return frames
			}
			cur = sseFrame{}
		}
	}
	return frames
}

func kinds(frames []sseFrame) []string {
	var out []string
	for _, f := range frames {
		out = append(out, f.kind)
	}
	return out
}

func waitDone(t *testing.T, f *serveFixture, sid, tid string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(f.srv.URL + "/v1/sessions/" + sid + "/turns/" + tid)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		_ = resp.Body.Close()
		if out["done"] == true {
			return out
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("turn did not finish")
	return nil
}

func TestServeStreamsTurnAsEnvelopes(t *testing.T) {
	f := newServeFixture(t, [][]types.Delta{agenttest.TextResponse("hello there")}, serveOptions{})
	sid, tid := f.startTurn("hi")
	frames := f.events(sid, tid, "", nil)

	got := strings.Join(kinds(frames), ",")
	if !strings.Contains(got, types.WirePartDelta) || !strings.HasSuffix(got, types.WireDone) {
		t.Fatalf("kinds = %s, want text deltas ending in done", got)
	}
	for i, fr := range frames {
		if fr.id != uint64(i+1) || fr.env.Seq != fr.id {
			t.Errorf("frame %d: id %d seq %d, want both %d", i, fr.id, fr.env.Seq, i+1)
		}
		if fr.env.RunID != tid || fr.env.Kind != fr.kind {
			t.Errorf("frame %d: run_id %q kind %q, want %q %q", i, fr.env.RunID, fr.env.Kind, tid, fr.kind)
		}
	}
	var text strings.Builder
	for _, fr := range frames {
		if d, err := fr.env.Delta(); err == nil {
			if td, ok := d.(types.PartDelta); ok {
				text.WriteString(td.Text)
			}
		}
	}
	if text.String() != "hello there" {
		t.Errorf("text = %q", text.String())
	}

	t.Run("Last-Event-ID resumes after the given seq", func(t *testing.T) {
		resumed := f.events(sid, tid, "2", nil)
		if len(resumed) != len(frames)-2 || resumed[0].id != 3 {
			t.Errorf("resumed %d frames starting at %d, want %d starting at 3", len(resumed), resumed[0].id, len(frames)-2)
		}
	})
	t.Run("tree is served", func(t *testing.T) {
		resp, err := http.Get(f.srv.URL + "/v1/sessions/" + sid + "/tree")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !json.Valid(body) || !strings.Contains(string(body), "hello there") {
			t.Errorf("tree = %d %s", resp.StatusCode, body)
		}
	})
}

func TestServeApprovals(t *testing.T) {
	script := [][]types.Delta{
		agenttest.ToolCallResponse("call_1", "danger", map[string]any{}),
		agenttest.TextResponse("finished"),
	}
	isMarker := func(fr sseFrame) bool { return fr.kind == types.WireMarker }

	tests := []struct {
		name      string
		decide    func(f *serveFixture, base string)
		timeout   time.Duration
		wantCalls int32
		wantErr   bool
	}{
		{
			name: "approved runs the tool",
			decide: func(f *serveFixture, base string) {
				f.post(base+"/interrupts/call_1", map[string]any{"approved": true}, http.StatusOK)
				f.post(base+"/interrupts/call_1", map[string]any{"approved": true}, http.StatusConflict)
			},
			wantCalls: 1,
		},
		{
			name: "denied skips the tool",
			decide: func(f *serveFixture, base string) {
				f.post(base+"/interrupts/call_1", map[string]any{"approved": false, "message": "no"}, http.StatusOK)
			},
		},
		{
			name: "missing decision is rejected, then timeout denies",
			decide: func(f *serveFixture, base string) {
				f.post(base+"/interrupts/call_1", map[string]any{"message": "yes please"}, http.StatusBadRequest)
				f.post(base+"/interrupts/unknown", map[string]any{"approved": true}, http.StatusNotFound)
			},
			timeout: 100 * time.Millisecond,
		},
		{
			name: "cancel ends the turn",
			decide: func(f *serveFixture, base string) {
				f.post(base+"/cancel", map[string]any{}, http.StatusAccepted)
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newServeFixture(t, script, serveOptions{approvalTimeout: tt.timeout})
			sid, tid := f.startTurn("do the thing")
			frames := f.events(sid, tid, "", isMarker)
			if len(frames) == 0 || !isMarker(frames[len(frames)-1]) {
				t.Fatalf("no marker event, got %v", kinds(frames))
			}
			d, err := frames[len(frames)-1].env.Delta()
			if err != nil {
				t.Fatal(err)
			}
			if m := d.(types.MarkerDelta); m.ToolCallID != "call_1" || m.ToolName != "danger" {
				t.Fatalf("marker = %+v", m)
			}

			tt.decide(f, "/v1/sessions/"+sid+"/turns/"+tid)
			status := waitDone(t, f, sid, tid)
			if got := f.calls.Load(); got != tt.wantCalls {
				t.Errorf("tool ran %d times, want %d", got, tt.wantCalls)
			}
			if _, hasErr := status["error"]; hasErr != tt.wantErr {
				t.Errorf("turn status = %v, want error %v", status, tt.wantErr)
			}
		})
	}
}

func TestServeOneTurnAtATime(t *testing.T) {
	f := newServeFixture(t, [][]types.Delta{
		agenttest.ToolCallResponse("call_1", "danger", map[string]any{}),
		agenttest.TextResponse("finished"),
	}, serveOptions{})
	sid, tid := f.startTurn("first")
	f.events(sid, tid, "", func(fr sseFrame) bool { return fr.kind == types.WireMarker })
	f.post("/v1/sessions/"+sid+"/turns", map[string]any{"message": "second"}, http.StatusConflict)
	f.post("/v1/sessions/"+sid+"/turns/"+tid+"/cancel", map[string]any{}, http.StatusAccepted)
	waitDone(t, f, sid, tid)
}

func TestServeGuard(t *testing.T) {
	h := newServer(context.Background(), serveOptions{}).handler()
	withToken := newServer(context.Background(), serveOptions{token: "s3cret"}).handler()
	tests := []struct {
		name    string
		handler http.Handler
		method  string
		host    string
		ctype   string
		auth    string
		want    int
	}{
		{name: "loopback host allowed", handler: h, method: http.MethodGet, host: "127.0.0.1:8787", want: http.StatusOK},
		{name: "localhost allowed", handler: h, method: http.MethodGet, host: "localhost:8787", want: http.StatusOK},
		{name: "foreign host refused without token", handler: h, method: http.MethodGet, host: "evil.example:8787", want: http.StatusForbidden},
		{name: "form post refused", handler: h, method: http.MethodPost, host: "127.0.0.1", ctype: "text/plain", want: http.StatusUnsupportedMediaType},
		{name: "token required", handler: withToken, method: http.MethodGet, host: "10.0.0.2", want: http.StatusUnauthorized},
		{name: "wrong token", handler: withToken, method: http.MethodGet, host: "10.0.0.2", auth: "Bearer nope", want: http.StatusUnauthorized},
		{name: "right token", handler: withToken, method: http.MethodGet, host: "10.0.0.2", auth: "Bearer s3cret", want: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "/healthz", strings.NewReader("{}"))
			req.Host = tt.host
			if tt.ctype != "" {
				req.Header.Set("Content-Type", tt.ctype)
			}
			if tt.auth != "" {
				req.Header.Set("Authorization", tt.auth)
			}
			rec := httptest.NewRecorder()
			tt.handler.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d (%s)", rec.Code, tt.want, rec.Body)
			}
		})
	}
}

func TestCheckServeAddr(t *testing.T) {
	tests := []struct {
		addr, token string
		wantErr     bool
	}{
		{"127.0.0.1:8787", "", false},
		{"localhost:0", "", false},
		{"[::1]:8787", "", false},
		{"0.0.0.0:8787", "", true},
		{":8787", "", true},
		{"0.0.0.0:8787", "t", false},
		{"no-port", "", true},
	}
	for _, tt := range tests {
		if err := checkServeAddr(tt.addr, tt.token); (err != nil) != tt.wantErr {
			t.Errorf("checkServeAddr(%q, %q) = %v, wantErr %v", tt.addr, tt.token, err, tt.wantErr)
		}
	}
}

func TestBuildPackTools(t *testing.T) {
	ws := t.TempDir()
	tests := []struct {
		name      string
		packs     []string
		workspace string
		network   string
		want      []string
		wantErr   bool
	}{
		{name: "none", want: nil},
		{name: "fs read only", packs: []string{"fs"}, workspace: ws, want: []string{"read", "list", "glob", "grep"}},
		{name: "fs with writes", packs: []string{"fs-write"}, workspace: ws, want: []string{"read", "list", "glob", "grep", "write", "edit"}},
		{name: "fetch", packs: []string{"fetch"}, want: []string{"fetch"}},
		{name: "bash with network allowed", packs: []string{"bash"}, workspace: ws, network: "allow", want: []string{"bash"}},
		{name: "fs needs a workspace", packs: []string{"fs"}, wantErr: true},
		{name: "unknown pack", packs: []string{"nope"}, wantErr: true},
		{name: "bad network policy", packs: []string{"bash"}, workspace: ws, network: "maybe", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tools, err := buildPackTools(tt.packs, tt.workspace, tt.network)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			var names []string
			for _, tool := range tools {
				names = append(names, tool.Definition().Name)
			}
			if strings.Join(names, ",") != strings.Join(tt.want, ",") {
				t.Errorf("tools = %v, want %v", names, tt.want)
			}
		})
	}
}

func TestServeSubAgentApproval(t *testing.T) {
	// A sub-agent's marker id is "<delegation id>/<child call id>".
	tests := []struct {
		name string
		path string
	}{
		{name: "slash sent as-is", path: "/interrupts/delegate/call_1"},
		{name: "slash percent-encoded", path: "/interrupts/delegate%2Fcall_1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := &atomic.Int32{}
			f := newServeFixtureWith(t, serveOptions{newAgent: func() (*agentsdk.Agent, error) {
				return must.Get(agentsdk.New(agentsdk.Config{
					Name: "parent",
					Provider: &agenttest.ScriptedProvider{Responses: [][]types.Delta{
						agenttest.ToolCallResponse("delegate", "delegate_to_child", map[string]any{"task": "work"}),
						agenttest.TextResponse("finished"),
					}},
					SubAgents: []agentsdk.SubAgentDef{{
						Name: "child",
						Provider: &agenttest.ScriptedProvider{Responses: [][]types.Delta{
							agenttest.ToolCallResponse("call_1", "danger", map[string]any{}),
							agenttest.TextResponse("child done"),
						}},
						Tools: types.NewToolRegistry(countedDanger(calls)),
					}},
				})), nil
			}}, calls)
			sid, tid := f.startTurn("delegate it")
			frames := f.events(sid, tid, "", func(fr sseFrame) bool { return fr.kind == types.WireMarker })
			d, err := frames[len(frames)-1].env.Delta()
			if err != nil {
				t.Fatal(err)
			}
			if m, ok := d.(types.MarkerDelta); !ok || m.ToolCallID != "delegate/call_1" {
				t.Fatalf("marker = %+v, want id delegate/call_1", d)
			}
			got := f.post("/v1/sessions/"+sid+"/turns/"+tid+tt.path, map[string]any{"approved": true}, http.StatusOK)
			if got["tool_call_id"] != "delegate/call_1" {
				t.Errorf("tool_call_id = %v", got["tool_call_id"])
			}
			waitDone(t, f, sid, tid)
			if calls.Load() != 1 {
				t.Errorf("child tool ran %d times, want 1", calls.Load())
			}
		})
	}
}

func TestServeDeleteSession(t *testing.T) {
	script := [][]types.Delta{
		agenttest.ToolCallResponse("call_1", "danger", map[string]any{}),
		agenttest.TextResponse("finished"),
	}
	f := newServeFixture(t, script, serveOptions{maxSessions: 1})
	sid, tid := f.startTurn("do the thing")
	f.events(sid, tid, "", func(fr sseFrame) bool { return fr.kind == types.WireMarker })
	f.post("/v1/sessions", map[string]any{}, http.StatusTooManyRequests)

	sess := f.app.sessions.Get(sid)
	sess.Host.mu.Lock()
	tr := sess.Host.byID[tid]
	sess.Host.mu.Unlock()

	f.do(http.MethodDelete, "/v1/sessions/"+sid, nil, http.StatusOK)
	f.do(http.MethodDelete, "/v1/sessions/"+sid, nil, http.StatusNotFound)
	f.do(http.MethodGet, "/v1/sessions/"+sid+"/turns/"+tid, nil, http.StatusNotFound)
	// The freed slot is usable at once.
	f.post("/v1/sessions", map[string]any{}, http.StatusCreated)

	deadline := time.Now().Add(5 * time.Second)
	for !tr.finished() {
		if time.Now().After(deadline) {
			t.Fatal("deleting the session did not cancel its turn")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if f.calls.Load() != 0 {
		t.Errorf("tool ran %d times after its session was deleted", f.calls.Load())
	}
}

func TestServeSessionCapUnderConcurrency(t *testing.T) {
	const limit, clients = 3, 24
	f := newServeFixtureWith(t, serveOptions{maxSessions: limit, newAgent: func() (*agentsdk.Agent, error) {
		time.Sleep(20 * time.Millisecond)
		return must.Get(agentsdk.New(agentsdk.Config{Name: "test", Provider: &agenttest.ScriptedProvider{}})), nil
	}}, nil)
	var created atomic.Int32
	var wg sync.WaitGroup
	for range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/v1/sessions", strings.NewReader("{}"))
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusCreated {
				created.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := created.Load(); got != limit {
		t.Errorf("created %d sessions, want exactly %d", got, limit)
	}
}

func TestServeEvictsIdleSessions(t *testing.T) {
	const ttl = time.Hour
	script := [][]types.Delta{
		agenttest.ToolCallResponse("call_1", "danger", map[string]any{}),
		agenttest.TextResponse("finished"),
	}
	tests := []struct {
		name     string
		setup    func(f *serveFixture) string
		after    time.Duration
		wantKept bool
	}{
		{
			name: "fresh empty session is kept",
			setup: func(f *serveFixture) string {
				return f.post("/v1/sessions", map[string]any{}, http.StatusCreated)["session_id"].(string)
			},
			after:    ttl / 2,
			wantKept: true,
		},
		{
			name: "idle empty session is dropped",
			setup: func(f *serveFixture) string {
				return f.post("/v1/sessions", map[string]any{}, http.StatusCreated)["session_id"].(string)
			},
			after: 2 * ttl,
		},
		{
			name: "running turn keeps its session",
			setup: func(f *serveFixture) string {
				sid, tid := f.startTurn("do the thing")
				f.events(sid, tid, "", func(fr sseFrame) bool { return fr.kind == types.WireMarker })
				return sid
			},
			after:    2 * ttl,
			wantKept: true,
		},
		{
			name: "finished turn ages out",
			setup: func(f *serveFixture) string {
				sid, tid := f.startTurn("do the thing")
				f.events(sid, tid, "", func(fr sseFrame) bool { return fr.kind == types.WireMarker })
				f.post("/v1/sessions/"+sid+"/turns/"+tid+"/cancel", map[string]any{}, http.StatusAccepted)
				waitDone(f.t, f, sid, tid)
				return sid
			},
			after: 2 * ttl,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newServeFixture(t, script, serveOptions{idleTTL: ttl})
			sid := tt.setup(f)
			f.app.evictIdle(time.Now().Add(tt.after))
			want := http.StatusNotFound
			if tt.wantKept {
				want = http.StatusOK
			}
			f.do(http.MethodGet, "/v1/sessions/"+sid+"/tree", nil, want)
			if tt.wantKept {
				// Release a pending approval so the turn does not outlive the test.
				f.do(http.MethodDelete, "/v1/sessions/"+sid, nil, http.StatusOK)
			}
		})
	}
}

func TestServeReplayGap(t *testing.T) {
	f := newServeFixture(t, [][]types.Delta{agenttest.TextResponse("one two three four five six")}, serveOptions{bufferEvents: 2})
	sid, tid := f.startTurn("hi")
	status := waitDone(t, f, sid, tid)
	lastSeq := uint64(status["last_seq"].(float64))
	if lastSeq <= 3 {
		t.Fatalf("last_seq = %d, the test needs more events than the buffer holds", lastSeq)
	}
	oldest := lastSeq - 1
	base := "/v1/sessions/" + sid + "/turns/" + tid + "/events"
	tests := []struct {
		name   string
		after  string
		want   int
		frames int
	}{
		{name: "fresh client after eviction", after: "", want: http.StatusGone},
		{name: "resume before the oldest kept", after: strconv.FormatUint(oldest-2, 10), want: http.StatusGone},
		{name: "resume right before the oldest kept", after: strconv.FormatUint(oldest-1, 10), want: http.StatusOK, frames: 2},
		{name: "resume at the end", after: strconv.FormatUint(lastSeq, 10), want: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, f.srv.URL+base, nil)
			if tt.after != "" {
				req.Header.Set("Last-Event-ID", tt.after)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != tt.want {
				t.Fatalf("status = %d %s, want %d", resp.StatusCode, body, tt.want)
			}
			if tt.want == http.StatusGone {
				var out map[string]any
				_ = json.Unmarshal(body, &out)
				if uint64(out["oldest_seq"].(float64)) != oldest {
					t.Errorf("oldest_seq = %v, want %d", out["oldest_seq"], oldest)
				}
				return
			}
			if got := strings.Count(string(body), "\nevent: "); got != tt.frames {
				t.Errorf("got %d events, want %d:\n%s", got, tt.frames, body)
			}
		})
	}
}

func TestServeStreamsAGUIEvents(t *testing.T) {
	tests := []struct {
		name      string
		responses [][]types.Delta
		wantFirst string
		wantLast  string
		wantText  string
	}{
		{
			name:      "text turn",
			responses: [][]types.Delta{agenttest.TextResponse("hello there")},
			wantFirst: "RUN_STARTED",
			wantLast:  "RUN_FINISHED",
			wantText:  "hello there",
		},
		{
			name:      "failed turn",
			responses: [][]types.Delta{{types.ErrorDelta{Error: errors.New("provider down")}}},
			wantFirst: "RUN_STARTED",
			wantLast:  "RUN_ERROR",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newServeFixture(t, tt.responses, serveOptions{})
			sid, tid := f.startTurn("hi")
			waitDone(t, f, sid, tid)

			resp, err := http.Get(f.srv.URL + "/v1/sessions/" + sid + "/turns/" + tid + "/events?format=agui")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			var eventTypes []string
			var text strings.Builder
			sc := bufio.NewScanner(resp.Body)
			for sc.Scan() {
				raw, ok := strings.CutPrefix(sc.Text(), "data: ")
				if !ok {
					continue
				}
				var ev struct {
					Type  string `json:"type"`
					Delta string `json:"delta"`
				}
				if err := json.Unmarshal([]byte(raw), &ev); err != nil {
					t.Fatalf("bad event %s: %v", raw, err)
				}
				eventTypes = append(eventTypes, ev.Type)
				if ev.Type == "TEXT_MESSAGE_CONTENT" {
					text.WriteString(ev.Delta)
				}
			}
			if len(eventTypes) < 2 || eventTypes[0] != tt.wantFirst || eventTypes[len(eventTypes)-1] != tt.wantLast {
				t.Fatalf("events = %v, want %s first and %s last", eventTypes, tt.wantFirst, tt.wantLast)
			}
			if text.String() != tt.wantText {
				t.Errorf("text = %q, want %q", text.String(), tt.wantText)
			}
		})
	}
}

func TestServeApprovalGrant(t *testing.T) {
	isMarker := func(fr sseFrame) bool { return fr.kind == types.WireMarker }
	newFixture := func(t *testing.T) *serveFixture {
		calls := &atomic.Int32{}
		tool := countedDanger(calls)
		opts := serveOptions{approvalTimeout: 2 * time.Second}
		opts.newAgent = func() (*agentsdk.Agent, error) {
			return must.Get(agentsdk.New(agentsdk.Config{
				Name: "test",
				Provider: &agenttest.ScriptedProvider{Responses: [][]types.Delta{
					agenttest.ToolCallResponse("call_1", "danger", map[string]any{}),
					agenttest.ToolCallResponse("call_2", "danger", map[string]any{}),
					agenttest.TextResponse("finished"),
				}},
				Tools: types.NewToolRegistry(tool),
			}, agentsdk.WithApprovalPolicy(agentsdk.ApprovalPolicy{}))), nil
		}
		return newServeFixtureWith(t, opts, calls)
	}

	t.Run("a tool grant approves the next call", func(t *testing.T) {
		f := newFixture(t)
		sid, tid := f.startTurn("do it twice")
		f.events(sid, tid, "", isMarker)
		base := "/v1/sessions/" + sid + "/turns/" + tid
		f.post(base+"/interrupts/call_1", map[string]any{"approved": true, "grant": map[string]any{
			"scope": "tool", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		}}, http.StatusOK)
		status := waitDone(t, f, sid, tid)
		if _, failed := status["error"]; failed || f.calls.Load() != 2 {
			t.Fatalf("status %v, tool ran %d times; want the grant to approve call_2", status, f.calls.Load())
		}
		var markers int
		for _, fr := range f.events(sid, tid, "", func(sseFrame) bool { return false }) {
			if isMarker(fr) {
				markers++
			}
		}
		if markers != 1 {
			t.Fatalf("asked %d times, want once", markers)
		}
	})

	t.Run("invalid grants are rejected", func(t *testing.T) {
		f := newFixture(t)
		sid, tid := f.startTurn("do it")
		f.events(sid, tid, "", isMarker)
		path := "/v1/sessions/" + sid + "/turns/" + tid + "/interrupts/call_1"
		for name, body := range map[string]map[string]any{
			"unknown scope":       {"approved": true, "grant": map[string]any{"scope": "forever"}},
			"args without match":  {"approved": true, "grant": map[string]any{"scope": "args"}},
			"matcher without one": {"approved": true, "grant": map[string]any{"scope": "args", "match": []any{map[string]any{"field": "path"}}}},
			"expired":             {"approved": true, "grant": map[string]any{"scope": "tool", "expires_at": "2000-01-01T00:00:00Z"}},
			"on a denial":         {"approved": false, "grant": map[string]any{"scope": "tool"}},
		} {
			out := f.post(path, body, http.StatusBadRequest)
			if out["error"] == nil {
				t.Errorf("%s: no error message", name)
			}
		}
		// The marker is still waiting for a valid decision.
		f.post(path, map[string]any{"approved": false}, http.StatusOK)
		waitDone(t, f, sid, tid)
	})
}
