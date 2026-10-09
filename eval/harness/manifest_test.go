package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseManifest(t *testing.T) {
	tests := []struct {
		name string
		json string
		// want lists "severity pointer substring" for each expected issue.
		want   []string
		usable bool
	}{
		{
			name:   "minimal",
			json:   `{"version": 1, "corpus": "evals"}`,
			usable: true,
		},
		{
			name: "full",
			json: `{"version": 1, "name": "docs", "corpus": "evals", "flows": ["base"],
				"subject": {"provider": "openai-compatible", "model": "llama", "base_url": "https://api.groq.com/openai/v1", "api_key_env": "GROQ_API_KEY"},
				"policy": {"concurrency": 4, "continue_on_error": false},
				"assert": [{"metric": "turn_succeeded", "op": ">=", "threshold": 1, "where": {"variant": "base"}}],
				"store": "results"}`,
			usable: true,
		},
		{
			name:   "unknown keys at every level",
			json:   `{"version": 1, "corpus": "evals", "flow": ["base"], "subject": {"mdel": "x"}, "assert": [{"metric": "turn_succeeded", "op": ">=", "treshold": 1}]}`,
			want:   []string{`error /flow "flows"`, `error /subject/mdel "model"`, `error /assert/0/treshold "threshold"`},
			usable: false,
		},
		{
			name:   "missing version and corpus",
			json:   `{}`,
			want:   []string{"error /version required", "error /corpus required"},
			usable: false,
		},
		{
			name:   "future version",
			json:   `{"version": 2, "corpus": "evals"}`,
			want:   []string{"error /version unsupported"},
			usable: false,
		},
		{
			name:   "wrong type",
			json:   `{"version": 1, "corpus": "evals", "policy": {"concurrency": "four"}}`,
			want:   []string{"error /policy/concurrency expected int"},
			usable: false,
		},
		{
			name:   "syntax error has a line",
			json:   "{\n\"version\": 1,\n}",
			want:   []string{"error  line 3"},
			usable: false,
		},
		{
			name: "bad values",
			json: `{"version": 1, "corpus": "evals", "count": -1, "flows": ["base", "base", "fast"],
				"subject": {"provider": "openai", "api_key_env": "OPENAI_API_KEY", "base_url": "ftp://x"},
				"policy": {"concurrency": -2},
				"assert": [{"metric": "turn_succeedd", "op": ">", "threshold": 1, "scope": "all", "min_pass_rate": 2}]}`,
			want: []string{
				"error /count positive", "error /flows/1 twice", "error /flows/2 unknown flow",
				"error /subject/base_url http", "error /subject/api_key_env applies to the openai-compatible",
				"error /policy/concurrency positive",
				`error /assert/0/metric "turn_succeeded"`, "error /assert/0/op one of", "error /assert/0/scope scope must", "error /assert/0/min_pass_rate must be in",
			},
			usable: false,
		},
		{
			name:   "secret in api_key_env",
			json:   `{"version": 1, "corpus": "evals", "subject": {"api_key_env": "sk-abc123-secret"}}`,
			want:   []string{"error /subject/api_key_env not the key itself"},
			usable: false,
		},
		{
			name:   "unknown provider",
			json:   `{"version": 1, "corpus": "evals", "subject": {"provider": "bedrock"}}`,
			want:   []string{"error /subject/provider unknown provider"},
			usable: false,
		},
		{
			name:   "uncatalogued model is a warning",
			json:   `{"version": 1, "corpus": "evals", "subject": {"provider": "anthropic", "model": "claude-nonexistent"}}`,
			want:   []string{"warning /subject/model not in the anthropic catalog"},
			usable: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, issues := ParseManifest([]byte(tt.json))
			if HasErrors(issues) == tt.usable {
				t.Errorf("HasErrors = %v, want %v: %v", HasErrors(issues), !tt.usable, issues)
			}
			if len(issues) != len(tt.want) {
				t.Errorf("got %d issues, want %d: %v", len(issues), len(tt.want), issues)
			}
			for _, w := range tt.want {
				parts := strings.SplitN(w, " ", 3)
				found := false
				for _, is := range issues {
					if string(is.Severity) == parts[0] && is.Pointer == parts[1] && strings.Contains(is.Message, parts[2]) {
						found = true
					}
				}
				if !found {
					t.Errorf("missing issue %q in %v", w, issues)
				}
			}
		})
	}
}

func TestLoadManifestResolvesPaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ManifestFile)
	writeFixture(t, path, `{"version": 1, "corpus": "evals", "store": "/abs/results"}`)
	m, issues, err := LoadManifest(path)
	if err != nil || HasErrors(issues) {
		t.Fatalf("LoadManifest = %v, %v", issues, err)
	}
	if got := m.Resolve(m.Corpus); got != filepath.Join(dir, "evals") {
		t.Errorf("corpus = %s", got)
	}
	if got := m.Resolve(m.Store); got != "/abs/results" {
		t.Errorf("store = %s", got)
	}
	if !m.ContinueOnError() || m.ProviderName() != OpenAICompatible || strings.Join(m.FlowNames(), ",") != "base,stateless" {
		t.Errorf("defaults: continue=%v provider=%s flows=%v", m.ContinueOnError(), m.ProviderName(), m.FlowNames())
	}

	writeFixture(t, path, `{"version": 1, "corpus": "evals", "extra": true}`)
	_, issues, err = LoadManifest(path)
	if err != nil || len(issues) != 1 || issues[0].File != path || !strings.HasPrefix(issues[0].String(), path+":/extra: error:") {
		t.Errorf("issues = %v, %v", issues, err)
	}
	if err := IssuesError(issues); err == nil || !strings.Contains(err.Error(), "1 validation error") {
		t.Errorf("IssuesError = %v", err)
	}
	if _, _, err := LoadManifest(filepath.Join(dir, "missing.json")); !os.IsNotExist(err) {
		t.Errorf("missing file err = %v", err)
	}
}

func TestValidateCorpus(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  []string // "severity substring"
	}{
		{"valid", map[string]string{"a/system.md": "s", "a/turn-0.md": "t", "a/turn-1.md": "t"}, nil},
		{"empty corpus", map[string]string{"README.md": "x"}, []string{"error no scripts"}},
		{"missing turn zero", map[string]string{"a/system.md": "s", "a/turn-1.md": "t"}, []string{"error missing turn-0.md"}},
		{"gap", map[string]string{"a/system.md": "s", "a/turn-0.md": "t", "a/turn-2.md": "t"}, []string{"warning jump from 0 to 2"}},
		{"duplicate index", map[string]string{"a/system.md": "s", "a/turn-0.md": "t", "a/turn-1.md": "t", "a/turn-01.md": "t"}, []string{"error turn 1 is defined by turn-01.md and turn-1.md"}},
		{"missing system", map[string]string{"a/turn-0.md": "t"}, []string{`error system "base"`}},
		{"unknown config key", map[string]string{"a/script.json": `{"formt": "text/html"}`, "a/system.md": "s", "a/turn-0.md": "t"}, []string{`error unknown key "formt" (did you mean "format"?)`}},
		{"legacy config name", map[string]string{"a/experiment.json": `{"format": "text/html"}`, "a/system.md": "s", "a/turn-0.md": "t"}, []string{"warning rename it to script.json"}},
		{"config without base system", map[string]string{"a/script.json": `{"systems": {"other": "o.md"}}`, "a/o.md": "s", "a/turn-0.md": "t"}, []string{`warning no "base" system prompt`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for name, body := range tt.files {
				writeFixture(t, filepath.Join(root, name), body)
			}
			issues := ValidateCorpus(root)
			if len(issues) != len(tt.want) {
				t.Errorf("got %v, want %v", issues, tt.want)
			}
			for _, w := range tt.want {
				severity, text, _ := strings.Cut(w, " ")
				found := false
				for _, is := range issues {
					if string(is.Severity) == severity && strings.Contains(is.Message, text) {
						found = true
					}
				}
				if !found {
					t.Errorf("missing %q in %v", w, issues)
				}
			}
		})
	}
}

func TestLoadCorpusStrict(t *testing.T) {
	tests := []struct {
		name    string
		files   map[string]string
		wantErr string
	}{
		{"unknown config key", map[string]string{"a/script.json": `{"systms": {}}`, "a/turn-0.md": "t"}, `unknown field "systms"`},
		{"trailing data", map[string]string{"a/script.json": `{} {}`, "a/system.md": "s", "a/turn-0.md": "t"}, "unexpected data"},
		{"duplicate turn", map[string]string{"a/system.md": "s", "a/turn-0.md": "t", "a/turn-00.md": "t"}, "two files define turn 0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for name, body := range tt.files {
				writeFixture(t, filepath.Join(root, name), body)
			}
			_, err := LoadCorpus(root)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
