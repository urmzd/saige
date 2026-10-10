package convert

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/urmzd/saige/agent/types"
)

// Offerings the tests plan against.
var (
	imageLimit = types.ModalityLimit{Media: []types.MediaType{types.MediaPNG, types.MediaJPEG},
		Sources: []types.SourceKind{types.SourceInline, types.SourceURI, types.SourceFile}, MaxBytes: 1 << 20}
	pdfLimit = types.ModalityLimit{Media: []types.MediaType{types.MediaPDF}, Sources: []types.SourceKind{types.SourceInline, types.SourceFile}}
)

// visionChat is a Chat-like offering: images natively (lowered from tool
// results), PDFs inline, no audio.
func visionChat() types.Offering {
	return types.Offering{ID: "openai/vision@openai-chat",
		Model:    types.ModelInfo{Vendor: "openai", Prefix: "vision", Known: true},
		Endpoint: types.EndpointInfo{Name: "openai-chat", Surface: types.SurfaceOpenAIChat, Files: types.FileSupport{URISchemes: []string{"https"}}},
		Modalities: types.Modalities{
			In:         map[types.Modality]types.ModalityLimit{types.ModalityImage: imageLimit, types.ModalityDocument: pdfLimit},
			ToolResult: map[types.Modality]string{types.ModalityImage: types.ToolResultFollowUpUser},
		}}
}

// textOnly takes text alone.
func textOnly() types.Offering {
	return types.Offering{ID: "anthropic/texty@anthropic", Model: types.ModelInfo{Vendor: "anthropic", Prefix: "texty", Known: true},
		Endpoint: types.EndpointInfo{Name: "anthropic", Surface: types.SurfaceAnthropicMessages}}
}

// vertexLike reads gs:// URIs; geminiLike is the same model on an endpoint
// that does not.
func vertexLike() types.Offering {
	o := types.Offering{ID: "google/g@google-vertex", Model: types.ModelInfo{Vendor: "google", Prefix: "g", Known: true},
		Endpoint: types.EndpointInfo{Name: "google-vertex", Surface: types.SurfaceVertex, Files: types.FileSupport{URISchemes: []string{"gs", "https"}}},
		Modalities: types.Modalities{In: map[types.Modality]types.ModalityLimit{
			types.ModalityImage: {Media: []types.MediaType{types.MediaPNG}, Sources: []types.SourceKind{types.SourceInline, types.SourceURI}},
			types.ModalityAudio: {Media: []types.MediaType{types.MediaWAV, types.MediaMP3}, Sources: []types.SourceKind{types.SourceInline, types.SourceURI}},
		}}}
	return o
}

func geminiLike() types.Offering {
	o := vertexLike()
	o.ID, o.Endpoint.Name, o.Endpoint.Surface = "google/g@google-gemini", "google-gemini", types.SurfaceGeminiAPI
	o.Endpoint.Files.URISchemes = []string{"https"}
	return o
}

func pngPart(b string) types.UserPart {
	return types.Image(types.Bytes(types.MediaPNG, []byte(b)))
}

func wav(b string) types.AudioPart {
	return types.AudioPart{Source: types.Bytes(types.MediaWAV, []byte(b))}
}

func pdf(b []byte) types.DocumentPart {
	return types.DocumentPart{Source: types.Bytes(types.MediaPDF, b)}
}

// fake is a converter whose output and failures the test controls.
type fake struct {
	name    string
	action  types.ModalityAction
	media   types.Modality
	text    string
	err     error
	cost    types.Cost
	pricing types.Pricing
	calls   atomic.Int32
}

func (f *fake) Name() string {
	if f.name != "" {
		return f.name
	}
	return "fake-" + string(f.action)
}
func (f *fake) Version() string                      { return "1" }
func (f *fake) Action() types.ModalityAction         { return f.action }
func (f *fake) Produces(types.Part) []types.Modality { return []types.Modality{types.ModalityText} }
func (f *fake) Accepts(p types.Part) bool {
	m, _ := types.PartModality(p)
	return m == f.media
}
func (f *fake) Estimate(types.Part, types.Offering) (types.ConversionEstimate, error) {
	return types.ConversionEstimate{InputTokens: 100, OutputTokens: 50, Cost: f.cost}, nil
}
func (f *fake) Convert(_ context.Context, p types.Part, _ types.ConvertEnv) ([]types.Part, types.ConversionUsage, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, types.ConversionUsage{Usage: types.UsageDelta{PromptTokens: 10}, Pricing: f.pricing, Model: "fake-model"}, f.err
	}
	return []types.Part{types.TextPart{Text: f.text}}, types.ConversionUsage{Usage: types.UsageDelta{PromptTokens: 100, CompletionTokens: 20},
		Pricing: f.pricing, Model: "fake-model"}, nil
}

// pricedFake is a fake that calls a model, so its call is reserved.
type pricedFake struct{ *fake }

func (p pricedFake) Pricing() types.Pricing { return p.pricing }

// journal is a durable step runner that records results by name, as a
// workflow engine does, and replays them without running fn.
type journal struct {
	mu    sync.Mutex
	steps map[string]types.StepResult
	runs  []string
}

func newJournal() *journal { return &journal{steps: map[string]types.StepResult{}} }

func (j *journal) RunStep(ctx context.Context, name string, fn func(context.Context) (types.StepResult, error)) (types.StepResult, error) {
	j.mu.Lock()
	if r, ok := j.steps[name]; ok {
		j.mu.Unlock()
		return r, nil
	}
	j.mu.Unlock()
	r, err := fn(ctx)
	if err != nil {
		return r, err
	}
	j.mu.Lock()
	j.steps[name] = r
	j.runs = append(j.runs, name)
	j.mu.Unlock()
	return r, nil
}

// stubProvider reports an offering and records what it is sent.
type stubProvider struct {
	name     string
	offering types.Offering
	pricing  types.Pricing
	reply    string
	fail     error
	opts     bool

	mu   sync.Mutex
	got  [][]types.Message
	reqs []types.Request
}

func (s *stubProvider) Name() string { return s.name }
func (s *stubProvider) Model() string {
	return string(s.offering.Model.Prefix)
}
func (s *stubProvider) Offering() types.Offering { return s.offering }
func (s *stubProvider) Capabilities() types.ModelCapabilities {
	mc := s.offering.Capabilities()
	mc.Pricing = s.pricing
	return mc
}
func (s *stubProvider) SupportsOptions() bool { return s.opts }
func (s *stubProvider) Stream(_ context.Context, req types.Request) (<-chan types.Delta, error) {
	s.mu.Lock()
	s.got = append(s.got, req.Messages)
	s.reqs = append(s.reqs, req)
	s.mu.Unlock()
	ch := make(chan types.Delta, 6)
	if s.fail != nil {
		ch <- types.ErrorDelta{Error: s.fail}
		close(ch)
		return ch, nil
	}
	reply := s.reply
	if reply == "" {
		reply = "ok"
	}
	ch <- types.PartStart{Index: 0, Kind: types.KindText}
	ch <- types.PartDelta{Index: 0, Text: reply}
	ch <- types.PartEnd{Index: 0}
	ch <- types.UsageDelta{PromptTokens: 1000, CompletionTokens: 100}
	close(ch)
	return ch, nil
}

func (s *stubProvider) last() []types.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.got) == 0 {
		return nil
	}
	return s.got[len(s.got)-1]
}

func (s *stubProvider) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.got)
}

func collect(ch <-chan types.Delta) (deltas []types.Delta, err error) {
	for d := range ch {
		if e, ok := d.(types.ErrorDelta); ok {
			err = e.Error
			continue
		}
		deltas = append(deltas, d)
	}
	return deltas, err
}

var errBoom = errors.New("boom")

// onePagePDF builds a minimal valid one-page PDF showing text.
func onePagePDF(text string) []byte {
	stream := fmt.Sprintf("BT /F1 18 Tf 20 100 Td (%s) Tj ET", text)
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
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.Bytes()
}
