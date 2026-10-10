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
		{"unknown capability", `{"version":2,"offering_templates":{"t":{"features":["tool"]}}}`, "offering_templates.t.features[0]", CodeUnknownCap},
		{"parameter as feature", `{"version":2,"offering_templates":{"t":{"features":["temperature"]}}}`, "offering_templates.t.features[0]", CodeUnknownCap},
		{"v1 unknown capability", `{"version":1,"templates":{"t":{"capabilities":["tool"]}}}`, "offering_templates.t.features[0]", CodeUnknownCap},
		{"extends cycle", `{"version":2,"offering_templates":{"a":{"extends":"b"},"b":{"extends":"a"}}}`, "offering_templates.a.extends", CodeExtendsCycle},
		{"secret literal", `{"version":2,"endpoints":{"e":{"surface":"openai.chat","serves":["openai"],"auth":{"secret":"sk-live"}}}}`,
			"endpoints.e.auth.secret", CodeSecretRef},
		{"unknown param", `{"version":2,"offering_templates":{"t":{"params":{"temprature":{"allowed":true}}}}}`, "offering_templates.t.params.temprature", CodeParam},
		{"media in wrong modality", `{"version":2,"offering_templates":{"t":{"modalities":{"in":{"image":{"media":["application/pdf"]}}}}}}`,
			"offering_templates.t.modalities.in.image.media[0]", CodeModality},
		{"api key", `{"version":1,"presets":{"p":{"chain":[{"provider":"openai","model":"gpt-4.1","api_key":"sk-x"}]}}}`,
			"presets.p.chain[0].api_key", CodeSecretKey},
		{"bad version", `{"version":3}`, "version", CodeVersion},
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
