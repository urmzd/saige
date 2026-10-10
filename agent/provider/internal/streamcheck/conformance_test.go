package streamcheck

import (
	"errors"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestCheckPartsAcceptsAConformingStream(t *testing.T) {
	stream := []types.Delta{
		types.PartStart{Index: 0, Kind: types.KindText},
		types.PartStart{Index: 1, Kind: types.KindToolCall, ID: "c", Name: "f"},
		types.PartDelta{Index: 1, Args: `{}`},
		types.PartDelta{Index: 0, Text: "hi"},
		types.PartEnd{Index: 0},
		types.PartEnd{Index: 1, Part: types.ToolCallPart{ID: "c", Name: "f", Arguments: map[string]any{}}},
		types.UsageDelta{PromptTokens: 1},
		types.ToolExecDelta{ToolCallID: "c", Inner: types.PartStart{Index: 0, Kind: types.KindText}},
		types.ToolExecDelta{ToolCallID: "c", Inner: types.PartEnd{Index: 0}},
	}
	if v := CheckParts(stream); len(v) != 0 {
		t.Fatalf("violations = %v", v)
	}
	RunPartConformance(t, stream)
}

func TestCheckPartsReportsEachRule(t *testing.T) {
	tests := []struct {
		name   string
		stream []types.Delta
		rule   string
	}{
		{"delta before start", []types.Delta{types.PartDelta{Index: 0, Text: "x"}}, "not open"},
		{"two payloads", []types.Delta{types.PartStart{Index: 0, Kind: types.KindText},
			types.PartDelta{Index: 0, Text: "a", Thinking: "b"}, types.PartEnd{Index: 0}}, "2 payload fields"},
		{"no payload", []types.Delta{types.PartStart{Index: 0, Kind: types.KindText},
			types.PartDelta{Index: 0}, types.PartEnd{Index: 0}}, "0 payload fields"},
		{"end without start", []types.Delta{types.PartEnd{Index: 2}}, "not open"},
		{"reused index", []types.Delta{types.PartStart{Index: 0, Kind: types.KindText}, types.PartEnd{Index: 0},
			types.PartStart{Index: 0, Kind: types.KindText}, types.PartEnd{Index: 0}}, "reuses index 0"},
		{"wrong end kind", []types.Delta{types.PartStart{Index: 0, Kind: types.KindText},
			types.PartEnd{Index: 0, Part: types.ThinkingPart{}}}, "thinking part for a text start"},
		{"left open", []types.Delta{types.PartStart{Index: 0, Kind: types.KindToolCall}}, "never ended"},
		{"no kind", []types.Delta{types.PartStart{Index: 0}, types.PartEnd{Index: 0}}, "no kind"},
		{"nested left open", []types.Delta{types.ToolExecDelta{ToolCallID: "c",
			Inner: types.PartStart{Index: 0, Kind: types.KindText}}}, "never ended"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := CheckParts(tt.stream)
			if len(v) == 0 {
				t.Fatal("no violation reported")
			}
			found := false
			for _, x := range v {
				if strings.Contains(x.Rule, tt.rule) {
					found = true
				}
			}
			if !found {
				t.Fatalf("violations = %v, want one with %q", v, tt.rule)
			}
		})
	}
}

func TestCheckPartsAllowsOpenPartsAfterAnError(t *testing.T) {
	stream := []types.Delta{
		types.PartStart{Index: 0, Kind: types.KindToolCall, ID: "c", Name: "f"},
		types.PartDelta{Index: 0, Args: `{"x":`},
		types.ErrorDelta{Error: errors.New("cut off")},
	}
	if v := CheckParts(stream); len(v) != 0 {
		t.Fatalf("a failed stream must not report its open parts: %v", v)
	}
}
