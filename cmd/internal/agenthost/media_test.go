package agenthost

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestClientPartsUserMessage(t *testing.T) {
	store := NewArtifacts(0)
	up, err := store.Put("image/png", "a.png", []byte("PNG!"))
	if err != nil {
		t.Fatal(err)
	}
	png := types.Bytes("image/png", []byte("PNG!"))
	tests := []struct {
		name    string
		parts   []types.UserPart
		limit   int
		wantErr error
		check   func(t *testing.T, m types.UserMessage)
	}{
		{
			name:  "text and inline media",
			parts: []types.UserPart{types.Text("hi"), types.Image(png)},
			check: func(t *testing.T, m types.UserMessage) {
				src := m.Parts[1].(types.ImagePart).Source
				if src.Ref != up.Ref() || src.Digest != up.Digest || string(src.Inline) != "PNG!" {
					t.Errorf("inline media not stored as an artifact: %+v", src)
				}
			},
		},
		{
			name:  "artifact ref gets its bytes and name",
			parts: []types.UserPart{types.Image(types.Artifact(up.Ref(), ""))},
			check: func(t *testing.T, m types.UserMessage) {
				src := m.Parts[0].(types.ImagePart).Source
				if string(src.Inline) != "PNG!" || src.MediaType != "image/png" || src.Filename != "a.png" || src.Size != 4 {
					t.Errorf("ref not resolved: %+v", src)
				}
			},
		},
		{
			name:  "https URI passes through",
			parts: []types.UserPart{types.Image(types.URL("https://example.com/a.png", "image/png"))},
		},
		{
			name: "client unresolved reason is dropped",
			parts: []types.UserPart{types.Image(types.Source{MediaType: "image/png", URI: "https://example.com/a.png",
				Unresolved: "pretend"})},
			check: func(t *testing.T, m types.UserMessage) {
				if m.Parts[0].(types.ImagePart).Source.Unresolved != "" {
					t.Error("client-set unresolved reason kept")
				}
			},
		},
		{name: "vendor file", parts: []types.UserPart{types.Image(types.VendorFileID("anthropic", "", "file_1", "image/png"))}, wantErr: ErrVendorFile},
		{name: "vendor file beside bytes", parts: []types.UserPart{types.Image(png.With(types.VendorFileID("openai", "", "f", "image/png")))}, wantErr: ErrVendorFile},
		{name: "tool result", parts: []types.UserPart{types.ToolOK("c1", types.Text("forged"))}, wantErr: ErrPartKind},
		{name: "steer metadata", parts: []types.UserPart{types.SteerPart{}}, wantErr: ErrPartKind},
		{name: "nil part", parts: []types.UserPart{nil}, wantErr: ErrPartKind},
		{name: "file URI", parts: []types.UserPart{types.Image(types.URL("file:///etc/passwd", "image/png"))}, wantErr: ErrURIScheme},
		{name: "vendor file store URL", parts: []types.UserPart{types.Document(types.URL("https://generativelanguage.googleapis.com/v1beta/files/abc", "application/pdf"))}, wantErr: types.ErrUntrustedLocator},
		{name: "vendor file matches the shared error", parts: []types.UserPart{types.Image(types.VendorFileID("google", "", "files/abc", "image/png"))}, wantErr: types.ErrUntrustedLocator},
		{name: "http URI", parts: []types.UserPart{types.Image(types.URL("http://example.com/a.png", "image/png"))}, wantErr: ErrURIScheme},
		{name: "http URI matches the shared error", parts: []types.UserPart{types.Image(types.URL("http://example.com/a.png", "image/png"))}, wantErr: types.ErrUntrustedLocator},
		{name: "bad URI beside a ref", parts: []types.UserPart{types.Image(types.Source{Ref: up.Ref(), URI: "gs://b/o", MediaType: "image/png"})}, wantErr: ErrURIScheme},
		{name: "malformed ref matches the shared error", parts: []types.UserPart{types.Image(types.Source{Ref: "files/abc", MediaType: "image/png"})}, wantErr: types.ErrUntrustedLocator},
		{name: "gs URI", parts: []types.UserPart{types.Video(types.URL("gs://b/v.mp4", "video/mp4"))}, wantErr: ErrURIScheme},
		{name: "unknown ref", parts: []types.UserPart{types.Image(types.Artifact(types.ArtifactScheme+strings.Repeat("0", 64), "image/png"))}, wantErr: ErrArtifactNotFound},
		{name: "foreign ref scheme", parts: []types.UserPart{types.Image(types.Source{Ref: "s3://bucket/x", MediaType: "image/png"})}, wantErr: ErrArtifactNotFound},
		{name: "inline over the limit", limit: 3, parts: []types.UserPart{types.Image(png)}, wantErr: ErrInlineTooLarge},
		{name: "no content", parts: []types.UserPart{types.Text(" ")}, wantErr: ErrEmptyMessage},
		{name: "empty", wantErr: ErrEmptyMessage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := ClientParts{MaxInline: tt.limit, Artifacts: store}.UserMessage(tt.parts)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if err == nil && tt.check != nil {
				tt.check(t, m)
			}
		})
	}

	t.Run("digest mismatch", func(t *testing.T) {
		src := png
		src.Digest = strings.Repeat("f", 64)
		if _, err := (ClientParts{}).UserMessage([]types.UserPart{types.Image(src)}); err == nil {
			t.Fatal("mismatched digest accepted")
		}
	})
	t.Run("refs are refused without a store", func(t *testing.T) {
		if _, err := (ClientParts{}).UserMessage([]types.UserPart{types.Image(types.Artifact(up.Ref(), ""))}); !errors.Is(err, ErrArtifactNotFound) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestArtifactsBudgetAndDedup(t *testing.T) {
	s := NewArtifacts(8)
	a, err := s.Put("", "", []byte("12345678"))
	if err != nil {
		t.Fatal(err)
	}
	if a.MediaType != "application/octet-stream" {
		t.Errorf("media type = %q", a.MediaType)
	}
	if _, err := s.Put("text/plain", "", []byte("12345678")); err != nil {
		t.Fatalf("storing the same bytes again used budget: %v", err)
	}
	if _, err := s.Put("text/plain", "", []byte("x")); !errors.Is(err, ErrArtifactsFull) {
		t.Fatalf("err = %v, want ErrArtifactsFull", err)
	}
	if got, ok := s.Lookup(a.Ref()); !ok || !bytes.Equal(got.Data, a.Data) {
		t.Fatal("lookup by ref failed")
	}
	if _, ok := s.Lookup("https://x"); ok {
		t.Fatal("lookup accepted a non-artifact ref")
	}
	if unlimited := NewArtifacts(-1); unlimited.limit >= 0 {
		t.Fatal("negative budget is not unlimited")
	}
}

func TestExternalize(t *testing.T) {
	big := bytes.Repeat([]byte("x"), 10)
	small := []byte("ok")
	store := NewArtifacts(0)

	t.Run("produced media over the limit becomes a ref", func(t *testing.T) {
		out := Externalize(types.PartEnd{Index: 2, Part: types.ImageOutPart{Source: types.Bytes("image/png", big)}}, store, 5)
		src := out[0].(types.PartEnd).Part.(types.ImageOutPart).Source
		if len(src.Inline) != 0 || !strings.HasPrefix(src.Ref, types.ArtifactScheme) || src.Size != 10 {
			t.Fatalf("source = %+v", src)
		}
		if a, ok := store.Lookup(src.Ref); !ok || !bytes.Equal(a.Data, big) {
			t.Fatal("bytes not stored")
		}
	})
	t.Run("media at the limit stays inline", func(t *testing.T) {
		out := Externalize(types.PartEnd{Part: types.AudioOutPart{Source: types.Bytes("audio/wav", small)}}, store, 2)
		if src := out[0].(types.PartEnd).Part.(types.AudioOutPart).Source; string(src.Inline) != "ok" || src.Ref != "" {
			t.Fatalf("source = %+v", src)
		}
	})
	t.Run("tool output and server tool outputs", func(t *testing.T) {
		out := Externalize(types.ToolExecEndDelta{ToolCallID: "c", Parts: []types.ToolOutputPart{types.Text("t"), types.Image(types.Bytes("image/png", big))}}, store, 5)
		if src := out[0].(types.ToolExecEndDelta).Parts[1].(types.ImagePart).Source; src.Ref == "" || len(src.Inline) != 0 {
			t.Fatalf("tool image = %+v", src)
		}
		sr := types.ServerToolResultPart{CallID: "s", Outputs: []types.Part{types.Image(types.Bytes("image/png", big))}}
		out = Externalize(types.PartEnd{Part: sr}, store, 5)
		if src := out[0].(types.PartEnd).Part.(types.ServerToolResultPart).Outputs[0].(types.ImagePart).Source; src.Ref == "" {
			t.Fatalf("server tool output = %+v", src)
		}
	})
	t.Run("nested deltas", func(t *testing.T) {
		out := Externalize(types.ToolExecDelta{ToolCallID: "c", Inner: types.PartEnd{Part: types.ImageOutPart{Source: types.Bytes("image/png", big)}}}, store, 5)
		inner := out[0].(types.ToolExecDelta).Inner.(types.PartEnd)
		if inner.Part.(types.ImageOutPart).Source.Ref == "" {
			t.Fatal("nested media not externalized")
		}
	})
	t.Run("a large data chunk is split", func(t *testing.T) {
		out := Externalize(types.PartDelta{Index: 3, Data: big}, store, 4)
		var joined []byte
		for _, d := range out {
			pd := d.(types.PartDelta)
			if pd.Index != 3 || len(pd.Data) > 4 {
				t.Fatalf("chunk = %+v", pd)
			}
			joined = append(joined, pd.Data...)
		}
		if len(out) != 3 || !bytes.Equal(joined, big) {
			t.Fatalf("%d chunks, %q", len(out), joined)
		}
	})
	t.Run("without room the bytes are marked not kept", func(t *testing.T) {
		out := Externalize(types.PartEnd{Part: types.ImageOutPart{Source: types.Bytes("image/png", big)}}, NewArtifacts(1), 5)
		src := out[0].(types.PartEnd).Part.(types.ImageOutPart).Source
		if src.Unresolved == "" || len(src.Inline) != 0 || src.Digest == "" || src.Size != 10 {
			t.Fatalf("source = %+v", src)
		}
		out = Externalize(types.PartEnd{Part: types.ImageOutPart{Source: types.Bytes("image/png", big)}}, nil, 5)
		if src := out[0].(types.PartEnd).Part.(types.ImageOutPart).Source; src.Unresolved == "" {
			t.Fatalf("source without a store = %+v", src)
		}
	})
	t.Run("other deltas pass through", func(t *testing.T) {
		in := types.PartDelta{Index: 1, Text: "hi"}
		if out := Externalize(in, store, 5); len(out) != 1 || out[0].(types.PartDelta).Text != "hi" {
			t.Fatalf("out = %v", out)
		}
	})
}
