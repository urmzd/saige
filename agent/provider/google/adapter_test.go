package google

import (
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestToGeminiContentsPDFIsNativeInlineData(t *testing.T) {
	pdf := []byte("%PDF-1.4 fake")
	doc := types.Bytes(types.MediaPDF, pdf)
	doc.Filename = "paper.pdf"
	msgs := []types.Message{types.UserMsg(types.Text("summarize this"), types.Document(doc))}

	_, contents := toGeminiContents(msgs)
	if len(contents) != 1 || len(contents[0].Parts) != 2 {
		t.Fatalf("contents = %+v, want one user content with 2 parts", contents)
	}
	blob := contents[0].Parts[1].InlineData
	if blob == nil {
		t.Fatal("a PDF document part must map to a native inline-data part")
	}
	if blob.MIMEType != string(types.MediaPDF) || string(blob.Data) != string(pdf) {
		t.Errorf("inline data = %q %d bytes, want application/pdf with raw PDF bytes", blob.MIMEType, len(blob.Data))
	}
}

func TestContentSupportClaimsMatchMapping(t *testing.T) {
	support := (&Adapter{}).ContentSupport()
	for _, mt := range []types.MediaType{types.MediaJPEG, types.MediaPNG, types.MediaGIF, types.MediaWebP, types.MediaPDF} {
		if !support.Supports(mt) {
			t.Errorf("expected native support for %s", mt)
		}
	}
}
