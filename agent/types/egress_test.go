package types

import (
	"context"
	"errors"
	"testing"
)

func TestCheckClientSource(t *testing.T) {
	cases := []struct {
		src Source
		ok  bool
	}{
		{Bytes(MediaPNG, []byte("x")), true},
		{Artifact(ArtifactScheme+"abc", MediaPNG), true},
		{URL("https://example.com/a.png"), true},
		{URL("http://example.com/a.png"), true},
		{VendorFileID("anthropic", "", "file_abc", MediaPDF), false},
		{URL("files/abc"), false},
		{URL("file_011abc"), false},
		{URL("gs://bucket/a.png"), false},
		{URL("s3://bucket/a.png"), false},
		{URL("file:///etc/passwd"), false},
		{URL("https://generativelanguage.googleapis.com/v1beta/files/abc"), false},
		{URL("https://api.openai.com/v1/files/file-abc/content"), false},
		{URL("https://storage.googleapis.com/bucket/a.png"), false},
		{URL("https://eu.api.anthropic.com/v1/files/x"), false},
	}
	for _, tc := range cases {
		err := CheckClientSource(tc.src)
		if tc.ok != (err == nil) {
			t.Errorf("%+v: err = %v, want ok %v", tc.src, err, tc.ok)
		}
		if err != nil && !errors.Is(err, ErrUntrustedLocator) {
			t.Errorf("%+v: err = %v, want ErrUntrustedLocator", tc.src, err)
		}
	}
}

func TestCheckClientParts(t *testing.T) {
	ok := []UserPart{Text("hi"), Image(Bytes(MediaPNG, []byte("x")))}
	if err := CheckClientParts(ok); err != nil {
		t.Fatal(err)
	}
	nested := []UserPart{ToolResultPart{CallID: "c", Parts: []ToolOutputPart{Text("r"), Image(URL("gs://b/o"))}}}
	if err := CheckClientParts(nested); !errors.Is(err, ErrUntrustedLocator) {
		t.Fatalf("err = %v, want the nested locator refused", err)
	}
}

func TestEgressContext(t *testing.T) {
	ctx := WithEgress(context.Background(), Egress{RequireText: true})
	if e, ok := EgressFrom(ctx); !ok || !e.RequireText {
		t.Fatal("boundary not carried")
	}
	if _, ok := EgressFrom(WithEgress(ctx, Egress{})); ok {
		t.Fatal("a zero boundary did not clear the one above")
	}
	if (Egress{}).IsOpaque(Text("x")) || !(Egress{}).IsOpaque(Image(Bytes(MediaPNG, nil))) {
		t.Fatal("IsOpaque")
	}
}
