package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/cache/memcache"
	"github.com/urmzd/saige/agent/types"
)

func allRecordable() CachedResponse {
	return CachedResponse{
		Deltas: []types.Delta{
			types.PartStart{Index: 0, Kind: types.KindThinking},
			types.PartDelta{Index: 0, Thinking: "hmm"},
			types.PartDelta{Index: 0, Signature: "sig=="}, types.PartEnd{Index: 0},
			types.PartStart{Index: 1, Kind: types.KindText},
			types.PartDelta{Index: 1, Text: "hello"},
			types.PartEnd{Index: 1},
			types.PartStart{Index: 2, Kind: types.KindToolCall, ID: "call_1", Name: "read"},
			types.PartDelta{Index: 2, Args: `{"n":1}`},
			types.PartEnd{Index: 2, Part: types.ToolCallPart{ID: "call_1", Name: "read", Arguments: map[string]any{"n": json.Number("1"), "nested": map[string]any{"s": "x"}}}},
			types.CitationDelta{Citation: types.Citation{URI: "https://example.com", Title: "ex", Meta: map[string]any{"rank": json.Number("2")}}, ToolCallID: "call_1"},
		},
		Usage: types.UsageDelta{Cumulative: true, PromptTokens: 5, CompletionTokens: 7, TotalTokens: 12,
			Latency: 1500 * time.Millisecond, ResponseModel: "m", ResponseID: "r", FinishReasons: []string{"tool_use"}},
	}
}

func TestResponseCodecRoundTrip(t *testing.T) {
	want := allRecordable()
	raw, err := EncodeResponse(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip changed the response:\n got %#v\nwant %#v", got, want)
	}
}

func TestResponseCodecRejects(t *testing.T) {
	good, _ := EncodeResponse(allRecordable())
	errDelta, _ := types.MarshalDelta(types.ErrorDelta{Error: errors.New("x")})
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"future version", bytes.Replace(good, []byte(`"v":2`), []byte(`"v":3`), 1)},
		{"not json", []byte("{")},
		{"non-recordable delta", []byte(`{"v":2,"deltas":[` + string(errDelta) + `]}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeResponse(tc.raw); !errors.Is(err, ErrResponseCodec) {
				t.Fatalf("err = %v, want ErrResponseCodec", err)
			}
		})
	}
	if _, err := EncodeResponse(CachedResponse{Deltas: []types.Delta{types.DoneDelta{}}}); err == nil {
		t.Fatal("encoding a non-recordable delta must fail")
	}
}

func TestBytesCacheSharedAcrossInstances(t *testing.T) {
	store := BytesCache(memcache.New[[]byte]())
	msgs := []types.Message{types.UserMsg(types.Text("same"))}
	first := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.ToolCallResponse("orig", "read", map[string]any{"n": 1})}}
	second := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("unused")}}
	cfg := Config{Cache: store, ScopeKey: "tenant", ConfigKey: "v1", CacheToolCalls: true}
	collect(mustStream(t, New(first, cfg), msgs))
	replayed := collect(mustStream(t, New(second, cfg), msgs))
	calls := agenttest.CollectToolCalls(replayChan(replayed))
	if len(calls) != 1 || calls[0].ID == "orig" || calls[0].Arguments["n"] != json.Number("1") {
		t.Fatalf("calls = %+v, want one replayed call with a fresh ID", calls)
	}
	var hit bool
	for _, d := range replayed {
		if u, ok := d.(types.UsageDelta); ok && u.CacheHit {
			hit = true
		}
	}
	if !hit {
		t.Fatal("second instance missed the shared byte store")
	}
}

func TestReplayRemapsEveryCallReference(t *testing.T) {
	out := collect(replay(allRecordable()))
	var fresh string
	for _, d := range out {
		switch v := d.(type) {
		case types.PartStart:
			if v.Kind == types.KindToolCall {
				fresh = v.ID
			}
		case types.PartEnd:
			if tc, ok := v.Part.(types.ToolCallPart); ok && tc.ID != fresh {
				t.Fatalf("end ID %q, want %q", tc.ID, fresh)
			}
		case types.CitationDelta:
			if v.ToolCallID != fresh {
				t.Fatalf("citation call ID %q, want %q", v.ToolCallID, fresh)
			}
		}
	}
	if fresh == "" || fresh == "call_1" {
		t.Fatalf("call ID not replaced: %q", fresh)
	}
}

func TestTruncatedResponseNotCached(t *testing.T) {
	truncated := append(agenttest.TextResponse("cut"), types.UsageDelta{FinishReasons: []string{"max_tokens"}})
	inner := &agenttest.ScriptedProvider{Responses: [][]types.Delta{truncated, agenttest.TextResponse("full")}}
	p := newProvider(inner)
	msgs := []types.Message{types.UserMsg(types.Text("q"))}
	collect(mustStream(t, p, msgs))
	if got := text(collect(mustStream(t, p, msgs))); got != "full" {
		t.Fatalf("got %q; a truncated response was replayed", got)
	}
}

type failingStore struct{ types.Cache[CachedResponse] }

func (failingStore) Get(context.Context, string) (CachedResponse, bool, error) {
	return CachedResponse{}, false, errors.New("backend down")
}

type recordingMetrics struct {
	types.NoopMetrics
	mu  sync.Mutex
	ops []string
}

func (m *recordingMetrics) RecordProviderCall(_ context.Context, op, _ string, _ time.Duration, _ error) {
	m.mu.Lock()
	m.ops = append(m.ops, op)
	m.mu.Unlock()
}

func TestGetErrorIsReported(t *testing.T) {
	var logs bytes.Buffer
	metrics := &recordingMetrics{}
	inner := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("live")}}
	p := New(inner, Config{
		Cache:   failingStore{memcache.New[CachedResponse]()},
		Logger:  slog.New(slog.NewTextHandler(&logs, nil)),
		Metrics: metrics,
	})
	if got := text(collect(mustStream(t, p, []types.Message{types.UserMsg(types.Text("q"))}))); got != "live" {
		t.Fatalf("a read error must fall through to the provider, got %q", got)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "backend down") {
		t.Fatalf("no warning logged: %s", logs.String())
	}
	if len(metrics.ops) != 1 || metrics.ops[0] != "chat.cache_error" {
		t.Fatalf("metrics = %v, want chat.cache_error", metrics.ops)
	}
}

// slowProvider answers after release is closed and counts calls. A call
// whose ctx ends first emits nothing and closes.
type slowProvider struct {
	calls   atomic.Int32
	release chan struct{}
	resp    []types.Delta
}

func (p *slowProvider) Stream(ctx context.Context, _ types.Request) (<-chan types.Delta, error) {
	p.calls.Add(1)
	ch := make(chan types.Delta, len(p.resp))
	go func() {
		defer close(ch)
		select {
		case <-p.release:
		case <-ctx.Done():
			return
		}
		for _, d := range p.resp {
			ch <- d
		}
	}()
	return ch, nil
}

func TestSingleFlightCollapsesConcurrentMisses(t *testing.T) {
	inner := &slowProvider{release: make(chan struct{}), resp: agenttest.ToolCallResponse("orig", "read", map[string]any{"q": "x"})}
	p := New(inner, Config{Cache: memcache.New[CachedResponse](), SingleFlight: true, CacheToolCalls: true})
	msgs := []types.Message{types.UserMsg(types.Text("same"))}

	const n = 10
	ids := make(chan string, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, err := p.Stream(context.Background(), types.Request{Messages: msgs})
			if err != nil {
				t.Error(err)
				return
			}
			calls := agenttest.CollectToolCalls(ch)
			if len(calls) != 1 || calls[0].Arguments["q"] != "x" {
				t.Errorf("calls = %+v", calls)
				return
			}
			ids <- calls[0].ID
		}()
	}
	time.Sleep(50 * time.Millisecond) // let every caller join the flight
	close(inner.release)
	wg.Wait()
	close(ids)
	if got := inner.calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
	seen := map[string]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatalf("tool call ID %q shared between callers", id)
		}
		seen[id] = true
	}
	if len(seen) != n {
		t.Fatalf("%d callers answered, want %d", len(seen), n)
	}
}

func TestSingleFlightCancelledLeaderHandsOver(t *testing.T) {
	inner := &slowProvider{release: make(chan struct{}), resp: agenttest.TextResponse("answer")}
	p := New(inner, Config{Cache: memcache.New[CachedResponse](), SingleFlight: true})
	msgs := []types.Message{types.UserMsg(types.Text("same"))}

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leader, err := p.Stream(leaderCtx, types.Request{Messages: msgs})
	if err != nil {
		t.Fatal(err)
	}
	const followers = 3
	texts := make(chan string, followers)
	for range followers {
		go func() {
			ch, err := p.Stream(context.Background(), types.Request{Messages: msgs})
			if err != nil {
				texts <- "error: " + err.Error()
				return
			}
			texts <- text(collect(ch))
		}()
	}
	time.Sleep(50 * time.Millisecond)
	cancelLeader()
	collect(leader)
	time.Sleep(50 * time.Millisecond) // a follower takes over and waits on release
	close(inner.release)
	for range followers {
		if got := <-texts; got != "answer" {
			t.Fatalf("follower got %q", got)
		}
	}
	if got := inner.calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (cancelled leader, then one follower)", got)
	}
}
