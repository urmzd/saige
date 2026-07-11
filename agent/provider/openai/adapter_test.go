package openai

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestFileContentToPartPDFIsNativeFilePart(t *testing.T) {
	pdf := []byte("%PDF-1.4 fake")
	part := fileContentToPart(types.FileContent{
		MediaType: types.MediaPDF,
		Data:      pdf,
		Filename:  "paper.pdf",
	})
	if part.OfFile == nil {
		t.Fatal("PDF FileContent must map to a native file part, not text")
	}
	wantData := "data:application/pdf;base64," + base64.StdEncoding.EncodeToString(pdf)
	if got := part.OfFile.File.FileData.Value; got != wantData {
		t.Errorf("file_data = %q, want %q", got, wantData)
	}
	if got := part.OfFile.File.Filename.Value; got != "paper.pdf" {
		t.Errorf("filename = %q, want paper.pdf", got)
	}
}

func TestFileContentToPartPDFDefaultsFilename(t *testing.T) {
	part := fileContentToPart(types.FileContent{MediaType: types.MediaPDF, Data: []byte("%PDF-")})
	if part.OfFile == nil {
		t.Fatal("expected a file part")
	}
	if got := part.OfFile.File.Filename.Value; got != "document.pdf" {
		t.Errorf("filename = %q, want document.pdf", got)
	}
}

func TestFileContentToPartImageAndFallback(t *testing.T) {
	img := fileContentToPart(types.FileContent{MediaType: types.MediaPNG, Data: []byte{0x89}})
	if img.OfImageURL == nil {
		t.Fatal("PNG must map to an image part")
	}
	// Non-native media without a native mapping degrades to text.
	txt := fileContentToPart(types.FileContent{MediaType: types.MediaCSV, Data: []byte("a,b"), Filename: "d.csv"})
	if txt.OfText == nil {
		t.Fatalf("CSV should degrade to text, got %+v", txt)
	}
}

func TestContentSupportClaimsMatchMapping(t *testing.T) {
	support := (&Adapter{}).ContentSupport()
	for _, mt := range []types.MediaType{types.MediaJPEG, types.MediaPNG, types.MediaGIF, types.MediaWebP, types.MediaPDF} {
		if !support.Supports(mt) {
			t.Errorf("expected native support for %s", mt)
		}
	}
	if support.Supports(types.MediaCSV) {
		t.Error("CSV must not be claimed native")
	}
}

func TestGenerateSingleTurnText(t *testing.T) {
	chunks := []string{
		`{"id":"c1","object":"chat.completion.chunk","model":"gpt-test","choices":[{"index":0,"delta":{"role":"assistant","content":"hello "}}]}`,
		`{"id":"c1","object":"chat.completion.chunk","model":"gpt-test","choices":[{"index":0,"delta":{"content":"world"}}]}`,
		`{"id":"c1","object":"chat.completion.chunk","model":"gpt-test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	adapter := NewAdapter("test-key", "gpt-test", WithBaseURL(server.URL))
	got, err := adapter.Generate(context.Background(), "say hello")
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello world" {
		t.Errorf("Generate = %q, want 'hello world'", got)
	}
}

func TestGenerateSurfacesAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"nope"}}`, http.StatusUnauthorized)
	}))
	defer server.Close()

	adapter := NewAdapter("bad-key", "gpt-test", WithBaseURL(server.URL))
	if _, err := adapter.Generate(context.Background(), "hi"); err == nil {
		t.Fatal("expected an error from a 401 response")
	}
}
