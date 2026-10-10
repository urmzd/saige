package agent

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestExtractJSON(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		err  error
	}{
		{"bare object", `{"a":1}`, `{"a":1}`, nil},
		{"surrounding whitespace", "\n  {\"a\":1}\n", `{"a":1}`, nil},
		{"bare array", `[1,2]`, `[1,2]`, nil},
		{"json fence", "Here you go:\n```json\n{\"a\":1}\n```\nDone.", `{"a":1}`, nil},
		{"plain fence", "```\n[1]\n```", `[1]`, nil},
		{"prose around", `The answer is {"a":{"b":"}"}} as requested.`, `{"a":{"b":"}"}}`, nil},
		{"think block removed", `<think>maybe {"a":0}</think>{"a":1}`, `{"a":1}`, nil},
		{"thinking tag removed", "<thinking>\n{\"x\":0}\n</thinking>\n```json\n{\"a\":1}\n```", `{"a":1}`, nil},
		{"lone closing think tag", `reasoning {"a":0} </think> {"a":1}`, `{"a":1}`, nil},
		{"skips invalid braces", `use {placeholder} then {"a":1}`, `{"a":1}`, nil},
		{"brace inside string ignored", `{"a":"[not"} trailing`, `{"a":"[not"}`, nil},
		{"scalar JSON is accepted whole", `42`, `42`, nil},
		{"no json", "I cannot answer that.", "", ErrNoJSON},
		{"unclosed", `{"a":`, "", ErrNoJSON},
		{"empty", "", "", ErrNoJSON},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExtractJSON(tt.in)
			if !errors.Is(err, tt.err) {
				t.Fatalf("err = %v, want %v", err, tt.err)
			}
			if got != tt.want {
				t.Fatalf("ExtractJSON = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCompleteJSON(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{`{`, `{}`, true},
		{`{"a`, `{}`, true},
		{`{"a"`, `{}`, true},
		{`{"a":`, `{}`, true},
		{`{"a": "he`, `{"a": "he"}`, true},
		{`{"a": "he\`, `{"a": "he"}`, true},
		{`{"a": 12`, `{"a": 12}`, true},
		{`{"x":1,"a": tr`, `{"x":1}`, true},
		{`{"a": "b",`, `{"a": "b"}`, true},
		{`{"a": [1, {"b": "c`, `{"a": [1, {"b": "c"}]}`, true},
		{`[1, 2`, `[1, 2]`, true},
		{`{"a":1}`, `{"a":1}`, true},
		{``, ``, false},
		{`tru`, ``, false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, ok := completeJSON(tt.in)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("completeJSON(%q) = %q, %v; want %q, %v", tt.in, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestPartialJSONTracker(t *testing.T) {
	schema := &types.ParameterSchema{Type: "object"}
	tests := []struct {
		name   string
		out    runOutput
		tools  []types.ToolDef
		deltas []types.Delta
		want   []string
	}{
		{
			name: "native text",
			out:  runOutput{schema: schema, mode: OutputNative},
			deltas: []types.Delta{
				types.PartStart{Index: 0, Kind: types.KindText},
				types.PartDelta{Index: 0, Text: `{"city": "To`},
				types.PartDelta{Index: 0, Text: `kyo", "pop`},
				types.PartDelta{Index: 0, Text: `": 14}`},
			},
			want: []string{`{"city":"To"}`, `{"city":"Tokyo"}`, `{"city":"Tokyo","pop":14}`},
		},
		{
			name: "prompted text after prose and inside a fence",
			out:  runOutput{schema: schema, mode: OutputPrompt},
			deltas: []types.Delta{
				types.PartDelta{Index: 0, Text: "Sure:\n```json\n"},
				types.PartDelta{Index: 0, Text: `{"a": 1`},
				types.PartDelta{Index: 0, Text: "}\n```\nDone"},
			},
			want: []string{`{"a":1}`},
		},
		{
			name:  "native schema with tools streams nothing",
			out:   runOutput{schema: schema, mode: OutputNative},
			tools: []types.ToolDef{{Name: "lookup"}},
			deltas: []types.Delta{
				types.PartDelta{Index: 0, Text: `{"a": 1}`},
			},
		},
		{
			name: "final_answer arguments only",
			out:  runOutput{schema: schema, mode: OutputTool},
			deltas: []types.Delta{
				types.PartStart{Index: 1, Kind: types.KindToolCall, ID: "c1", Name: "lookup"},
				types.PartDelta{Index: 1, Args: `{"q": "x"}`},
				types.PartStart{Index: 2, Kind: types.KindToolCall, ID: "c2", Name: FinalAnswerToolName},
				types.PartDelta{Index: 2, Args: `{"city": "Os`},
				types.PartDelta{Index: 1, Args: `ignored`},
				types.PartDelta{Index: 2, Args: `aka"}`},
				types.PartDelta{Index: 0, Text: `{"no": 1}`},
			},
			want: []string{`{"city":"Os"}`, `{"city":"Osaka"}`},
		},
		{
			name:   "no schema",
			out:    runOutput{},
			deltas: []types.Delta{types.PartDelta{Index: 0, Text: `{"a": 1}`}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newPartialJSON(tt.out, tt.tools)
			var got []string
			for _, d := range tt.deltas {
				if pj, ok := p.push(d); ok {
					got = append(got, string(pj.JSON))
				}
			}
			if strings.Join(got, "|") != strings.Join(tt.want, "|") {
				t.Fatalf("partials = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFinalAnswerTool(t *testing.T) {
	tool := &finalAnswerTool{
		schema: *cityPopulationSchema,
		check: func(raw json.RawMessage) error {
			if strings.Contains(string(raw), "Atlantis") {
				return errors.New("no such city")
			}
			return nil
		},
	}
	tests := []struct {
		name    string
		args    map[string]any
		want    string
		wantErr string
	}{
		{"valid", map[string]any{"city": "Tokyo"}, `{"city":"Tokyo"}`, ""},
		{"schema error", map[string]any{"city": 3.0}, "", "must be string"},
		{"check error", map[string]any{"city": "Atlantis"}, "", "no such city"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tool.Execute(t.Context(), tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("Execute = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}
