package agui

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

var update = flag.Bool("update", false, "rewrite golden files")

// partsRun is a run with every part kind a client renders.
func partsRun() []types.Delta {
	ref := types.Artifact(types.ArtifactScheme+strings.Repeat("ab", 32), "image/png")
	ref.Size = 300 << 10
	var ds []types.Delta
	ds = append(ds, types.PartDeltas(0, types.ThinkingPart{Text: "hmm"})...)
	ds = append(ds, types.PartDeltas(1, types.Text("Here it is."))...)
	ds = append(ds,
		types.PartStart{Index: 2, Kind: types.KindToolCall, ID: "c1", Name: "chart"},
		types.PartDelta{Index: 2, Args: `{"q":1}`},
		types.PartEnd{Index: 2, Part: types.ToolCallPart{ID: "c1", Name: "chart", Arguments: map[string]any{"q": 1}}},
		types.ToolExecStartDelta{ToolCallID: "c1", Name: "chart"},
		types.ToolExecEndDelta{ToolCallID: "c1", Name: "chart", Result: "rendered",
			Parts: []types.ToolOutputPart{types.Text("rendered"), types.Image(ref)}},
		types.PartStart{Index: 3, Kind: types.KindImageOut, MediaType: "image/png"},
		types.PartDelta{Index: 3, Data: []byte("PNG")},
		types.PartEnd{Index: 3, Part: types.ImageOutPart{Source: types.Bytes("image/png", []byte("PNG")), ImageMeta: types.ImageMeta{Width: 2, Height: 2}}},
		types.PartStart{Index: 4, Kind: types.KindAudioOut, MediaType: "audio/wav"},
		types.PartDelta{Index: 4, Transcript: "hello "},
		types.PartDelta{Index: 4, Transcript: "there"},
		types.PartEnd{Index: 4, Part: types.AudioOutPart{Source: types.URL("https://cdn.example/a.wav", "audio/wav")}},
	)
	ds = append(ds, types.PartDeltas(5, types.CitationPart{Citation: types.Citation{Ordinal: 1, Title: "Src", URI: "https://example.com"}})...)
	ds = append(ds,
		types.PartStart{Index: 6, Kind: types.KindRefusal},
		types.PartDelta{Index: 6, Refusal: "I can't "},
		types.PartDelta{Index: 6, Refusal: "do that."},
		types.PartEnd{Index: 6, Part: types.RefusalPart{Text: "I can't do that.", Category: "safety"}},
		types.DoneDelta{},
	)
	return ds
}

func TestMapperPartsGolden(t *testing.T) {
	link := func(src types.Source) string {
		if id, ok := strings.CutPrefix(src.Ref, types.ArtifactScheme); ok {
			return "/artifacts/" + id
		}
		return ""
	}
	m := NewMapper("th", "run", WithMediaLink(link))
	var lines []string
	lines = append(lines, mustJSON(t, m.Start()))
	for _, ev := range mapAll(t, m, partsRun()) {
		lines = append(lines, mustJSON(t, ev))
	}
	got := strings.Join(lines, "\n") + "\n"
	path := filepath.Join("testdata", "parts.golden")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if string(want) != got {
		t.Errorf("AG-UI events differ from %s:\n%s", path, got)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestMapperMediaWithoutLink(t *testing.T) {
	m := NewMapper("th", "run")
	events, err := m.Map(types.PartEnd{Index: 0, Part: types.ImageOutPart{Source: types.Bytes("image/png", []byte("PNG"))}})
	if err != nil || len(events) != 1 {
		t.Fatalf("events = %v, %v", events, err)
	}
	var media Media
	if err := json.Unmarshal(events[0].Value, &media); err != nil {
		t.Fatal(err)
	}
	if events[0].Name != MediaEventName || string(media.Data) != "PNG" || media.URL != "" || media.Size != 3 {
		t.Fatalf("media = %+v", media)
	}
	// A ref without a link function is still a link.
	events, _ = m.Map(types.ToolExecEndDelta{ToolCallID: "c", Parts: []types.ToolOutputPart{types.Image(types.Artifact("saige-artifact://ff", "image/png"))}})
	if len(events) != 2 || events[1].ToolCallID != "c" || !strings.Contains(string(events[1].Value), `"url":"saige-artifact://ff"`) {
		t.Fatalf("tool media events = %+v", events)
	}
}
