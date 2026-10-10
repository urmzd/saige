package google

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/types"
	"google.golang.org/genai"
)

func TestContextCacheBindsExactPrefixAndTools(t *testing.T) {
	a := &Adapter{model: "gemini-2.5-flash"}
	prefix := []types.Message{types.SystemMsg(types.Text("rules")), types.UserMsg(types.Text("reference"))}
	tools := []types.ToolDef{{Name: "read", Parameters: types.ParameterSchema{Type: "object"}}}
	contents, config, _ := a.buildRequest(prefix, 0, tools)
	fingerprint, err := cacheFingerprint(contents, config)
	if err != nil {
		t.Fatal(err)
	}
	cache := ContextCache{Name: "cachedContents/test", Model: a.model, PrefixCount: 2, Fingerprint: fingerprint, ExpiresAt: time.Now().Add(time.Hour)}
	a.contextCache = &cache
	request := append(append([]types.Message(nil), prefix...), types.UserMsg(types.Text("question")))
	remainder, bound, err := a.cachedRequest(request, tools)
	if err != nil || len(remainder) != 1 || bound.CachedContent != cache.Name || bound.SystemInstruction != nil || len(bound.Tools) != 0 {
		t.Fatal("cache prefix duplicated or binding failed", err)
	}
	if _, _, err = a.cachedRequest(request, nil); err == nil {
		t.Fatal("changed tools accepted")
	}
	changed := append([]types.Message(nil), request...)
	changed[0] = types.SystemMsg(types.Text("new rules"))
	if _, _, err = a.cachedRequest(changed, tools); err == nil {
		t.Fatal("changed prefix accepted")
	}
	other := *a
	other.model = "other"
	if _, _, err = other.cachedRequest(request, tools); err == nil {
		t.Fatal("cross-model cache accepted")
	}
	cache.ExpiresAt = time.Now().Add(-time.Hour)
	if _, _, err = a.cachedRequest(request, tools); err == nil {
		t.Fatal("expired cache accepted")
	}
}

// A handle created without a tool choice keeps the fingerprint of the
// three-field encoding, so existing handles stay valid.
func TestContextCacheFingerprintWithoutToolConfig(t *testing.T) {
	a := &Adapter{model: "gemini-2.5-flash"}
	prefix := []types.Message{types.SystemMsg(types.Text("rules")), types.UserMsg(types.Text("reference"))}
	tools := []types.ToolDef{{Name: "read", Parameters: types.ParameterSchema{Type: "object"}}}
	contents, config, _ := a.buildRequest(prefix, 0, tools)
	if config.ToolConfig != nil {
		t.Fatalf("ToolConfig = %+v, want nil without a tool choice", config.ToolConfig)
	}
	got, err := cacheFingerprint(contents, config)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(struct {
		Contents []*genai.Content
		System   *genai.Content
		Tools    []*genai.Tool
	}{contents, config.SystemInstruction, config.Tools})
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(raw)
	if want := hex.EncodeToString(hash[:]); got != want {
		t.Fatalf("fingerprint = %s, want %s", got, want)
	}
}

func TestContextCacheToolChoice(t *testing.T) {
	prefix := []types.Message{types.SystemMsg(types.Text("rules")), types.UserMsg(types.Text("reference"))}
	tools := []types.ToolDef{{Name: "read", Parameters: types.ParameterSchema{Type: "object"}}}
	request := append(append([]types.Message(nil), prefix...), types.UserMsg(types.Text("question")))
	required := types.ToolChoice{Mode: types.ToolChoiceRequired}

	var created map[string]any
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(req.Body)
		_ = json.Unmarshal(raw, &created)
		body := `{"name":"cachedContents/test","expireTime":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`
		return &http.Response{StatusCode: http.StatusOK, Request: req,
			Header: http.Header{"Content-Type": []string{"application/json"}},
			Body:   io.NopCloser(strings.NewReader(body))}, nil
	})
	a, err := New(context.Background(), Config{APIKey: "k", Model: "gemini-2.5-flash"}, WithHTTPClient(&http.Client{Transport: transport}), WithToolChoice(required))
	if err != nil {
		t.Fatal(err)
	}
	cache, err := a.CreateContextCache(context.Background(), prefix, tools, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := created["toolConfig"]; !ok {
		t.Fatalf("create body = %v, want toolConfig", created)
	}

	for _, tc := range []struct {
		name   string
		choice *types.ToolChoice
		ok     bool
	}{
		{"same choice", &required, true},
		{"changed choice", &types.ToolChoice{Mode: types.ToolChoiceNone}, false},
		{"no choice", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := []Option{WithContextCache(cache)}
			if tc.choice != nil {
				opts = append(opts, WithToolChoice(*tc.choice))
			}
			b, err := New(context.Background(), Config{APIKey: "k", Model: "gemini-2.5-flash"}, opts...)
			if err != nil {
				t.Fatal(err)
			}
			_, bound, err := b.cachedRequest(request, tools)
			if (err == nil) != tc.ok {
				t.Fatalf("cachedRequest err = %v, want ok=%v", err, tc.ok)
			}
			if tc.ok && (bound.ToolConfig != nil || len(bound.Tools) != 0) {
				t.Fatal("tool config must stay on the cached resource")
			}
		})
	}
}

func TestRouteLocksReportBoundContextCache(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cache *ContextCache
		want  int
	}{
		{"unbound", nil, 0},
		{"bound", &ContextCache{Name: "cachedContents/x", Model: "gemini-2.5-flash"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &Adapter{model: "gemini-2.5-flash", contextCache: tc.cache}
			if got := a.RouteLocks(); len(got) != tc.want || (tc.want == 1 && got[0] != "context_cache") {
				t.Fatalf("RouteLocks = %v", got)
			}
		})
	}
}
