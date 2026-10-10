package convert_test

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/convert"
	"github.com/urmzd/saige/agent/provider"
	"github.com/urmzd/saige/agent/provider/router"
	"github.com/urmzd/saige/agent/types"
)

// The live checks run only with SAIGE_LIVE=1 and the credentials each one
// names. They use the cheapest models: gpt-6-luna on Chat Completions,
// claude-haiku-5-5, gemini-3.1-flash-lite on Vertex AI as the converter
// model, and a local Ollama model the catalog does not know (so it takes
// text only).

func live(t *testing.T, env ...string) {
	t.Helper()
	if os.Getenv("SAIGE_LIVE") != "1" {
		t.Skip("set SAIGE_LIVE=1 to call providers")
	}
	for _, e := range env {
		if os.Getenv(e) == "" {
			t.Skipf("set %s", e)
		}
	}
}

func build(t *testing.T, cfg provider.Config) types.Provider {
	t.Helper()
	p, err := provider.Build(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// gemini is the converter model: gemini-3.1-flash-lite on Vertex AI.
func gemini(t *testing.T) types.Provider {
	live(t, "GOOGLE_CLOUD_PROJECT")
	return build(t, provider.Config{Provider: provider.Google, Model: "gemini-3.1-flash-lite", Vertex: &provider.Vertex{}})
}

func perAction(m types.Modality, a types.ModalityAction) types.ModalityDial {
	return types.ModalityDial{Per: map[types.Modality][]types.ModalityAction{m: {a}}}
}

// run invokes the agent and returns its answer, the conversion reports it
// streamed, and the routes.
func run(t *testing.T, a *agent.Agent, msgs ...types.Message) (string, []types.ConversionDelta, []types.RouteDelta, types.UsageDelta) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	stream := a.Invoke(ctx, msgs)
	var text strings.Builder
	var convs []types.ConversionDelta
	var routes []types.RouteDelta
	var usage types.UsageDelta
	for d := range stream.Deltas() {
		switch v := d.(type) {
		case types.PartDelta:
			text.WriteString(v.Text)
		case types.ConversionDelta:
			convs = append(convs, v)
		case types.RouteDelta:
			routes = append(routes, v)
		case types.UsageDelta:
			usage = usage.Merge(v)
		}
	}
	if err := stream.Wait(); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(text.String()), convs, routes, usage
}

func spend(t *testing.T, label string, b *types.Budget) {
	t.Helper()
	for _, r := range b.Breakdown() {
		t.Logf("%s spend: %s %d in / %d out tokens, %s", label, r.Model, r.Usage.InputTokens, r.Usage.OutputTokens, r.Cost)
	}
	t.Logf("%s total: %s", label, b.Spent())
}

func onePagePDF(text string) []byte {
	stream := fmt.Sprintf("BT /F1 18 Tf 20 100 Td (%s) Tj ET", text)
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 400 144] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
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
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.Bytes()
}

func redSquare(t *testing.T) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for x := range 64 {
		for y := range 64 {
			img.Set(x, y, color.RGBA{R: 220, A: 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// A PDF to gpt-6-luna on Chat Completions, whose offering takes no
// documents: the built-in extractor turns it into text.
func TestLivePDFToChatViaExtract(t *testing.T) {
	live(t, "OPENAI_API_KEY")
	b := types.NewBudget(types.BudgetPolicy{Limit: types.USD(0.05), PerCallCost: types.USD(0.01)})
	p := build(t, provider.Config{Provider: provider.OpenAI, Model: "gpt-6-luna"})
	a := agent.NewAgent(agent.AgentConfig{Provider: p, Budget: b}, agent.WithConversion(types.ConversionPolicy{
		Dial: perAction(types.ModalityDocument, types.ActExtract), Converters: []types.Converter{convert.Documents()}}))
	doc := types.DocumentPart{Source: types.Bytes(types.MediaPDF, onePagePDF("Invoice 4417: total due 92 dollars"))}
	doc.Source.Filename = "invoice.pdf"
	answer, convs, _, _ := run(t, a, types.UserMsg(types.Text("What is the total due on the invoice? Reply with the number only."), doc))
	if !strings.Contains(answer, "92") {
		t.Fatalf("answer = %q", answer)
	}
	if len(convs) != 1 || convs[0].Report.Decisions[0].Action != types.DecisionExtracted || convs[0].Report.Offering != "openai/gpt-6-luna@openai-chat" {
		t.Fatalf("conversions = %+v", convs)
	}
	t.Logf("answer %q, decision %+v", answer, convs[0].Report.Decisions[0])
	spend(t, "pdf", b)
}

// speech renders text to a 16 kHz WAV with the macOS speech synthesizer.
func speech(t *testing.T, text string) []byte {
	t.Helper()
	if _, err := exec.LookPath("say"); err != nil {
		t.Skip("needs the macOS say command to make a speech clip")
	}
	out := filepath.Join(t.TempDir(), "clip.wav")
	if b, err := exec.Command("say", "-o", out, "--file-format=WAVE", "--data-format=LEI16@16000", text).CombinedOutput(); err != nil {
		t.Fatalf("say: %v: %s", err, b)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// Audio to claude-haiku-5-5, which takes none: gemini-3.1-flash-lite on
// Vertex AI transcribes it, once, however often the history is sent again.
func TestLiveAudioToClaudeViaTranscribe(t *testing.T) {
	live(t, "ANTHROPIC_API_KEY", "GOOGLE_CLOUD_PROJECT")
	clip := speech(t, "The secret code word is pineapple.")
	b := types.NewBudget(types.BudgetPolicy{Limit: types.USD(0.05), PerCallCost: types.USD(0.01)})
	p := build(t, provider.Config{Provider: provider.Anthropic, Model: "claude-haiku-5-5"})
	a := agent.NewAgent(agent.AgentConfig{Provider: p, Budget: b}, agent.WithConversion(types.ConversionPolicy{
		Dial: perAction(types.ModalityAudio, types.ActTranscribe), Converters: []types.Converter{convert.Transcribe(gemini(t))}}))
	audio := types.AudioPart{Source: types.Bytes(types.MediaWAV, clip)}
	answer, convs, _, _ := run(t, a, types.UserMsg(types.Text("What is the code word in the recording? Reply with the word only."), audio))
	if !strings.Contains(strings.ToLower(answer), "pineapple") {
		t.Fatalf("answer = %q", answer)
	}
	d := convs[0].Report.Decisions[0]
	if d.Action != types.DecisionTranscribed || d.Cached || d.Cost == nil || !strings.HasPrefix(d.Via, "transcribe@1.gemini-3.1-flash-lite") {
		t.Fatalf("decision = %+v", d)
	}
	// The next turn sends the clip again: the transcript is reused.
	answer2, convs2, _, _ := run(t, a, types.UserMsg(types.Text("Spell that word backwards. Reply with the letters only.")))
	if len(convs2) != 1 || !convs2[0].Report.Decisions[0].Cached {
		t.Fatalf("second turn conversions = %+v, want a cache hit", convs2)
	}
	t.Logf("answers %q, %q; decision %+v", answer, answer2, d)
	spend(t, "audio", b)
}

// ollamaText is a local model the catalog does not know, so its offering
// takes text only.
func ollamaText(t *testing.T) types.Provider {
	live(t)
	model := os.Getenv("SAIGE_LIVE_OLLAMA_TEXT_MODEL")
	if model == "" {
		model = "gemma4"
	}
	p := build(t, provider.Config{Provider: provider.Ollama, Model: types.ModelID(model)})
	off, _ := convert.Target(p)
	if len(off.Modalities.MediaTypes()) != 0 {
		t.Skipf("%s takes media; set SAIGE_LIVE_OLLAMA_TEXT_MODEL to a text-only model", model)
	}
	return p
}

// An image to a text-only model: gemini-3.1-flash-lite describes it.
func TestLiveImageToTextModelViaDescribe(t *testing.T) {
	text := ollamaText(t)
	b := types.NewBudget(types.BudgetPolicy{Limit: types.USD(0.05), PerCallCost: types.USD(0.01), AllowUnpriced: true})
	a := agent.NewAgent(agent.AgentConfig{Provider: text, Budget: b}, agent.WithConversion(types.ConversionPolicy{
		Dial: perAction(types.ModalityImage, types.ActDescribe), Converters: []types.Converter{convert.Describe(gemini(t))}}))
	img := types.Image(types.Bytes(types.MediaPNG, redSquare(t)))
	answer, convs, _, _ := run(t, a, types.UserMsg(types.Text("What color is the image? Reply with one word."), img))
	if !strings.Contains(strings.ToLower(answer), "red") {
		t.Fatalf("answer = %q", answer)
	}
	if len(convs) != 1 || convs[0].Report.Decisions[0].Action != types.DecisionDescribed {
		t.Fatalf("conversions = %+v", convs)
	}
	t.Logf("answer %q, decision %+v", answer, convs[0].Report.Decisions[0])
	spend(t, "image", b)
}

// Failover from a vision member to a text-only one re-plans the attempt:
// the first member would take the image natively but cannot be reached; the
// second describes it first.
func TestLiveFailoverReplansForATextOnlyMember(t *testing.T) {
	live(t, "OPENAI_API_KEY")
	text := ollamaText(t)
	describe := convert.Describe(gemini(t))
	pol := types.ConversionPolicy{Dial: perAction(types.ModalityImage, types.ActDescribe), Converters: []types.Converter{describe}}
	unreachable := build(t, provider.Config{Provider: provider.OpenAI, Model: "gpt-6-luna", BaseURL: "http://127.0.0.1:9/v1"})
	r, err := router.New(router.Config{Profiles: []router.Profile{
		{ID: "luna", Provider: unreachable},
		{ID: "local-text", Provider: text},
	}})
	if err != nil {
		t.Fatal(err)
	}
	b := types.NewBudget(types.BudgetPolicy{Limit: types.USD(0.05), PerCallCost: types.USD(0.01), AllowUnpriced: true})
	a := agent.NewAgent(agent.AgentConfig{Provider: r.Session(), Budget: b}, agent.WithConversion(pol))
	img := types.Image(types.Bytes(types.MediaPNG, redSquare(t)))
	answer, convs, routes, _ := run(t, a, types.UserMsg(types.Text("What color is the image? Reply with one word."), img))
	if !strings.Contains(strings.ToLower(answer), "red") {
		t.Fatalf("answer = %q", answer)
	}
	if len(routes) != 2 || routes[0].Profile != "luna" || routes[0].Conversions != nil ||
		routes[1].Profile != "local-text" || routes[1].Conversions == nil || routes[1].Conversions.Decisions[0].Action != types.DecisionDescribed {
		t.Fatalf("routes = %+v", routes)
	}
	if len(convs) != 1 || convs[0].Profile != "local-text" {
		t.Fatalf("conversions = %+v", convs)
	}
	t.Logf("answer %q via %s after %s (%s)", answer, routes[1].Profile, routes[0].Profile, routes[1].Reason)
	spend(t, "failover", b)
}

// A batch request carrying a PDF to gpt-6-luna on Chat Completions: the
// request is planned when the batch is submitted, and the built-in
// extractor turns the PDF into text before the batch is uploaded. Vendor
// batches can take a while to finish; the test waits up to 30 minutes.
func TestLiveBatchPDFViaExtract(t *testing.T) {
	live(t, "OPENAI_API_KEY")
	b := types.NewBudget(types.BudgetPolicy{Limit: types.USD(0.05), PerCallCost: types.USD(0.01)})
	p := build(t, provider.Config{Provider: provider.OpenAI, Model: "gpt-6-luna"})
	a := agent.NewAgent(agent.AgentConfig{Provider: p, Budget: b}, agent.WithConversion(types.ConversionPolicy{
		Dial: perAction(types.ModalityDocument, types.ActExtract), Converters: []types.Converter{convert.Documents()}}))
	doc := types.DocumentPart{Source: types.Bytes(types.MediaPDF, onePagePDF("Invoice 5120: total due 37 dollars"))}
	doc.Source.Filename = "invoice.pdf"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	res, err := a.RunBatch(ctx, []agent.BatchInput{{ID: "invoice", Messages: []types.Message{
		types.UserMsg(types.Text("What is the total due on the invoice? Reply with the number only."), doc)}}}, agent.BatchConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Err != nil || !strings.Contains(res[0].Text(), "37") {
		t.Fatalf("results = %+v", res)
	}
	t.Logf("answer %q, usage %+v", res[0].Text(), res[0].Usage)
	spend(t, "batch pdf", b)
}
