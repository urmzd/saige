package extractor_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	agenttypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/rag/extractor"
	"github.com/urmzd/saige/rag/types"
)

func TestAutoAsPartExtractor(t *testing.T) {
	auto := extractor.NewAuto()
	ex := auto.PartExtractor()
	parts, err := ex.Extract(context.Background(), []byte("<html><head><title>Guide</title></head><body><h1>Setup</h1><p>Run make.</p></body></html>"), "text/html; charset=utf-8")
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 {
		t.Fatalf("parts = %#v", parts)
	}
	text := parts[0].(agenttypes.TextPart).Text
	if !strings.Contains(text, "# Guide") || !strings.Contains(text, "Run make.") {
		t.Fatalf("text = %q", text)
	}
	if _, err := ex.Extract(context.Background(), []byte("  \n "), "text/plain"); !errors.Is(err, extractor.ErrNoText) {
		t.Fatalf("empty document: %v", err)
	}
	if _, err := ex.Extract(context.Background(), []byte{0x89, 'P', 'N', 'G'}, "image/png"); !errors.Is(err, types.ErrUnsupportedMIMEType) {
		t.Fatalf("image without a describer: %v", err)
	}

	got := auto.Extractors()
	for _, mt := range []agenttypes.MediaType{"application/pdf", "text/html", "text/plain"} {
		if got[mt] == nil {
			t.Errorf("no extractor for %s", mt)
		}
	}
}

// describer describes any image the same way and counts calls.
type describer struct{ calls int }

func (d *describer) Extract(_ context.Context, data []byte, mt agenttypes.MediaType) ([]agenttypes.UserPart, error) {
	d.calls++
	return []agenttypes.UserPart{agenttypes.Text("A bar chart of revenue by quarter (" + string(mt) + ")")}, nil
}

func TestImagesIngestThroughADescriber(t *testing.T) {
	auto := extractor.NewAuto()
	d := &describer{}
	auto.RegisterImages(d)
	png := []byte("\x89PNG fake")
	doc, err := auto.Extract(context.Background(), &types.RawDocument{SourceURI: "file://chart.png", MIMEType: "image/png", Data: png,
		Metadata: map[string]string{"team": "finance"}})
	if err != nil {
		t.Fatal(err)
	}
	if d.calls != 1 || len(doc.Sections) != 1 || len(doc.Sections[0].Variants) != 1 {
		t.Fatalf("doc = %+v", doc)
	}
	v := doc.Sections[0].Variants[0]
	if v.ContentType != types.ContentImage || string(v.Data) != string(png) || !strings.Contains(v.Text, "revenue by quarter") ||
		v.MIMEType != "image/png" || v.Metadata["team"] != "finance" || v.UUID == "" || v.SectionUUID != doc.Sections[0].UUID {
		t.Fatalf("variant = %+v", v)
	}
	part, err := v.Part()
	if err != nil || part.Kind() != agenttypes.KindImage {
		t.Fatalf("variant part = %#v, %v", part, err)
	}

	empty := &extractor.Image{Describe: agenttypes.ExtractorFunc(func(context.Context, []byte, agenttypes.MediaType) ([]agenttypes.UserPart, error) {
		return nil, nil
	})}
	if _, err := empty.Extract(context.Background(), &types.RawDocument{MIMEType: "image/png", Data: png}); !errors.Is(err, extractor.ErrNoText) {
		t.Fatalf("empty description: %v", err)
	}
}

func TestRegisterPartsIngestsWithAPartExtractor(t *testing.T) {
	auto := extractor.NewAuto()
	auto.RegisterParts("application/vnd.test; v=1", agenttypes.ExtractorFunc(func(_ context.Context, data []byte, _ agenttypes.MediaType) ([]agenttypes.UserPart, error) {
		return []agenttypes.UserPart{agenttypes.Text("decoded: " + string(data))}, nil
	}))
	doc, err := auto.Extract(context.Background(), &types.RawDocument{MIMEType: "application/vnd.test", Data: []byte("abc")})
	if err != nil {
		t.Fatal(err)
	}
	if got := doc.Sections[0].Variants[0]; got.Text != "decoded: abc" || got.ContentType != types.ContentText {
		t.Fatalf("variant = %+v", got)
	}
	if !strings.Contains(extractor.DocumentText(doc), "decoded: abc") {
		t.Fatal("document text lost the part")
	}
}
