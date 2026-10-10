package convert

import (
	"context"
	"testing"

	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/rag/extractor"
	ragtypes "github.com/urmzd/saige/rag/types"
)

func TestAsExtractorServesTheRAGRegistry(t *testing.T) {
	f := &fake{action: types.ActDescribe, media: types.ModalityImage, text: "a red square"}
	auto := extractor.NewAuto()
	auto.RegisterImages(AsExtractor(f))
	doc, err := auto.Extract(context.Background(), &ragtypes.RawDocument{MIMEType: "image/png", Data: []byte("png")})
	if err != nil {
		t.Fatal(err)
	}
	if v := doc.Sections[0].Variants[0]; v.Text != "a red square" || v.ContentType != ragtypes.ContentImage || f.calls.Load() != 1 {
		t.Fatalf("variant = %+v, calls %d", v, f.calls.Load())
	}
	if _, err := AsExtractor(f).Extract(context.Background(), []byte("wav"), types.MediaWAV); err == nil {
		t.Fatal("a converter ran on media it does not accept")
	}
}
