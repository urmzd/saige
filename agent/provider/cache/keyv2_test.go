package cache

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/urmzd/saige/agent/cache/memcache"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

func TestKeyHashesMediaByDigestNotLocator(t *testing.T) {
	b := []byte("png bytes")
	inline := types.Bytes(types.MediaPNG, b)
	withURI := inline
	withURI.Inline, withURI.URI = nil, "https://example.com/a.png"
	withFile := inline
	withFile.Inline, withFile.Files = nil, []types.VendorFile{{Provider: "anthropic", Endpoint: "anthropic", ID: "file_1"}}
	reuploaded := withFile
	reuploaded.Files = []types.VendorFile{{Provider: "anthropic", Endpoint: "anthropic", ID: "file_2"}}
	noDigest := types.Source{MediaType: types.MediaPNG, Inline: b}
	elided := types.Source{MediaType: types.MediaPNG, Digest: inline.Digest}

	key := func(src types.Source, meta ...types.ImageMeta) string {
		p := types.ImagePart{Source: src}
		if len(meta) > 0 {
			p.ImageMeta = meta[0]
		}
		return Key("m", []types.Message{types.UserMsg(types.Text("q"), p)}, nil, nil)
	}
	base := key(inline)
	for name, src := range map[string]types.Source{"uri": withURI, "file": withFile, "reupload": reuploaded, "no digest": noDigest, "elided": elided} {
		if key(src) != base {
			t.Errorf("%s: key differs for the same media", name)
		}
	}
	if key(types.Bytes(types.MediaPNG, []byte("other"))) == base {
		t.Error("different media share a key")
	}
	if key(inline, types.ImageMeta{Detail: "high"}) == base {
		t.Error("detail does not change the key")
	}
	// Without a digest, the URI and then the vendor upload name the media.
	if key(types.URL("https://example.com/a.png", types.MediaPNG)) == key(types.URL("https://example.com/b.png", types.MediaPNG)) {
		t.Error("URIs without digests share a key")
	}
	nested := func(src types.Source) string {
		tr := types.ToolResultPart{CallID: "c", Parts: []types.ToolOutputPart{types.Text("r"), types.ImagePart{Source: src}}}
		return Key("m", []types.Message{types.UserMsg(tr)}, nil, nil)
	}
	if nested(inline) != nested(withURI) || nested(inline) == nested(types.Bytes(types.MediaPNG, []byte("other"))) {
		t.Error("tool result media are not hashed by digest")
	}
}

func TestKeyCoversTheConversionReport(t *testing.T) {
	msgs := []types.Message{types.UserMsg(types.Text("q"), types.Image(types.Bytes(types.MediaPNG, []byte("x"))))}
	if Key("m", msgs, nil, nil) != KeyWithConversions("m", msgs, nil, nil, "") {
		t.Fatal("Key is not KeyWithConversions without a report")
	}
	if KeyWithConversions("m", msgs, nil, nil, "aaa") == KeyWithConversions("m", msgs, nil, nil, "bbb") ||
		KeyWithConversions("m", msgs, nil, nil, "aaa") == Key("m", msgs, nil, nil) {
		t.Fatal("the report hash does not change the key")
	}
}

func TestKeyCoversEveryPartKind(t *testing.T) {
	base := Key("m", []types.Message{types.AssistantMsg(types.Text("a"))}, nil, nil)
	for _, p := range []types.AssistantPart{
		types.RefusalPart{Text: "no"},
		types.ThinkingPart{Text: "t", Signature: "s"},
		types.ToolCallPart{ID: "c", Name: "n", Arguments: map[string]any{"a": 1}},
		types.CitationPart{Citation: types.Citation{URI: "https://x"}},
	} {
		if Key("m", []types.Message{types.AssistantMsg(types.Text("a"), p)}, nil, nil) == base {
			t.Errorf("%s does not change the key", p.Kind())
		}
	}
	// Metadata never reaches the provider.
	if Key("m", []types.Message{types.UserMsg(types.Text("a"), types.FeedbackPart{})}, nil, nil) !=
		Key("m", []types.Message{types.UserMsg(types.Text("a"))}, nil, nil) {
		t.Error("metadata changes the key")
	}
}

// The derivation is frozen: a change to it must raise KeyVersion.
func TestKeyGolden(t *testing.T) {
	got := Key("m", []types.Message{types.SystemMsg(types.Text("s")), types.UserMsg(types.Text("hi"))}, nil, nil)
	const want = "d2dc826e0827f9616de1f2ed1d18e6dffefe739c0342099e9a11e01bf6316cd6"
	if got != want {
		t.Fatalf("key = %s, want %s", got, want)
	}
}

// planner stands in for a conversion decorator under the cache.
type planner struct {
	mu      sync.Mutex
	planned types.ConversionReport
	served  *types.ConversionReport
	reject  error
	calls   int
}

func (p *planner) PlanConversions(context.Context, types.Request) (types.ConversionReport, types.ConversionEstimate, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.planned, types.ConversionEstimate{}, p.reject
}

func (p *planner) Stream(context.Context, types.Request) (<-chan types.Delta, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.reject != nil {
		return nil, p.reject
	}
	ch := make(chan types.Delta, 6)
	if p.served != nil {
		ch <- types.ConversionDelta{Report: *p.served}
	}
	ch <- types.PartStart{Index: 0, Kind: types.KindText}
	ch <- types.PartDelta{Index: 0, Text: "answer"}
	ch <- types.PartEnd{Index: 0}
	ch <- types.UsageDelta{PromptTokens: 1}
	close(ch)
	return ch, nil
}

func report(action string) types.ConversionReport {
	r := types.ConversionReport{Offering: "o", Decisions: []types.ConversionDecision{{Kind: types.KindImage, Digest: "d", Action: action}}}
	r.Hash = r.ComputeHash()
	return r
}

func TestConvertedViewAndOriginalNeverShareAnEntry(t *testing.T) {
	inner := &planner{}
	p := must.Get(New(inner, Config{Cache: memcache.New[CachedResponse]()}))
	msgs := []types.Message{types.UserMsg(types.Text("q"), types.Image(types.Bytes(types.MediaPNG, []byte("x"))))}
	call := func() {
		t.Helper()
		ch, err := p.Stream(context.Background(), types.Request{Messages: msgs})
		if err != nil {
			t.Fatal(err)
		}
		collect(ch)
	}
	described := report(types.DecisionDescribed)
	inner.planned, inner.served = described, &described
	call()
	call()
	if inner.calls != 1 {
		t.Fatalf("calls = %d, want the described view served from the cache", inner.calls)
	}
	// The same parts on a model that takes the image natively.
	inner.planned, inner.served = report(types.DecisionNative), nil
	call()
	if inner.calls != 2 {
		t.Fatal("the original replayed the converted view's response")
	}
	call()
	if inner.calls != 2 {
		t.Fatal("the original view was not cached")
	}
}

func TestServedViewMustMatchThePlannedOne(t *testing.T) {
	msgs := []types.Message{types.UserMsg(types.Image(types.Bytes(types.MediaPNG, []byte("x"))))}
	described, omitted := report(types.DecisionDescribed), report(types.DecisionOmitted)
	for name, tc := range map[string]struct {
		planned types.ConversionReport
		served  *types.ConversionReport
	}{
		"fell back to another action":      {described, &omitted},
		"planned a conversion, none ran":   {described, nil},
		"planned native, a conversion ran": {report(types.DecisionNative), &described},
	} {
		t.Run(name, func(t *testing.T) {
			inner := &planner{planned: tc.planned, served: tc.served}
			p := must.Get(New(inner, Config{Cache: memcache.New[CachedResponse]()}))
			for range 2 {
				ch, err := p.Stream(context.Background(), types.Request{Messages: msgs})
				if err != nil {
					t.Fatal(err)
				}
				collect(ch)
			}
			if inner.calls != 2 {
				t.Fatalf("calls = %d, want a response for another view left uncached", inner.calls)
			}
		})
	}
}

func TestRejectedPlanPassesThrough(t *testing.T) {
	boom := errors.New("rejected")
	inner := &planner{reject: boom}
	p := must.Get(New(inner, Config{Cache: memcache.New[CachedResponse]()}))
	if _, err := p.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("q"))}}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the inner provider's rejection", err)
	}
}

// An entry a v0.3x release wrote (codec version 1, under a key version 1
// derivation) is never replayed: its key is not derived any more, and its
// value does not decode, so a shared store misses once and the response is
// written again under the version 2 key.
func TestV1EntriesMissOnce(t *testing.T) {
	const v1Entry = `{"v":1,"deltas":[{"v":1,"kind":"text.start","data":{}},{"v":1,"kind":"text.delta","data":{"content":"stale"}},{"v":1,"kind":"text.end","data":{}}],"usage":{"v":1,"kind":"usage","data":{"prompt_tokens":1}}}`
	if _, err := DecodeResponse([]byte(v1Entry)); !errors.Is(err, ErrResponseCodec) {
		t.Fatalf("err = %v, want a v1 entry refused", err)
	}
	store := memcache.New[[]byte]()
	msgs := []types.Message{types.UserMsg(types.Text("hi"))}
	inner := &planner{}
	p := must.Get(New(inner, Config{Cache: BytesCache(store), ScopeKey: "s", ConfigKey: "c"}))
	// Plant the old value under the key this request now derives: even a
	// collision with a stale value is a miss, never a replay.
	parts, _ := json.Marshal([]string{p.identity, ""})
	_ = store.Set(context.Background(), ":"+Key(string(parts), msgs, nil, nil), []byte(v1Entry), 0)
	for range 2 {
		ch, err := p.Stream(context.Background(), types.Request{Messages: msgs})
		if err != nil {
			t.Fatal(err)
		}
		if got := text(collect(ch)); got != "answer" {
			t.Fatalf("got %q, want the fresh answer", got)
		}
	}
	if inner.calls != 1 {
		t.Fatalf("calls = %d, want one miss and then a hit on the rewritten entry", inner.calls)
	}
}
