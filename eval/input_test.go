package eval

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestDecodeInputForms(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		kind      InputKind
		text      string
		media     int
		wantError error
	}{
		{name: "text form", raw: `"What is 2+2?"`, kind: InputText, text: "What is 2+2?"},
		{name: "subject object", raw: `{"question":"q"}`, kind: InputOther, text: `{"question":"q"}`},
		{name: "number", raw: `42`, kind: InputOther, text: `42`},
		{name: "empty", raw: ``, kind: InputOther},
		{
			name: "parts with a uri",
			raw:  `{"parts":[{"type":"text","text":"Describe it."},{"type":"image","source":{"media_type":"image/png","uri":"file:img/a.png"}}]}`,
			kind: InputParts, text: "Describe it.", media: 1,
		},
		{
			name:      "inline bytes need the flag",
			raw:       `{"parts":[{"type":"image","source":{"media_type":"image/png","data":"aGVsbG8="}}]}`,
			wantError: ErrInlineInput,
		},
		{
			name: "small explicit inline bytes",
			raw:  `{"inline":true,"parts":[{"type":"image","source":{"media_type":"image/png","data":"aGVsbG8="}}]}`,
			kind: InputParts, media: 1,
		},
		{
			name:      "a part a user cannot send",
			raw:       `{"parts":[{"type":"refusal","text":"no"}]}`,
			wantError: types.ErrPartRole,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in, err := DecodeInput(json.RawMessage(tt.raw))
			if tt.wantError != nil {
				if !errors.Is(err, tt.wantError) {
					t.Fatalf("err = %v, want %v", err, tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if in.Kind != tt.kind || in.Text() != tt.text || len(in.Media()) != tt.media {
				t.Fatalf("got kind %s text %q media %d", in.Kind, in.Text(), len(in.Media()))
			}
		})
	}
}

func TestDecodeInputCapsInlineBytes(t *testing.T) {
	big := types.Image(types.Bytes(types.MediaPNG, make([]byte, MaxInlineInputBytes+1)))
	raw, err := types.MarshalPartInline(big)
	if err != nil {
		t.Fatal(err)
	}
	row := `{"inline":true,"parts":[` + string(raw) + `]}`
	if _, err := DecodeInput(json.RawMessage(row)); !errors.Is(err, ErrInlineInput) {
		t.Fatalf("err = %v, want ErrInlineInput", err)
	}
}

func TestEncodeInput(t *testing.T) {
	t.Run("one text part is the text form", func(t *testing.T) {
		raw, err := EncodeInput([]types.UserPart{types.Text("hi")}, InputOptions{})
		if err != nil || string(raw) != `"hi"` {
			t.Fatalf("got %s, %v", raw, err)
		}
	})
	t.Run("bytes are dropped when another locator exists", func(t *testing.T) {
		src := types.Bytes(types.MediaPNG, []byte("png"))
		src.URI = "file:a.png"
		raw, err := EncodeInput([]types.UserPart{types.Text("look"), types.Image(src)}, InputOptions{InlineMax: 1 << 10})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), `"data"`) || strings.Contains(string(raw), `"inline"`) {
			t.Fatalf("bytes written: %s", raw)
		}
		in, err := DecodeInput(raw)
		if err != nil || in.Kind != InputParts || len(in.Media()) != 1 {
			t.Fatalf("round trip: %+v, %v", in, err)
		}
		got, _ := types.SourceOf(in.Media()[0])
		if got.URI != "file:a.png" || got.Digest != src.Digest {
			t.Fatalf("source = %+v", got)
		}
	})
	t.Run("bytes alone are inlined only within the limit", func(t *testing.T) {
		part := types.Image(types.Bytes(types.MediaPNG, []byte("tiny png")))
		if _, err := EncodeInput([]types.UserPart{part}, InputOptions{}); !errors.Is(err, ErrUnlocatedMedia) {
			t.Fatalf("err = %v, want ErrUnlocatedMedia", err)
		}
		raw, err := EncodeInput([]types.UserPart{part}, InputOptions{InlineMax: 64})
		if err != nil {
			t.Fatal(err)
		}
		in, err := DecodeInput(raw)
		if err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		got, _ := types.SourceOf(in.Media()[0])
		if string(got.Inline) != "tiny png" {
			t.Fatalf("inline = %q", got.Inline)
		}
	})
	t.Run("an elided source is kept as a record", func(t *testing.T) {
		part := types.Image(types.Source{MediaType: types.MediaPNG, Digest: "abc"})
		raw, err := EncodeInput([]types.UserPart{part}, InputOptions{})
		if err != nil || !strings.Contains(string(raw), `"sha256":"abc"`) {
			t.Fatalf("got %s, %v", raw, err)
		}
	})
}

func TestResolveInputReadsDatasetFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "img"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "img", "a.png"), []byte("png bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(t.TempDir(), "secret.png"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolvers := map[string]types.Resolver{"file": DirResolver(dir)}
	ctx := context.Background()

	parts := []types.UserPart{types.Text("t"), types.Image(types.URL("file:img/a.png"))}
	got, err := ResolveInput(ctx, parts, resolvers)
	if err != nil {
		t.Fatal(err)
	}
	src, _ := types.SourceOf(got[1])
	if string(src.Inline) != "png bytes" || src.MediaType != types.MediaPNG || src.URI != "file:img/a.png" || src.Digest == "" {
		t.Fatalf("resolved source = %+v", src)
	}
	if orig, _ := types.SourceOf(parts[1]); len(orig.Inline) != 0 {
		t.Fatal("the input parts were modified")
	}

	escape := []types.UserPart{types.Image(types.URL("file:../secret.png", types.MediaPNG))}
	if _, err := ResolveInput(ctx, escape, resolvers); err == nil {
		t.Fatal("a path outside the dataset directory was read")
	}

	wrong := types.URL("file:img/a.png", types.MediaPNG)
	wrong.Digest = "0000"
	if _, err := ResolveInput(ctx, []types.UserPart{types.Image(wrong)}, resolvers); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("digest mismatch not reported: %v", err)
	}

	untouched := []types.UserPart{types.Image(types.URL("https://example.com/a.png", types.MediaPNG))}
	got, err = ResolveInput(ctx, untouched, resolvers)
	if err != nil {
		t.Fatal(err)
	}
	if src, _ := types.SourceOf(got[0]); len(src.Inline) != 0 {
		t.Fatal("a scheme without a resolver was fetched")
	}
}
