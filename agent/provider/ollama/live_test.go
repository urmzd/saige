package ollama

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/provider/internal/streamcheck"
	"github.com/urmzd/saige/agent/types"
)

// liveAdapter returns an adapter for the local runtime, or skips. It runs
// only with SAIGE_LIVE=1; OLLAMA_HOST and SAIGE_OLLAMA_MODEL override the
// defaults (localhost, qwen3.5:4b, which takes text, tools, thinking and
// images).
func liveAdapter(t *testing.T, opts ...Option) *Adapter {
	t.Helper()
	if os.Getenv("SAIGE_LIVE") != "1" {
		t.Skip("set SAIGE_LIVE=1 with a local ollama runtime to run")
	}
	host := os.Getenv("OLLAMA_HOST")
	if host == "" {
		host = "http://localhost:11434"
	}
	if !strings.Contains(host, "://") {
		host = "http://" + host
	}
	model := os.Getenv("SAIGE_OLLAMA_MODEL")
	if model == "" {
		model = "qwen3.5:4b"
	}
	return NewAdapter(NewClient(host, model, "", opts...))
}

func liveStream(t *testing.T, a *Adapter, req types.Request) []types.AssistantPart {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
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
	return asm.Parts()
}

func TestLiveText(t *testing.T) {
	a := liveAdapter(t, WithThink(false))
	parts := liveStream(t, a, types.Request{Messages: []types.Message{types.UserMsg(types.Text("Reply with the single word: pong"))}})
	if got := types.TextOf(types.AssistantMsg(parts...)); !strings.Contains(strings.ToLower(got), "pong") {
		t.Fatalf("text = %q", got)
	}
}

func TestLiveThinking(t *testing.T) {
	a := liveAdapter(t, WithThink(true), WithChatOptions(Options{NumPredict: 2048}))
	parts := liveStream(t, a, types.Request{Messages: []types.Message{types.UserMsg(types.Text("What is 17+25? Answer with the number."))}})
	var thought bool
	for _, p := range parts {
		if th, ok := p.(types.ThinkingPart); ok && th.Text != "" {
			thought = true
		}
	}
	if !thought || !strings.Contains(types.TextOf(types.AssistantMsg(parts...)), "42") {
		t.Fatalf("parts = %#v, want thinking and 42", parts)
	}
}

func TestLiveToolCallRoundTrip(t *testing.T) {
	a := liveAdapter(t, WithThink(false))
	tools := []types.ToolDef{{Name: "get_weather", Description: "Get the current weather for a city",
		Parameters: types.ParameterSchema{Type: "object", Required: []string{"city"},
			Properties: map[string]types.PropertyDef{"city": {Type: "string"}}}}}
	msgs := []types.Message{types.UserMsg(types.Text("What is the weather in Paris? Use the tool."))}
	parts := liveStream(t, a, types.Request{Messages: msgs, Tools: tools})
	var call *types.ToolCallPart
	for _, p := range parts {
		if c, ok := p.(types.ToolCallPart); ok {
			call = &c
		}
	}
	if call == nil || call.Name != "get_weather" || call.ID == "" {
		t.Fatalf("parts = %#v, want a get_weather call", parts)
	}
	msgs = append(msgs, types.AssistantMsg(parts...),
		types.ToolResults(types.ToolOK(call.ID, types.Text("It is 31 degrees Celsius and sunny."))))
	parts = liveStream(t, a, types.Request{Messages: msgs, Tools: tools})
	if got := types.TextOf(types.AssistantMsg(parts...)); !strings.Contains(got, "31") {
		t.Fatalf("answer = %q, want the tool's 31 degrees", got)
	}
}

func TestLiveImage(t *testing.T) {
	a := liveAdapter(t, WithThink(false))
	img := types.Image(types.Bytes(types.MediaPNG, solidPNG(32, 32, 255, 0, 0)))
	parts := liveStream(t, a, types.Request{Messages: []types.Message{
		types.UserMsg(types.Text("What single color fills this image? One word."), img)}})
	if got := types.TextOf(types.AssistantMsg(parts...)); !strings.Contains(strings.ToLower(got), "red") {
		t.Fatalf("text = %q, want red", got)
	}
}

func TestLiveToolResultImage(t *testing.T) {
	a := liveAdapter(t, WithThink(false))
	tools := []types.ToolDef{{Name: "get_photo", Description: "Get the photo", Parameters: types.ParameterSchema{Type: "object"}}}
	parts := liveStream(t, a, types.Request{Tools: tools, Messages: []types.Message{
		types.UserMsg(types.Text("Fetch the photo with the tool and tell me its single fill color in one word.")),
		types.AssistantMsg(types.ToolCallPart{ID: "call_1", Name: "get_photo", Arguments: map[string]any{}}),
		types.ToolResults(types.ToolOK("call_1", types.Text("photo attached"), types.Image(types.Bytes(types.MediaPNG, solidPNG(32, 32, 0, 0, 255))))),
	}})
	if got := types.TextOf(types.AssistantMsg(parts...)); !strings.Contains(strings.ToLower(got), "blue") {
		t.Fatalf("text = %q, want blue", got)
	}
}

func TestLiveSchemaWithOptions(t *testing.T) {
	a := liveAdapter(t)
	schema := &types.ParameterSchema{Type: "object", Required: []string{"sum"},
		Properties: map[string]types.PropertyDef{"sum": {Type: "integer"}}}
	parts := liveStream(t, a, types.Request{
		Messages: []types.Message{types.UserMsg(types.Text("What is 2+3? Reply as JSON."))},
		Schema:   schema,
		Options:  &types.RequestOptions{ToolChoice: &types.ToolChoice{Mode: types.ToolChoiceNone}},
	})
	if got := types.TextOf(types.AssistantMsg(parts...)); !strings.Contains(strings.ReplaceAll(got, " ", ""), `"sum":5`) {
		t.Fatalf("text = %q", got)
	}
}

// solidPNG encodes a w x h image filled with one color.
func solidPNG(w, h int, r, g, b uint8) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: color.RGBA{R: r, G: g, B: b, A: 255}}, image.Point{}, draw.Src)
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}
