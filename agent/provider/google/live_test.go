package google

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/provider/internal/streamcheck"
	"github.com/urmzd/saige/agent/types"
	"google.golang.org/genai"
)

// liveAdapter returns a Vertex AI adapter, or skips. It runs only with
// SAIGE_LIVE=1 and GOOGLE_CLOUD_PROJECT set; GOOGLE_CLOUD_LOCATION
// defaults to global, and model defaults to SAIGE_GOOGLE_MODEL or
// gemini-3.1-flash-lite.
func liveAdapter(t *testing.T, model string, opts ...Option) *Adapter {
	t.Helper()
	project := os.Getenv("GOOGLE_CLOUD_PROJECT")
	if os.Getenv("SAIGE_LIVE") != "1" || project == "" {
		t.Skip("set SAIGE_LIVE=1 and GOOGLE_CLOUD_PROJECT to run against Vertex AI")
	}
	location := os.Getenv("GOOGLE_CLOUD_LOCATION")
	if location == "" {
		location = "global"
	}
	if model == "" {
		model = os.Getenv("SAIGE_GOOGLE_MODEL")
	}
	if model == "" {
		model = "gemini-3.1-flash-lite"
	}
	a, err := NewAdapter(context.Background(), "", model, append([]Option{WithVertex(project, location)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func liveStream(t *testing.T, a *Adapter, req types.Request) types.AssistantMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ch, err := a.Stream(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	var all []types.Delta
	asm := types.NewPartAssembler()
	for d := range ch {
		if e, ok := d.(types.ErrorDelta); ok {
			t.Fatal(e.Error)
		}
		all = append(all, d)
		asm.Push(d)
	}
	streamcheck.RunPartConformance(t, all)
	if n := asm.Violations(); n != 0 {
		t.Fatalf("%d part violations", n)
	}
	return types.AssistantMessage{Parts: asm.Parts()}
}

func solidPNG(t *testing.T, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: c}, image.Point{}, draw.Src)
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// toneWAV is one second of a 440 Hz tone, 16 kHz mono.
func toneWAV() []byte {
	const rate = 16000
	var pcm bytes.Buffer
	for i := range rate {
		_ = binary.Write(&pcm, binary.LittleEndian, int16(8000*math.Sin(2*math.Pi*440*float64(i)/rate)))
	}
	var b bytes.Buffer
	w := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	b.WriteString("RIFF")
	w(uint32(36 + pcm.Len()))
	b.WriteString("WAVEfmt ")
	w(uint32(16))
	w(uint16(1))
	w(uint16(1))
	w(uint32(rate))
	w(uint32(rate * 2))
	w(uint16(2))
	w(uint16(16))
	b.WriteString("data")
	w(uint32(pcm.Len())) //nolint:gosec // one second of audio
	b.Write(pcm.Bytes())
	return b.Bytes()
}

// onePagePDF is a single page that reads "The secret word is PELICAN".
func onePagePDF() []byte {
	stream := "BT /F1 18 Tf 20 70 Td (The secret word is PELICAN) Tj ET"
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 144] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, o := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.Bytes()
}

func wantText(t *testing.T, msg types.AssistantMessage, word string) {
	t.Helper()
	if got := types.TextOf(msg); !strings.Contains(strings.ToLower(got), strings.ToLower(word)) {
		t.Fatalf("answer = %q, want it to contain %q", got, word)
	}
}

func TestLiveMediaInput(t *testing.T) {
	a := liveAdapter(t, "")
	clip, err := os.ReadFile("testdata/clip.mp4")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		part types.UserPart
		ask  string
		want string
	}{
		{"image", types.Image(types.Bytes(types.MediaPNG, solidPNG(t, color.RGBA{R: 255, A: 255}))), "What color is this image? One word.", "red"},
		{"audio", types.Audio(types.Bytes(types.MediaWAV, toneWAV())), "Is there any sound in this audio? Answer yes or no.", "yes"},
		{"video", types.Video(types.Bytes(types.MediaMP4, clip), types.VideoMeta{ClipEnd: time.Second, FPS: 2}), "What color fills this video? One word.", "green"},
		{"pdf", types.Document(types.Bytes(types.MediaPDF, onePagePDF())), "What is the secret word? One word.", "pelican"},
		{"gs uri", types.Image(types.URL("gs://cloud-samples-data/generative-ai/image/scones.jpg", types.MediaJPEG)), "What baked food is shown? One word.", "scone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantText(t, liveStream(t, a, types.Request{Messages: []types.Message{types.UserMsg(types.Text(tc.ask), tc.part)}}), tc.want)
		})
	}
}

// TestLiveToolResultMedia returns an image from a tool and replays the
// signed call.
func TestLiveToolResultMedia(t *testing.T) {
	a := liveAdapter(t, "")
	tools := []types.ToolDef{{Name: "take_photo", Description: "takes a photo", Parameters: types.ParameterSchema{Type: "object"}}}
	msgs := []types.Message{types.UserMsg(types.Text("Call take_photo, then say the photo's color in one word."))}
	turn := liveStream(t, a, types.Request{Messages: msgs, Tools: tools})
	var id string
	for _, c := range types.Each[types.ToolCallPart](turn) {
		id = c.ID
	}
	if id == "" {
		t.Fatalf("no tool call in %+v", turn.Parts)
	}
	msgs = append(msgs, turn, types.ToolResults(types.ToolOK(id, types.Image(types.Bytes(types.MediaPNG, solidPNG(t, color.RGBA{B: 255, A: 255}))))))
	wantText(t, liveStream(t, a, types.Request{Messages: msgs, Tools: tools}), "blue")
}

func TestLiveGroundingCitations(t *testing.T) {
	a := liveAdapter(t, "", WithGoogleSearch())
	msg := liveStream(t, a, types.Request{Messages: []types.Message{types.UserMsg(
		types.Text("Search the web: what was the closing price of Apple stock yesterday? One sentence."))}})
	n := 0
	for _, c := range types.Each[types.CitationPart](msg) {
		if c.Anchor != nil {
			n++
		}
	}
	if n == 0 {
		t.Fatalf("no anchored citation in %+v", msg.Parts)
	}
}

// TestLiveGeneratedMedia runs only with SAIGE_GOOGLE_IMAGE_MODEL or
// SAIGE_GOOGLE_TTS_MODEL set, such as gemini-2.5-flash-image and
// gemini-2.5-flash-preview-tts.
func TestLiveGeneratedMedia(t *testing.T) {
	t.Run("image", func(t *testing.T) {
		model := os.Getenv("SAIGE_GOOGLE_IMAGE_MODEL")
		if model == "" {
			t.Skip("set SAIGE_GOOGLE_IMAGE_MODEL")
		}
		a := liveAdapter(t, model, WithResponseModalities(genai.ModalityText, genai.ModalityImage))
		msg := liveStream(t, a, types.Request{Messages: []types.Message{types.UserMsg(types.Text("Draw a tiny red circle on white."))}})
		for _, p := range types.Each[types.ImageOutPart](msg) {
			if len(p.Source.Inline) > 0 {
				return
			}
		}
		t.Fatalf("no image in %+v", msg.Parts)
	})
	t.Run("audio", func(t *testing.T) {
		model := os.Getenv("SAIGE_GOOGLE_TTS_MODEL")
		if model == "" {
			t.Skip("set SAIGE_GOOGLE_TTS_MODEL")
		}
		a := liveAdapter(t, model, WithResponseModalities(genai.ModalityAudio), WithSpeechConfig(&genai.SpeechConfig{
			VoiceConfig: &genai.VoiceConfig{PrebuiltVoiceConfig: &genai.PrebuiltVoiceConfig{VoiceName: "Kore"}}}))
		msg := liveStream(t, a, types.Request{Messages: []types.Message{types.UserMsg(types.Text("Say: hello there."))}})
		for _, p := range types.Each[types.AudioOutPart](msg) {
			if len(p.Source.Inline) > 0 && p.SampleRate > 0 {
				return
			}
		}
		t.Fatalf("no audio in %+v", msg.Parts)
	})
}
