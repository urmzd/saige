package google

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/provider/internal/streamcheck"
	"github.com/urmzd/saige/agent/types"
	"google.golang.org/genai"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

const stop = `"finishReason":"STOP"`

func chunk(parts string, extra ...string) string {
	c := `{"content":{"role":"model","parts":[` + parts + `]}`
	for _, e := range extra {
		c += "," + e
	}
	return `{"candidates":[` + c + `}]}`
}

// streamFixtures are recorded-shape Gemini stream chunks, one per response
// row of the part mapping.
var streamFixtures = map[string][]string{
	"text_runs": {
		chunk(`{"text":"Hel"}`), chunk(`{"text":"lo, "}`), chunk(`{"text":"world."}`, stop),
	},
	"thinking_signature": {
		chunk(`{"text":"Let me ","thought":true}`),
		chunk(`{"text":"think.","thought":true,"thoughtSignature":"` + b64("thought-sig") + `"}`),
		chunk(`{"text":"391"}`),
		chunk(`{"thoughtSignature":"`+b64("text-sig")+`"}`, stop),
	},
	"function_calls": {
		chunk(`{"text":"Checking."}`),
		chunk(`{"functionCall":{"id":"fc1","name":"get_weather","args":{"city":"Oslo"}},"thoughtSignature":"`+b64("call-sig")+`"},`+
			`{"functionCall":{"id":"fc2","name":"get_weather","args":{"city":"Rome"}}}`, stop),
	},
	"code_execution": {
		chunk(`{"executableCode":{"id":"x1","language":"PYTHON","code":"print(1+1)"}}`),
		chunk(`{"codeExecutionResult":{"id":"x1","outcome":"OUTCOME_OK","output":"2\n"}}`),
		chunk(`{"text":"It is 2."}`, stop),
	},
	"grounding": {
		chunk(`{"text":"Apple closed at $1. "}`),
		chunk(`{"text":"Go is fun."}`, stop, `"groundingMetadata":{"webSearchQueries":["apple close"],`+
			`"groundingChunks":[{"web":{"uri":"https://a.example","title":"A","domain":"a.example"}},`+
			`{"retrievedContext":{"uri":"gs://kb/doc.pdf","title":"KB","text":"Go is fun","pageNumber":3}},`+
			`{"web":{"uri":"https://c.example","title":"C"}}],`+
			`"groundingSupports":[{"segment":{"endIndex":18,"text":"Apple closed at $1"},"groundingChunkIndices":[0],"confidenceScores":[0.9]},`+
			`{"segment":{"startIndex":2,"endIndex":5,"text":"Go is fun"},"groundingChunkIndices":[1]},`+
			`{"segment":{"text":"not in the answer"},"groundingChunkIndices":[0]}]}`),
	},
	"image_out": {
		chunk(`{"text":"Here it is."}`),
		chunk(`{"inlineData":{"mimeType":"image/png","data":"`+b64("png-bytes")+`"},"thoughtSignature":"`+b64("image-sig")+`"}`, stop),
	},
	"audio_out": {
		chunk(`{"inlineData":{"mimeType":"audio/L16;codec=pcm;rate=24000","data":"` + b64("aaaa") + `"}}`),
		chunk(`{"inlineData":{"mimeType":"audio/L16;codec=pcm;rate=24000","data":"` + b64("bbbb") + `"}}`),
		chunk(`{"inlineData":{"mimeType":"audio/L16;codec=pcm;rate=24000","data":"`+b64("cc")+`"}}`, stop),
	},
	"file_data_out": {
		chunk(`{"fileData":{"mimeType":"image/jpeg","fileUri":"gs://out/img.jpg"}}`, stop),
	},
	"safety_stop": {
		chunk(`{"text":"Partial"}`), chunk(``, `"finishReason":"SAFETY"`),
	},
	"prompt_blocked": {
		`{"promptFeedback":{"blockReason":"PROHIBITED_CONTENT","blockReasonMessage":"not allowed"}}`,
	},
	"unknown_media": {
		chunk(`{"text":"file:"}`), chunk(`{"inlineData":{"mimeType":"application/zip","data":"`+b64("PK")+`"}}`, stop),
	},
	"cut_off": {
		chunk(`{"text":"Hel"}`),
	},
}

var uuidRE = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// renderDeltas writes one line per part delta or error, with generated IDs
// numbered in order of appearance.
func renderDeltas(t *testing.T, deltas []types.Delta) string {
	t.Helper()
	var b strings.Builder
	for _, d := range deltas {
		switch v := d.(type) {
		case types.PartStart:
			fmt.Fprintf(&b, "start %d %s", v.Index, v.Kind)
			if v.MediaType != "" {
				fmt.Fprintf(&b, " %s", v.MediaType)
			}
			if v.ID != "" {
				fmt.Fprintf(&b, " id=%s", v.ID)
			}
			if v.Name != "" {
				fmt.Fprintf(&b, " name=%s", v.Name)
			}
		case types.PartDelta:
			fmt.Fprintf(&b, "delta %d", v.Index)
			for _, f := range []struct{ name, val string }{{"text", v.Text}, {"thinking", v.Thinking}, {"signature", v.Signature},
				{"args", v.Args}, {"refusal", v.Refusal}, {"data", string(v.Data)}, {"transcript", v.Transcript}} {
				if f.val != "" {
					fmt.Fprintf(&b, " %s=%q", f.name, f.val)
				}
			}
		case types.PartEnd:
			fmt.Fprintf(&b, "end %d", v.Index)
			if v.Part != nil {
				raw, err := types.MarshalPart(v.Part)
				if err != nil {
					t.Fatal(err)
				}
				fmt.Fprintf(&b, " %s", raw)
			}
		case types.ErrorDelta:
			fmt.Fprintf(&b, "error %s", types.KindOf(v.Error))
		default:
			continue
		}
		b.WriteByte('\n')
	}
	ids := map[string]string{}
	return uuidRE.ReplaceAllStringFunc(b.String(), func(id string) string {
		if ids[id] == "" {
			ids[id] = fmt.Sprintf("gen-%d", len(ids)+1)
		}
		return ids[id]
	})
}

func streamFixture(t *testing.T, events []string, opts ...Option) []types.Delta {
	t.Helper()
	opts = append([]Option{WithHTTPClient(&http.Client{Transport: sseTransport{events: events}})}, opts...)
	a, err := NewAdapter(context.Background(), "k", "gemini-3.1-flash-lite", opts...)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := a.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("go"))}})
	if err != nil {
		t.Fatal(err)
	}
	var all []types.Delta
	for d := range ch {
		all = append(all, d)
	}
	return all
}

// TestStreamGoldens pins the part deltas of each response fixture, checks
// the part protocol, and checks that indices are the positions in the
// finished message.
func TestStreamGoldens(t *testing.T) {
	for name, events := range streamFixtures {
		t.Run(name, func(t *testing.T) {
			deltas := streamFixture(t, events)
			streamcheck.RunPartConformance(t, deltas)
			asm := types.NewPartAssembler()
			starts := 0
			for _, d := range deltas {
				if s, ok := d.(types.PartStart); ok {
					if s.Index != starts {
						t.Errorf("start index %d, want %d: indices must count up from 0", s.Index, starts)
					}
					starts++
				}
				asm.Push(d)
			}
			if n := asm.Violations(); n != 0 {
				t.Fatalf("%d assembler violations", n)
			}
			got := renderDeltas(t, deltas)
			path := filepath.Join("testdata", "streams", name+".txt")
			if *update {
				_ = os.MkdirAll(filepath.Dir(path), 0o750)
				if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path) //nolint:gosec // a fixed test path
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if got != string(want) {
				t.Errorf("deltas differ from %s:\n%s", path, got)
			}
		})
	}
}

func assembled(t *testing.T, deltas []types.Delta) types.AssistantMessage {
	t.Helper()
	asm := types.NewPartAssembler()
	for _, d := range deltas {
		asm.Push(d)
	}
	return types.AssistantMessage{Parts: asm.Parts()}
}

// TestGroundingAnchors checks the citations of the grounding fixture: a
// support whose offsets match is anchored by them, one whose offsets do not
// is found by its text, one whose text is absent is reported without an
// anchor, and a chunk no support names is still reported.
func TestGroundingAnchors(t *testing.T) {
	msg := assembled(t, streamFixture(t, streamFixtures["grounding"]))
	var cites []types.CitationPart
	for _, c := range types.Each[types.CitationPart](msg) {
		cites = append(cites, c)
	}
	if len(cites) != 4 {
		t.Fatalf("citations = %+v", cites)
	}
	text := types.TextOf(msg)
	a := cites[0]
	if a.Anchor == nil || a.Anchor.PartIndex != 0 || text[a.Citation.Start:a.Citation.End] != "Apple closed at $1" ||
		a.Citation.Meta["confidence"] != float32(0.9) || a.Citation.Kind != types.CitationWeb || a.Citation.Producer != providerName {
		t.Errorf("first = %+v anchor %+v", a.Citation, a.Anchor)
	}
	g := cites[1]
	if g.Anchor == nil || g.Anchor.PartIndex != 0 || text[g.Anchor.Start:g.Anchor.End] != "Go is fun" ||
		g.Citation.Kind != types.CitationRetrieval || g.Citation.Meta["page"] != 3 {
		t.Errorf("second = %+v anchor %+v", g.Citation, g.Anchor)
	}
	if cites[2].Anchor != nil || cites[2].Citation.URI != "https://a.example" {
		t.Errorf("third = %+v, want an unanchored citation", cites[2])
	}
	if cites[3].Anchor != nil || cites[3].Citation.URI != "https://c.example" || cites[3].Citation.HasSpan() {
		t.Errorf("fourth = %+v, want the unreferenced source", cites[3])
	}
}

// TestAnchorAcrossTextParts anchors a segment in the second of two text
// parts split by a function call.
func TestAnchorAcrossTextParts(t *testing.T) {
	r := newResponseMapper(func(types.Delta) {})
	_ = r.part(&genai.Part{Text: "First."})
	_ = r.part(&genai.Part{FunctionCall: &genai.FunctionCall{ID: "f", Name: "f"}})
	_ = r.part(&genai.Part{Text: "Second part."})
	anchor, start, end := r.anchor(&genai.Segment{StartIndex: 6, EndIndex: 12, Text: "Second"})
	if anchor == nil || anchor.PartIndex != 2 || anchor.Start != 0 || anchor.End != 6 || start != 6 || end != 12 {
		t.Fatalf("anchor = %+v %d %d", anchor, start, end)
	}
}

// TestSignedStreamRoundTrip replays streamed turns: each signature goes back
// on the part Gemini put it on.
func TestSignedStreamRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		want    []string // per model part: what it is and its signature
	}{
		{"thinking_signature", []string{"thought:thought-sig", "text:text-sig"}},
		{"function_calls", []string{"text:", "call get_weather:call-sig", "call get_weather:"}},
		{"image_out", []string{"text:", "image:image-sig"}},
		{"audio_out", []string{"audio:"}},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			msg := assembled(t, streamFixture(t, streamFixtures[tc.fixture]))
			_, contents, err := (&mapper{model: "gemini-3.1-flash-lite", names: map[string]string{}}).contents(
				[]types.Message{types.UserMsg(types.Text("go")), msg})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, p := range contents[1].Parts {
				var what string
				switch {
				case p.Thought:
					what = "thought"
				case p.Text != "":
					what = "text"
				case p.FunctionCall != nil:
					what = "call " + p.FunctionCall.Name
				case p.InlineData != nil && strings.HasPrefix(p.InlineData.MIMEType, "image/"):
					what = "image"
				case p.InlineData != nil:
					what = "audio"
				}
				got = append(got, what+":"+string(p.ThoughtSignature))
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("model parts = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestGeneratedMedia checks the finished output parts: an image with its
// signature, and one audio part joined from the streamed chunks with the
// sample rate read from its media type.
func TestGeneratedMedia(t *testing.T) {
	img := assembled(t, streamFixture(t, streamFixtures["image_out"]))
	var image types.ImageOutPart
	for _, p := range types.Each[types.ImageOutPart](img) {
		image = p
	}
	if string(image.Source.Inline) != "png-bytes" || image.Source.MediaType != types.MediaPNG || image.Signature != b64("image-sig") {
		t.Fatalf("image = %+v", image)
	}
	deltas := streamFixture(t, streamFixtures["audio_out"])
	var chunks [][]byte
	for _, d := range deltas {
		if v, ok := d.(types.PartDelta); ok && len(v.Data) > 0 {
			chunks = append(chunks, v.Data)
		}
	}
	msg := assembled(t, deltas)
	audio, ok := msg.Parts[0].(types.AudioOutPart)
	if len(msg.Parts) != 1 || !ok || string(audio.Source.Inline) != "aaaabbbbcc" || audio.SampleRate != 24000 ||
		audio.Channels != 1 || audio.Format != "pcm" || len(chunks) != 3 {
		t.Fatalf("audio = %+v, chunks = %d", msg.Parts, len(chunks))
	}
}

// TestRefusalParts checks that a safety stop and a blocked prompt are
// recorded as refusal parts and still end with a content-filter error.
func TestRefusalParts(t *testing.T) {
	for name, want := range map[string]types.RefusalPart{
		"safety_stop":    {Category: "SAFETY"},
		"prompt_blocked": {Text: "not allowed", Category: "PROHIBITED_CONTENT"},
	} {
		deltas := streamFixture(t, streamFixtures[name])
		var refusal types.RefusalPart
		for _, p := range types.Each[types.RefusalPart](assembled(t, deltas)) {
			refusal = p
		}
		last, _ := deltas[len(deltas)-1].(types.ErrorDelta)
		if refusal != want || last.Error == nil || !types.IsContentFilter(last.Error) {
			t.Errorf("%s: refusal = %+v, last = %#v", name, refusal, deltas[len(deltas)-1])
		}
	}
}

// TestBatchMatchesStream checks that a complete response maps to the same
// parts as the stream of the same content.
func TestBatchMatchesStream(t *testing.T) {
	for _, name := range []string{"image_out", "grounding", "code_execution", "thinking_signature", "safety_stop"} {
		t.Run(name, func(t *testing.T) {
			events := streamFixtures[name]
			// Merge the chunks into one response, as batch mode returns it.
			var resp genai.GenerateContentResponse
			cand := &genai.Candidate{Content: &genai.Content{Role: "model"}}
			for _, e := range events {
				var c genai.GenerateContentResponse
				if err := json.Unmarshal([]byte(e), &c); err != nil {
					t.Fatal(err)
				}
				if len(c.Candidates) == 0 {
					continue
				}
				cand.Content.Parts = append(cand.Content.Parts, c.Candidates[0].Content.Parts...)
				if c.Candidates[0].FinishReason != "" {
					cand.FinishReason, cand.FinishMessage = c.Candidates[0].FinishReason, c.Candidates[0].FinishMessage
				}
				if c.Candidates[0].GroundingMetadata != nil {
					cand.GroundingMetadata = c.Candidates[0].GroundingMetadata
				}
			}
			resp.Candidates = []*genai.Candidate{cand}
			msg, _, _, err := responseMessage(&resp)
			if err != nil {
				t.Fatal(err)
			}
			want := assembled(t, streamFixture(t, events))
			norm := func(m types.AssistantMessage) string {
				var b bytes.Buffer
				for _, p := range m.Parts {
					raw, _ := types.MarshalPartInline(p)
					b.Write(raw)
					b.WriteByte('\n')
				}
				return uuidRE.ReplaceAllString(b.String(), "id")
			}
			if got, w := norm(msg), norm(want); got != w {
				t.Fatalf("batch parts:\n%s\nstream parts:\n%s", got, w)
			}
		})
	}
}
