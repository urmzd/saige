package types

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// ruled is an offering that declares a token rule for every modality.
func ruled() *Offering {
	return &Offering{ID: "test/ruled@test", Modalities: Modalities{In: map[Modality]ModalityLimit{
		ModalityImage:    {Tokens: TokenRule{Base: 85, Tile: 512, PerTile: 170}, MaxPixels: 2048 * 2048},
		ModalityDocument: {Tokens: TokenRule{PerPage: 1500}},
		ModalityAudio:    {Tokens: TokenRule{PerSecond: 32}},
		ModalityVideo:    {Tokens: TokenRule{Base: 10, PerSecond: 263}},
	}}}
}

func TestEstimateMediaByTokenRules(t *testing.T) {
	flat := estimateFileTokens
	tests := []struct {
		name     string
		part     Part
		offering *Offering
		want     int
	}{
		{"image without an offering is flat", Image(URL("https://x/a.png"), ImageMeta{Width: 1024, Height: 1024}), nil, flat},
		{"image tiles", Image(URL("https://x/a.png"), ImageMeta{Width: 1024, Height: 768}), ruled(), 85 + 4*170},
		{"image scaled to the pixel cap", Image(URL("https://x/a.png"), ImageMeta{Width: 8192, Height: 8192}), ruled(), 85 + 16*170},
		{"image of unknown size is flat", Image(URL("https://x/a.png")), ruled(), flat},
		{"per image", Image(URL("https://x/a.png")), &Offering{Modalities: Modalities{In: map[Modality]ModalityLimit{
			ModalityImage: {Tokens: TokenRule{PerImage: 258}}}}}, 258},
		{"per pixels", Image(URL("https://x/a.png"), ImageMeta{Width: 100, Height: 100}), &Offering{Modalities: Modalities{In: map[Modality]ModalityLimit{
			ModalityImage: {Tokens: TokenRule{PerPixels: 750}}}}}, 14},
		{"document pages", Document(URL("https://x/a.pdf", MediaPDF), DocumentMeta{Pages: 3}), ruled(), 4500},
		{"document of unknown length is flat", Document(URL("https://x/a.pdf", MediaPDF)), ruled(), flat},
		{"audio seconds", Audio(URL("https://x/a.wav", MediaWAV), AudioMeta{Duration: 2500 * time.Millisecond}), ruled(), 3 * 32},
		{"video clip seconds", Video(URL("https://x/a.mp4", MediaMP4), VideoMeta{Duration: time.Minute, ClipStart: 10 * time.Second, ClipEnd: 20 * time.Second}), ruled(), 10 + 10*263},
		{"text document counts characters", Document(Bytes(MediaText, []byte(strings.Repeat("a", 400)))), ruled(), 100},
		{"json file counts characters", File(Source{MediaType: MediaJSON, Size: 80, URI: "https://x/a.json"}), nil, 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := EstimateMediaTokens(tt.part, tt.offering)
			if !ok || got != tt.want {
				t.Fatalf("EstimateMediaTokens = %d, %v, want %d", got, ok, tt.want)
			}
		})
	}
	if _, ok := EstimateMediaTokens(Text("x"), nil); ok {
		t.Error("a text part counted as media")
	}
}

func TestEstimateTokensForWholeHistory(t *testing.T) {
	img := Image(URL("https://x/a.png"), ImageMeta{Width: 512, Height: 512})
	msgs := []Message{
		UserMsg(Text("abcd"), img),
		AssistantMsg(ServerToolCallPart{ID: "s1", ToolKind: ServerToolWebSearch, Name: "ws", Input: map[string]any{"q": "go"}},
			ServerToolResultPart{CallID: "s1", ToolKind: ServerToolWebSearch, Result: []byte(`{"r":"0123456789"}`)}),
		ToolResults(ToolResultPart{CallID: "c", Parts: []ToolOutputPart{Text("ok"), img}}),
	}
	// Text: "abcd" 4, "ws"+`{"q":"go"}` 12, the result payload 18, "ok" 2 = 36 chars, 9 tokens.
	want := 3*estimateMessageOverhead + 9 + 2*(85+170)
	if got := EstimateTokensFor(msgs, ruled()); got != want {
		t.Fatalf("EstimateTokensFor = %d, want %d", got, want)
	}
	if got := EstimateTokens(msgs); got != 3*estimateMessageOverhead+9+2*estimateFileTokens {
		t.Fatalf("EstimateTokens = %d, want the flat media charge", got)
	}
	n, err := EstimatingTokenizer{Offering: ruled()}.CountTokens(context.Background(), msgs)
	if err != nil || n != want {
		t.Fatalf("CountTokens = %d, %v", n, err)
	}
}

func TestUsageMergeByModality(t *testing.T) {
	a := UsageDelta{Cumulative: true, PromptTokens: 100, PromptByModality: map[Modality]int{ModalityText: 40, ModalityAudio: 60}}
	b := UsageDelta{Cumulative: true, PromptTokens: 100, CompletionTokens: 7, PromptByModality: map[Modality]int{ModalityText: 40, ModalityAudio: 60},
		CompletionByModality: map[Modality]int{ModalityAudio: 7}}
	got := a.Merge(b)
	if got.PromptByModality[ModalityAudio] != 60 || got.PromptByModality[ModalityText] != 40 || got.CompletionByModality[ModalityAudio] != 7 {
		t.Fatalf("cumulative merge = %+v", got)
	}
	inc := UsageDelta{PromptByModality: map[Modality]int{ModalityImage: 5}}.Merge(UsageDelta{PromptByModality: map[Modality]int{ModalityImage: 3}})
	if inc.PromptByModality[ModalityImage] != 8 {
		t.Fatalf("incremental merge = %+v", inc.PromptByModality)
	}
	if a.PromptByModality[ModalityAudio] != 60 || len(a.CompletionByModality) != 0 {
		t.Fatal("merge wrote through to its input")
	}
	if UsageFromDelta(UsageDelta{PromptTokens: 1, Requests: 3}).Requests != 3 || UsageFromDelta(UsageDelta{PromptTokens: 1}).Requests != 1 {
		t.Fatal("requests not carried into billing")
	}
}

// TestUsageWireBeforeModalities decodes a usage envelope written before the
// per-modality counts existed.
func TestUsageWireBeforeModalities(t *testing.T) {
	d, err := UnmarshalDelta([]byte(`{"v":2,"kind":"usage","data":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`))
	if err != nil {
		t.Fatal(err)
	}
	u := d.(UsageDelta)
	if u.PromptTokens != 10 || u.PromptByModality != nil || u.CompletionByModality != nil || u.Requests != 0 {
		t.Fatalf("decoded %+v", u)
	}
}

func TestMediaReference(t *testing.T) {
	src := Bytes(MediaPNG, []byte("png bytes"))
	src.Ref = ArtifactScheme + src.Digest
	src.Filename = "chart.png"
	got := MediaReference(Image(src, ImageMeta{Width: 1024, Height: 768}))
	want := fmt.Sprintf(`[image png 1024x768 "chart.png" sha256:%s… %s]`, src.Digest[:12], src.Ref)
	if got != want {
		t.Fatalf("MediaReference = %q, want %q", got, want)
	}
	doc := MediaReference(Document(URL("https://x/r.pdf", MediaPDF), DocumentMeta{Pages: 4, Title: "Report"}))
	if doc != `[document pdf 4 pages "Report" https://x/r.pdf]` {
		t.Fatalf("document reference = %q", doc)
	}
	if MediaReference(Text("x")) != "" {
		t.Error("a text part got a reference")
	}
	if strings.Contains(MediaReference(Audio(Source{MediaType: MediaWAV, URI: "data:audio/wav;base64,AAAA"})), "base64") {
		t.Error("a data URI leaked into the reference")
	}
}

// TestSummaryInputNeverCarriesBytes checks that the text a summarizer
// reads names every media part, in messages and tool results, by
// reference, and holds none of its bytes.
func TestSummaryInputNeverCarriesBytes(t *testing.T) {
	secret := "RAW-MEDIA-BYTES"
	img := Image(Bytes(MediaPNG, []byte(secret)), ImageMeta{Width: 2, Height: 2})
	out := ImageOutPart{Source: Bytes(MediaPNG, []byte(secret))}
	msgs := []Message{
		UserMsg(Text("look"), img, Document(Bytes(MediaPDF, []byte(secret)))),
		AssistantMsg(ToolCallPart{ID: "c1", Name: "snap"}, out),
		ToolResults(ToolResultPart{CallID: "c1", Parts: []ToolOutputPart{Text("shot"), img}}),
		AssistantMsg(ServerToolCallPart{ID: "s1", ToolKind: ServerToolCodeExecution, Name: "code"},
			ServerToolResultPart{CallID: "s1", ToolKind: ServerToolCodeExecution, Text: "ran", Outputs: []Part{File(VendorFileID("anthropic", "", "file_9", MediaCSV))}}),
	}
	text := MessagesToText(msgs)
	if strings.Contains(text, secret) || strings.Contains(text, "file_9") {
		t.Fatalf("summary input leaks bytes or file IDs:\n%s", text)
	}
	for _, want := range []string{"User: [image png 2x2", "User: [document pdf", "Assistant: [image_out png", "Tool Result [c1]: shot [image png",
		"Server Tool Call [s1]: code", "Server Tool Result [s1]: ran [file csv"} {
		if !strings.Contains(text, want) {
			t.Errorf("summary input lacks %q:\n%s", want, text)
		}
	}
}

// TestCompactionKeepsMediaToolPairs checks that the strategies keep a tool
// call with its media-bearing result, keep the media of recent turns whole,
// and summarize older media by reference.
func TestCompactionKeepsMediaToolPairs(t *testing.T) {
	secret := "PIXELS"
	img := Image(Bytes(MediaPNG, []byte(secret)))
	msgs := []Message{SystemMsg(Text("sys")), UserMsg(Text("task"))}
	for i := range 6 {
		id := fmt.Sprintf("c%d", i)
		msgs = append(msgs, call(id, "snap"),
			ToolResults(ToolResultPart{CallID: id, Parts: []ToolOutputPart{Text("shot"), img}}))
	}
	for _, s := range []CompactionStrategy{NewKeepRecent(2), &Summary{KeepTurns: 2}, &RelevantPlusSummary{KeepTurns: 2, K: 1}} {
		t.Run(s.Name(), func(t *testing.T) {
			p := &summaryProvider{text: "summary"}
			res, err := s.CompactEntries(context.Background(), CompactRequest{Entries: NewCompactEntries(msgs), Query: "snap", Force: true, Provider: p})
			if err != nil || !res.Changed() {
				t.Fatalf("res = %+v, %v", res, err)
			}
			out := EntryMessages(res.Entries)
			if err := ToolPairingError(out); err != nil {
				t.Fatal(err)
			}
			kept := 0
			for _, m := range out {
				for _, r := range toolResults(m) {
					if !r.HasMedia() {
						t.Fatalf("a kept result lost its media: %+v", r)
					}
					kept++
				}
			}
			if kept < 2 {
				t.Fatalf("kept %d results, want the recent turns whole", kept)
			}
			for _, c := range p.calls {
				if strings.Contains(c, secret) || !strings.Contains(c, "[image png") {
					t.Fatalf("summary input = %q, want media by reference only", c)
				}
			}
		})
	}
}
