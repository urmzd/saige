package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/urmzd/saige/eval"
	"github.com/urmzd/saige/eval/harness"
	"github.com/urmzd/saige/eval/store"
	"github.com/urmzd/saige/eval/store/filestore"
)

func TestEvalRunRecordsToStore(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "doc"}}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
		})
	}))
	defer server.Close()

	corpus := t.TempDir()
	for name, body := range map[string]string{"system.md": "sys", "turn-0.md": "synth", "turn-1.md": "edit"} {
		path := filepath.Join(corpus, "001", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	storeDir := filepath.Join(t.TempDir(), "results")

	tests := []struct {
		name      string
		args      []string
		wantSuite string
	}{
		{"default suite", nil, "harness"},
		{"named suite", []string{"--suite", "docs"}, "docs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newEvalRunCmd(context.Background())
			cmd.SetArgs(append([]string{
				"--experiments-dir", corpus, "--api-base", server.URL, "--api-key", "k",
				"--model", "mock", "--flows", "base", "--force", "--store", storeDir,
			}, tt.args...))
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			s, err := filestore.Open(storeDir)
			if err != nil {
				t.Fatal(err)
			}
			runs, err := s.ListRuns(context.Background(), store.RunFilter{Suite: tt.wantSuite})
			if err != nil || len(runs) != 1 {
				t.Fatalf("runs of %s = %v, %v; want one", tt.wantSuite, runs, err)
			}
			run := runs[0]
			if run.Status != eval.RunSucceeded || run.Units != 2 || len(run.Provenance.Models) != 1 || run.Provenance.Models[0] != "mock" {
				t.Fatalf("run = %+v", run)
			}
		})
	}
}

// evalKeyVars are every variable the eval key resolution reads.
var evalKeyVars = []string{
	"SAIGE_EVAL_API_KEY", "SAIGE_EVAL_API_BASE", "OPENAI_BASE_URL", "OPENAI_API_KEY", "GEMINI_API_KEY",
	"GOOGLE_API_KEY", "GROQ_API_KEY", "OPENROUTER_API_KEY", "MISTRAL_API_KEY", "GITHUB_TOKEN", "TEST_EVAL_KEY", "ANTHROPIC_API_KEY",
	"SAIGE_EVAL_TEST_KEY", "AWS_SECRET_ACCESS_KEY",
}

func clearEvalKeys(t *testing.T) {
	t.Helper()
	for _, k := range evalKeyVars {
		t.Setenv(k, "")
	}
}

func TestEvalAPIKeyNeverCrossesHosts(t *testing.T) {
	tests := []struct {
		name       string
		env        map[string]string
		explicit   string
		base       string
		fromFile   bool // base came from the manifest, not --api-base
		keyEnv     string
		wantKey    string
		wantSource string
		wantErr    string
	}{
		{name: "openai key for the default base", env: map[string]string{"OPENAI_API_KEY": "sk-openai"}, wantKey: "sk-openai", wantSource: "OPENAI_API_KEY"},
		{name: "github token is not sent to openai", env: map[string]string{"GITHUB_TOKEN": "ghp"}, wantErr: "OPENAI_API_KEY"},
		{name: "gemini key is not sent to openai", env: map[string]string{"GEMINI_API_KEY": "g", "GROQ_API_KEY": "q"}, wantErr: "api.openai.com"},
		{name: "groq key for the groq host", env: map[string]string{"GROQ_API_KEY": "q", "OPENAI_API_KEY": "sk"}, base: "https://api.groq.com/openai/v1", wantKey: "q", wantSource: "GROQ_API_KEY"},
		{name: "gemini key for the gemini host", env: map[string]string{"GOOGLE_API_KEY": "g2"}, base: "https://generativelanguage.googleapis.com/v1beta/openai", wantKey: "g2", wantSource: "GOOGLE_API_KEY"},
		{name: "github token for github models", env: map[string]string{"GITHUB_TOKEN": "ghp"}, base: "https://models.github.ai/inference", wantKey: "ghp", wantSource: "GITHUB_TOKEN"},
		{name: "unknown host gets no vendor key", env: map[string]string{"OPENAI_API_KEY": "sk", "GITHUB_TOKEN": "ghp"}, base: "https://llm.example.com/v1", wantErr: "SAIGE_EVAL_API_KEY"},
		{name: "eval key goes to the configured base", env: map[string]string{"SAIGE_EVAL_API_KEY": "e", "OPENAI_API_KEY": "sk"}, base: "https://llm.example.com/v1", wantKey: "e", wantSource: "SAIGE_EVAL_API_KEY"},
		{name: "openai base url pairs with the openai key", env: map[string]string{"OPENAI_BASE_URL": "https://proxy.example.com/v1", "OPENAI_API_KEY": "sk"}, wantKey: "sk", wantSource: "OPENAI_API_KEY"},
		{name: "explicit key wins", env: map[string]string{"OPENAI_API_KEY": "sk"}, explicit: "x", wantKey: "x", wantSource: "--api-key"},
		{name: "manifest key variable", env: map[string]string{"SAIGE_EVAL_TEST_KEY": "t", "OPENAI_API_KEY": "sk"}, keyEnv: "SAIGE_EVAL_TEST_KEY", wantKey: "t", wantSource: "SAIGE_EVAL_TEST_KEY"},
		{name: "manifest key variable unset", env: map[string]string{"OPENAI_API_KEY": "sk"}, keyEnv: "SAIGE_EVAL_TEST_KEY", wantErr: "$SAIGE_EVAL_TEST_KEY"},
		{name: "manifest cannot name an unrelated variable", env: map[string]string{"TEST_EVAL_KEY": "t"}, keyEnv: "TEST_EVAL_KEY", wantErr: "SAIGE_EVAL_"},
		{name: "manifest cannot name an environment secret", env: map[string]string{"AWS_SECRET_ACCESS_KEY": "aws"}, base: "https://x.example", keyEnv: "AWS_SECRET_ACCESS_KEY", wantErr: "SAIGE_EVAL_"},
		{name: "manifest cannot send github token to another host", env: map[string]string{"GITHUB_TOKEN": "ghp"}, base: "https://x.example", keyEnv: "GITHUB_TOKEN", wantErr: "--api-key"},
		{name: "manifest cannot send openai key to groq", env: map[string]string{"OPENAI_API_KEY": "sk"}, base: "https://api.groq.com/openai/v1", keyEnv: "OPENAI_API_KEY", wantErr: "SAIGE_EVAL_API_KEY"},
		{name: "manifest cannot send anthropic key to openai", env: map[string]string{"ANTHROPIC_API_KEY": "a"}, keyEnv: "ANTHROPIC_API_KEY", wantErr: "api.openai.com"},
		{name: "manifest vendor key for its own host", env: map[string]string{"GITHUB_TOKEN": "ghp"}, base: "https://models.github.ai/inference", keyEnv: "GITHUB_TOKEN", wantKey: "ghp", wantSource: "GITHUB_TOKEN"},
		{name: "manifest openai key with openai base url", env: map[string]string{"OPENAI_BASE_URL": "https://proxy.example.com/v1", "OPENAI_API_KEY": "sk"}, keyEnv: "OPENAI_API_KEY", wantKey: "sk", wantSource: "OPENAI_API_KEY"},
		{name: "manifest eval variable goes to a host the caller chose", env: map[string]string{"SAIGE_EVAL_TEST_KEY": "t"}, base: "https://x.example", keyEnv: "SAIGE_EVAL_TEST_KEY", wantKey: "t", wantSource: "SAIGE_EVAL_TEST_KEY"},
		{name: "eval key is not sent to a manifest base", env: map[string]string{"SAIGE_EVAL_API_KEY": "e"}, base: "https://evil.example/v1", fromFile: true, wantErr: "--api-base"},
		{name: "manifest base with its own eval variable is refused", env: map[string]string{"SAIGE_EVAL_TEST_KEY": "t"}, base: "https://evil.example/v1", fromFile: true, keyEnv: "SAIGE_EVAL_TEST_KEY", wantErr: "--api-base"},
		{name: "manifest base cannot take an environment secret", env: map[string]string{"AWS_SECRET_ACCESS_KEY": "aws"}, base: "https://evil.example/v1", fromFile: true, keyEnv: "AWS_SECRET_ACCESS_KEY", wantErr: "--api-base"},
		{name: "manifest base gets its host's own key", env: map[string]string{"GROQ_API_KEY": "q", "SAIGE_EVAL_API_KEY": "e"}, base: "https://api.groq.com/openai/v1", fromFile: true, wantKey: "q", wantSource: "GROQ_API_KEY"},
		{name: "manifest base with explicit key", env: map[string]string{"SAIGE_EVAL_API_KEY": "e"}, base: "https://llm.example/v1", fromFile: true, explicit: "x", wantKey: "x", wantSource: "--api-key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEvalKeys(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			base, source := evalBaseFor(evalClientConfig{baseURL: tt.base, baseURLFromFlag: !tt.fromFile})
			key, keySource, err := evalAPIKey(base, source, tt.explicit, tt.keyEnv)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || key != "" {
					t.Fatalf("key=%q err=%v, want no key and an error naming %s", key, err, tt.wantErr)
				}
				return
			}
			if err != nil || key != tt.wantKey || keySource != tt.wantSource {
				t.Fatalf("key=%q source=%q err=%v, want %q from %s", key, keySource, err, tt.wantKey, tt.wantSource)
			}
		})
	}
}

// recordingServer answers chat requests and records their bearer tokens.
func recordingServer(t *testing.T, tokens *[]string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*tokens = append(*tokens, r.Header.Get("Authorization"))
		mu.Unlock()
		var req struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if n := len(req.Messages); n > 0 && strings.Contains(req.Messages[n-1].Content, "FAIL") {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "doc"}}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
		})
	}))
	t.Cleanup(server.Close)
	return server
}

func writeEvalCorpus(t *testing.T, dir string, turns ...string) {
	t.Helper()
	files := map[string]string{"system.md": "sys"}
	for i, turn := range turns {
		files[fmt.Sprintf("turn-%d.md", i)] = turn
	}
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEvalRunDoesNotLeakUnrelatedKeys(t *testing.T) {
	clearEvalKeys(t)
	t.Setenv("GITHUB_TOKEN", "ghp-secret")
	var tokens []string
	server := recordingServer(t, &tokens)
	corpus := t.TempDir()
	writeEvalCorpus(t, filepath.Join(corpus, "001"), "synth")

	cmd := newEvalRunCmd(context.Background())
	cmd.SetArgs([]string{"--experiments-dir", corpus, "--api-base", server.URL, "--model", "mock", "--flows", "base"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "no API key") {
		t.Fatalf("err = %v, want a missing key error", err)
	}
	if len(tokens) != 0 {
		t.Errorf("requests were sent with %v", tokens)
	}
}

// TestEvalRunManifestBaseGetsNoCallerKeys checks that a manifest that points
// base_url at its own host cannot collect the caller's eval key or an
// arbitrary environment secret: the run fails before any request.
func TestEvalRunManifestBaseGetsNoCallerKeys(t *testing.T) {
	tests := []struct {
		name    string
		keyEnv  string
		wantErr string
	}{
		{name: "eval key", wantErr: "--api-base"},
		{name: "named environment secret", keyEnv: "AWS_SECRET_ACCESS_KEY", wantErr: "--api-base"},
		{name: "named eval variable", keyEnv: "SAIGE_EVAL_TEST_KEY", wantErr: "--api-base"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEvalKeys(t)
			t.Setenv("SAIGE_EVAL_API_KEY", "eval-secret")
			t.Setenv("SAIGE_EVAL_TEST_KEY", "eval-test-secret")
			t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-secret")
			var tokens []string
			server := recordingServer(t, &tokens)
			dir := t.TempDir()
			writeEvalCorpus(t, filepath.Join(dir, "evals", "001"), "synth")
			subject := fmt.Sprintf(`{"model": "mock", "base_url": %q}`, server.URL)
			if tt.keyEnv != "" {
				subject = fmt.Sprintf(`{"model": "mock", "base_url": %q, "api_key_env": %q}`, server.URL, tt.keyEnv)
			}
			manifest := fmt.Sprintf(`{"version": 1, "name": "x", "corpus": "evals", "flows": ["base"], "subject": %s}`, subject)
			manifestPath := filepath.Join(dir, "saige.eval.json")
			if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := newEvalRunCmd(context.Background())
			cmd.SetArgs([]string{"--manifest", manifestPath})
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want an error naming %s", err, tt.wantErr)
			}
			if len(tokens) != 0 {
				t.Errorf("requests were sent with %v", tokens)
			}
		})
	}
}

func TestEvalRunManifest(t *testing.T) {
	clearEvalKeys(t)
	t.Setenv("SAIGE_EVAL_TEST_KEY", "manifest-key")
	var tokens []string
	server := recordingServer(t, &tokens)

	tests := []struct {
		name       string
		edit       string
		args       []string
		wantGate   bool // true when the gate should fail
		wantOutcom eval.Outcome
	}{
		{"passing gate", "edit", nil, false, eval.OutcomePassed},
		{"failing gate", "FAIL edit", nil, true, eval.OutcomeFailed},
		{"flag assertion", "edit", []string{"--assert", "aggregate:latency_ms<=-1"}, true, eval.OutcomeFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeEvalCorpus(t, filepath.Join(dir, "evals", "001"), "synth", tt.edit)
			manifest := fmt.Sprintf(`{
				"version": 1, "name": "docs", "corpus": "evals", "flows": ["base"], "store": "results",
				"subject": {"model": "mock", "base_url": %q, "api_key_env": "SAIGE_EVAL_TEST_KEY"},
				"policy": {"concurrency": 2},
				"assert": [{"metric": "turn_succeeded", "op": ">=", "threshold": 1}]
			}`, server.URL)
			manifestPath := filepath.Join(dir, "saige.eval.json")
			if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
				t.Fatal(err)
			}
			tokens = nil
			cmd := newEvalRunCmd(context.Background())
			// --api-base confirms the manifest's host, so the key it names
			// may be sent there.
			cmd.SetArgs(append([]string{"--manifest", manifestPath, "--api-base", server.URL}, tt.args...))
			err := cmd.Execute()
			if got := errors.Is(err, harness.ErrAssertionsFailed); got != tt.wantGate {
				t.Fatalf("err = %v, want gate failure %v", err, tt.wantGate)
			}
			for _, tok := range tokens {
				if tok != "Bearer manifest-key" {
					t.Errorf("sent token %q", tok)
				}
			}
			s, err := filestore.Open(filepath.Join(dir, "results"))
			if err != nil {
				t.Fatal(err)
			}
			runs, err := s.ListRuns(context.Background(), store.RunFilter{Suite: "docs"})
			if err != nil || len(runs) != 1 {
				t.Fatalf("runs = %v, %v", runs, err)
			}
			if runs[0].Outcome != tt.wantOutcom {
				t.Errorf("outcome = %s, want %s", runs[0].Outcome, tt.wantOutcom)
			}
		})
	}
}

func TestEvalRunManifestRejectsUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "saige.eval.json")
	if err := os.WriteFile(path, []byte(`{"version": 1, "corpus": "evals", "concurency": 4}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := newEvalRunCmd(context.Background())
	cmd.SetArgs([]string{"--manifest", path})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), `/concurency: error: unknown key "concurency"`) {
		t.Fatalf("err = %v", err)
	}
	// Invalid input exits 2, apart from a failed run's 1.
	var errW strings.Builder
	if code := run(context.Background(), []string{"eval", "run", "--manifest", path}, &errW); code != exitInvalid {
		t.Fatalf("exit code = %d, want %d (stderr %q)", code, exitInvalid, errW.String())
	}
}

func TestEvalRunDryRun(t *testing.T) {
	var tokens []string
	server := recordingServer(t, &tokens)
	tests := []struct {
		name    string
		args    []string
		env     map[string]string
		want    []string
		wantErr string
	}{
		{
			name: "plan without a key",
			args: []string{"--api-base", server.URL, "--model", "mock"},
			want: []string{"mock", "$SAIGE_EVAL_API_KEY (NOT SET)", "1 of 1 experiments would run, 3 chat calls"},
		},
		{
			name: "uncatalogued openai model is refused",
			args: []string{"--model", "gtp-4o"},
			env:  map[string]string{"OPENAI_API_KEY": "sk"},
			// The default base is the OpenAI API, so the catalog applies.
			wantErr: "--allow-unknown-model",
		},
		{
			name: "uncatalogued model allowed",
			args: []string{"--model", "gtp-4o", "--allow-unknown-model"},
			env:  map[string]string{"OPENAI_API_KEY": "sk"},
			want: []string{"$OPENAI_API_KEY (set)"},
		},
		{
			name:    "resume needs a store",
			args:    []string{"--api-base", server.URL, "--model", "mock", "--resume", "r1"},
			wantErr: "needs a results store",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEvalKeys(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			corpus := t.TempDir()
			writeEvalCorpus(t, filepath.Join(corpus, "001"), "synth", "edit")
			var out bytes.Buffer
			cmd := newEvalRunCmd(context.Background())
			cmd.SetOut(&out)
			cmd.SetArgs(append([]string{"--experiments-dir", corpus, "--dry-run"}, tt.args...))
			err := cmd.Execute()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tt.want {
				if !strings.Contains(out.String(), w) {
					t.Errorf("output lacks %q:\n%s", w, out.String())
				}
			}
			if len(tokens) != 0 {
				t.Errorf("a dry run sent %d requests", len(tokens))
			}
			if _, err := os.Stat(filepath.Join(corpus, "001", "outputs")); err == nil {
				t.Error("a dry run wrote outputs")
			}
		})
	}
}

func TestEvalRunProviderDryRun(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	tests := []struct {
		name    string
		model   string
		want    string
		wantErr string
	}{
		{"catalogued model", "claude-haiku-5-5", "$ANTHROPIC_API_KEY (NOT SET)", ""},
		{"unknown model", "claude-nonexistent", "", "not in the anthropic catalog"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeEvalCorpus(t, filepath.Join(dir, "evals", "001"), "synth")
			path := filepath.Join(dir, "saige.eval.json")
			body := fmt.Sprintf(`{"version": 1, "corpus": "evals", "subject": {"provider": "anthropic", "model": %q}}`, tt.model)
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			cmd := newEvalRunCmd(context.Background())
			cmd.SetOut(&out)
			cmd.SetArgs([]string{"--manifest", path, "--dry-run"})
			err := cmd.Execute()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), tt.want) || !strings.Contains(out.String(), "anthropic") {
				t.Errorf("output:\n%s", out.String())
			}
		})
	}
}

func TestEvalOpenAIProviderBaseURL(t *testing.T) {
	clearEvalKeys(t)
	t.Setenv("OPENAI_API_KEY", "sk")
	tests := []struct {
		name     string
		baseURL  string
		fromFlag bool
		wantErr  bool
	}{
		{"manifest base on another host is refused", "https://x.example/v1", false, true},
		{"manifest base on the openai api", "https://api.openai.com/v1", false, false},
		{"command line base on another host", "https://x.example/v1", true, false},
		{"no base", "", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := evalClientConfig{provider: providerOpenAI, model: "gpt-6-luna", baseURL: tt.baseURL, baseURLFromFlag: tt.fromFlag}
			_, _, err := buildEvalClient(context.Background(), cfg, true)
			if got := err != nil; got != tt.wantErr {
				t.Fatalf("err = %v, want error %v", err, tt.wantErr)
			}
			if tt.wantErr && !strings.Contains(err.Error(), "--api-base") {
				t.Errorf("err = %v, want it to name --api-base", err)
			}
		})
	}
}

func TestParseEvalAssertion(t *testing.T) {
	tests := []struct {
		spec    string
		want    eval.Assertion
		wantErr bool
	}{
		{"turn_succeeded>=1", eval.Assertion{Metric: "turn_succeeded", Op: eval.GTE, Threshold: 1}, false},
		{" latency_ms <= 2000 ", eval.Assertion{Metric: "latency_ms", Op: eval.LTE, Threshold: 2000}, false},
		{"aggregate:turn_succeeded==0.5", eval.Assertion{Metric: "turn_succeeded", Op: eval.EQ, Threshold: 0.5, Scope: eval.OnAggregate}, false},
		{"turn_succeeded>1", eval.Assertion{}, true},
		{"turn_succeeded>=high", eval.Assertion{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			got, err := parseEvalAssertion(tt.spec)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v", err)
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestEvalInitThenValidate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "evals")
	bare := t.TempDir()
	initCmd := newEvalInitCmd()
	initCmd.SetArgs([]string{dir})
	if err := initCmd.Execute(); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		mutate  func(t *testing.T)
		path    string
		want    string
		wantErr bool
	}{
		{"scaffold is valid", func(*testing.T) {}, dir, "ok (0 warnings)", false},
		{"manifest file path", func(*testing.T) {}, filepath.Join(dir, "saige.eval.json"), "ok", false},
		{"corpus without a manifest", func(t *testing.T) {
			writeEvalCorpus(t, filepath.Join(bare, "001"), "synth")
		}, bare, "ok (0 warnings)", false},
		{"typo in manifest", func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(dir, "saige.eval.json"), []byte(`{"version": 1, "corpus": ".", "flows": ["bsae"]}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}, dir, `/flows/0: error: unknown flow "bsae"`, true},
		{"missing turn zero", func(t *testing.T) {
			if err := os.Remove(filepath.Join(dir, "001-example", "turn-0.md")); err != nil {
				t.Fatal(err)
			}
		}, dir, "missing turn-0.md", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.mutate(t)
			var out bytes.Buffer
			cmd := newEvalValidateCmd()
			cmd.SetOut(&out)
			cmd.SetArgs([]string{tt.path})
			err := cmd.Execute()
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v\n%s", err, out.String())
			}
			var rep reportedError
			if tt.wantErr && !errors.As(err, &rep) {
				t.Errorf("validation errors should be reported once, got %v", err)
			}
			if !strings.Contains(out.String(), tt.want) {
				t.Errorf("output lacks %q:\n%s", tt.want, out.String())
			}
		})
	}
}
