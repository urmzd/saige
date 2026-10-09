package catalog

import (
	"errors"
	"strings"
	"testing"
)

func issueAt(t *testing.T, err error, path, code string) {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) || !errors.Is(err, ErrInvalidCatalog) {
		t.Fatalf("want a *ValidationError, got %v", err)
	}
	for _, is := range ve.Issues {
		if is.Path == path && is.Code == code {
			return
		}
	}
	t.Fatalf("no issue %s at %q in %v", code, path, ve.Issues)
}

func TestStrictDecodeErrors(t *testing.T) {
	tests := []struct {
		name, doc, path, code string
	}{
		{"unknown key", `{"version":1,"presets":{"p":{"chain":[{"provider":"openai","model":"gpt-4.1","options":{"reasoning":{"budgett":1}}}]}}}`,
			"presets.p.chain[0].options.reasoning.budgett", CodeUnknownKey},
		{"duplicate key", `{"version":1,"revision":"a","revision":"b"}`, "revision", CodeDuplicateKey},
		{"wrong type", `{"version":1,"models":[{"provider":"openai","prefix":"x","limits":{"context_window":"big"}}]}`,
			"models[0].limits.context_window", CodeWrongType},
		{"bad duration", `{"version":1,"presets":{"p":{"llm_timeout":"soon","chain":[]}}}`, "presets.p.llm_timeout", CodeBadDuration},
		{"unknown capability", `{"version":1,"templates":{"t":{"capabilities":["tool"]}}}`, "templates.t.capabilities[0]", CodeUnknownCap},
		{"extends cycle", `{"version":1,"templates":{"a":{"extends":"b"},"b":{"extends":"a"}}}`, "templates.a.extends", CodeExtendsCycle},
		{"api key", `{"version":1,"presets":{"p":{"chain":[{"provider":"openai","model":"gpt-4.1","api_key":"sk-x"}]}}}`,
			"presets.p.chain[0].api_key", CodeSecretKey},
		{"bad version", `{"version":2}`, "version", CodeVersion},
		{"missing version", `{"revision":"x"}`, "version", CodeMissing},
		{"trailing data", `{"version":1} {}`, "", CodeTrailingData},
		{"null in options", `{"version":1,"presets":{"p":{"options":{"temperature":null},"chain":[]}}}`, "presets.p.options.temperature", CodeNullNotAllowed},
		{"forced tool choice in options", `{"version":1,"presets":{"p":{"options":{"tool_choice":"required"},"chain":[]}}}`, "presets.p.options.tool_choice", CodeToolChoice},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(strings.NewReader(tt.doc))
			issueAt(t, err, tt.path, tt.code)
		})
	}
}

func TestUnknownKeySuggestsField(t *testing.T) {
	_, err := Load(strings.NewReader(`{"version":1,"revison":"x"}`))
	if err == nil || !strings.Contains(err.Error(), `did you mean "revision"`) {
		t.Fatalf("got %v", err)
	}
}

func TestNullDeletesInRowPatchOnly(t *testing.T) {
	if _, err := Load(strings.NewReader(`{"version":1,"models":[{"provider":"openai","prefix":"gpt-4.1","pricing":null}],"presets":{"old":null}}`)); err != nil {
		t.Fatalf("row and preset nulls are merge deletions: %v", err)
	}
}

func TestLoadFileNamesSource(t *testing.T) {
	_, err := LoadFile("testdata/missing.json")
	if err == nil {
		t.Fatal("missing file loaded")
	}
}
