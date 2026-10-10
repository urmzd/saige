package main

import (
	"context"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestIngestMIME(t *testing.T) {
	for file, want := range map[string]string{
		"notes.md":    "text/markdown",
		"README":      "text/plain",
		"guide.HTML":  "text/html",
		"paper.pdf":   "application/pdf",
		"chart.png":   "image/png",
		"photo.jpeg":  "image/jpeg",
		"unknown.zzz": "text/plain",
	} {
		if got := ingestMIME(file); got != want {
			t.Errorf("ingestMIME(%q) = %q, want %q", file, got, want)
		}
	}
}

func TestJudgeConversion(t *testing.T) {
	native, err := judgeConversion("native")
	if err != nil || !native.IsZero() {
		t.Fatalf("native = %+v, %v", native, err)
	}
	extract, err := judgeConversion("extract")
	if err != nil || len(extract.Converters) != 1 || extract.Dial.Per[types.ModalityDocument][0] != types.ActExtract {
		t.Fatalf("extract = %+v, %v", extract, err)
	}
	omit, err := judgeConversion("omit")
	if err != nil || omit.Dial.Default != types.ActOmit {
		t.Fatalf("omit = %+v, %v", omit, err)
	}
	if _, err := judgeConversion("describe"); err == nil {
		t.Fatal("an unknown mode was accepted")
	}
}

func TestLazyDescriberResolvesOnce(t *testing.T) {
	calls := 0
	ex := lazyDescriber(func() (types.Provider, error) {
		calls++
		return nil, context.Canceled
	})
	for range 2 {
		if _, err := ex.Extract(context.Background(), []byte("png"), types.MediaPNG); err == nil {
			t.Fatal("no error without a provider")
		}
	}
	if calls != 1 {
		t.Fatalf("provider built %d times", calls)
	}
}
