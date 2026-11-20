package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestAnthropicLogin(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "configs"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"default", "work"} {
		if err := os.WriteFile(filepath.Join(dir, "configs", name+".json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	empty, xdg, home := t.TempDir(), t.TempDir(), t.TempDir()
	for _, d := range []string{filepath.Join(xdg, "anthropic", "configs"), filepath.Join(home, ".config", "anthropic", "configs")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "default.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tt := range []struct {
		name string
		vars map[string]string
		want bool
	}{
		{"nothing set", nil, false},
		{"auth token", map[string]string{"ANTHROPIC_AUTH_TOKEN": "t"}, true},
		{"federation", map[string]string{"ANTHROPIC_FEDERATION_RULE_ID": "r", "ANTHROPIC_ORGANIZATION_ID": "o", "ANTHROPIC_IDENTITY_TOKEN_FILE": "/f"}, true},
		{"federation without a token", map[string]string{"ANTHROPIC_FEDERATION_RULE_ID": "r", "ANTHROPIC_ORGANIZATION_ID": "o"}, false},
		{"default profile", map[string]string{"ANTHROPIC_CONFIG_DIR": dir}, true},
		{"named profile", map[string]string{"ANTHROPIC_CONFIG_DIR": dir, "ANTHROPIC_PROFILE": "work"}, true},
		{"missing named profile", map[string]string{"ANTHROPIC_CONFIG_DIR": dir, "ANTHROPIC_PROFILE": "gone"}, false},
		{"profile name with a path", map[string]string{"ANTHROPIC_CONFIG_DIR": dir, "ANTHROPIC_PROFILE": "../configs/work"}, false},
		{"directory without profiles", map[string]string{"ANTHROPIC_CONFIG_DIR": empty}, false},
		{"xdg", map[string]string{"XDG_CONFIG_HOME": xdg, "HOME": empty}, true},
		{"home", map[string]string{"HOME": home}, true},
	} {
		if got := AnthropicLogin(env(tt.vars)); got != tt.want {
			t.Errorf("%s: AnthropicLogin = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestBuildAnthropicWithLogin checks that an Anthropic adapter built without
// a key leaves authentication to the SDK, which sends the auth token as a
// bearer credential, and that a login is never used for a gateway.
func TestBuildAnthropicWithLogin(t *testing.T) {
	const reply = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-haiku-5-5\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	var auth, apiKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		auth, apiKey = r.Header.Get("Authorization"), r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(server.Close)
	// The SDK reads the process environment, so the test sets it for real.
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "login-token")
	t.Setenv("ANTHROPIC_BASE_URL", server.URL)
	t.Setenv("ANTHROPIC_CONFIG_DIR", t.TempDir())

	p, err := Build(context.Background(), Config{Provider: Anthropic, Model: "claude-haiku-5-5"})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := p.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("hi"))}})
	if err != nil {
		t.Fatal(err)
	}
	for d := range ch {
		if e, ok := d.(types.ErrorDelta); ok {
			t.Fatal(e.Error)
		}
	}
	if auth != "Bearer login-token" || apiKey != "" {
		t.Errorf("authorization = %q, x-api-key = %q; want the login token and no key", auth, apiKey)
	}

	if _, err := Build(context.Background(), Config{Provider: Anthropic, Model: "claude-haiku-5-5", BaseURL: "https://gateway.example"}); !types.IsAuth(err) {
		t.Errorf("gateway without a key: %v, want an auth error", err)
	}
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	if _, err := Build(context.Background(), Config{Provider: Anthropic, Model: "claude-haiku-5-5"}); !types.IsAuth(err) {
		t.Errorf("no key and no login: %v, want an auth error", err)
	}
}
