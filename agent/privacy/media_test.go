package privacy

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

func docPart(mt types.MediaType, text string) types.DocumentPart {
	src := types.Bytes(mt, []byte(text))
	src.URI = "https://example.com/raw"
	src.Ref = types.ArtifactScheme + src.Digest
	src.Files = []types.VendorFile{{Provider: "anthropic", ID: "file_raw"}}
	src.Filename = "notes.txt"
	return types.DocumentPart{Source: src}
}

func TestTokenizeTextBearingMedia(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		mt     types.MediaType
		text   string
		wantMT types.MediaType
	}{
		{"plain text", "text/plain; charset=utf-8", "write to ada@example.com", "text/plain; charset=utf-8"},
		{"csv", "text/csv", "name,email\nada,ada@example.com\n", "text/csv"},
		{"json", "application/json", `{"email":"ada@example.com"}`, "application/json"},
		{"json made invalid", "application/json", `{"a@example.com":1}`, "application/json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := NewVault(nil)
			orig := docPart(tc.mt, tc.text)
			msgs, err := TokenizeMessages(ctx, v, []types.Message{types.UserMsg(orig)})
			if err != nil {
				t.Fatal(err)
			}
			got := msgs[0].(types.UserMessage).Parts[0].(types.DocumentPart).Source
			body := string(got.Inline)
			if strings.Contains(body, "example.com") || !strings.Contains(body, "<<EMAIL_1>>") {
				t.Fatalf("bytes = %q, want the address tokenized", body)
			}
			if got.URI != "" || got.Ref != "" || len(got.Files) != 0 {
				t.Errorf("source %+v keeps a locator that reaches the original text", got)
			}
			if got.Digest == orig.Source.Digest || got.Digest != types.Bytes(got.MediaType, got.Inline).Digest {
				t.Errorf("digest %q does not address the tokenized bytes", got.Digest)
			}
			if got.Filename != "notes.txt" {
				t.Errorf("filename = %q", got.Filename)
			}
			if strings.Contains(string(tc.mt), "json") {
				if json.Valid(got.Inline) && got.MediaType != tc.wantMT {
					t.Errorf("valid JSON relabeled %s", got.MediaType)
				}
				if !json.Valid(got.Inline) && !strings.HasPrefix(string(got.MediaType), "text/plain") {
					t.Errorf("invalid JSON sent as %s", got.MediaType)
				}
			} else if got.MediaType != tc.wantMT {
				t.Errorf("media type = %s", got.MediaType)
			}
			if string(orig.Source.Inline) != tc.text {
				t.Error("the caller's part was modified")
			}
		})
	}

	// Nothing found: the part is sent as it was, with every locator.
	clean := docPart("text/plain", "nothing personal")
	msgs, _ := TokenizeMessages(ctx, NewVault(nil), []types.Message{types.UserMsg(clean)})
	if got := msgs[0].(types.UserMessage).Parts[0].(types.DocumentPart).Source; got.URI == "" || got.Digest != clean.Source.Digest {
		t.Errorf("an untouched document lost its locators: %+v", got)
	}

	// Binary or non-UTF-8 bytes are opaque, not text.
	for _, p := range []types.Part{
		types.DocumentPart{Source: types.Bytes(types.MediaPDF, []byte("%PDF ada@example.com"))},
		types.DocumentPart{Source: types.Bytes("text/plain", []byte{0xff, 0xfe, 'a'})},
		types.DocumentPart{Source: types.URL("https://example.com/notes.txt", "text/plain")},
		pngImage(),
	} {
		if !opaque(p) {
			t.Errorf("%v counted as tokenizable", p)
		}
	}
	if opaque(docPart("text/markdown", "x")) || opaque(types.FilePart{Source: types.Bytes("application/ld+json", []byte("{}"))}) {
		t.Error("text-bearing media counted as opaque")
	}
}

func pngImage() types.ImagePart {
	return types.ImagePart{Source: types.Bytes(types.MediaPNG, []byte("png"))}
}

func TestToolRedactorTokenizesTextBearingMedia(t *testing.T) {
	r := NewToolRedactor(NewVault(nil))
	res := r.TokenizeResult(context.Background(), types.ToolDef{Name: "read"}, types.ToolResult{Parts: []types.ToolOutputPart{
		docPart("text/plain", "contact ada@example.com"), pngImage(),
	}})
	doc := res.Parts[0].(types.DocumentPart)
	if strings.Contains(string(doc.Source.Inline), "ada@") {
		t.Fatalf("tool result document not tokenized: %q", doc.Source.Inline)
	}
	if _, ok := res.Parts[1].(types.ImagePart); !ok {
		t.Fatal("image dropped")
	}
}

func TestTokenizeAssistantParts(t *testing.T) {
	v := NewVault(nil)
	msgs, err := TokenizeMessages(context.Background(), v, []types.Message{types.AssistantMsg(
		types.RefusalPart{Text: "I will not email ada@example.com"},
		types.CitationPart{Citation: types.Citation{Quote: "ada@example.com wrote", Title: "t"}},
		types.AudioOutPart{Source: types.Bytes(types.MediaWAV, []byte("pcm")), Transcript: "mail ada@example.com"},
		types.ThinkingPart{Text: "ada@example.com", Signature: "sig"},
	)})
	if err != nil {
		t.Fatal(err)
	}
	parts := msgs[0].(types.AssistantMessage).Parts
	if s := parts[0].(types.RefusalPart).Text; strings.Contains(s, "ada@") {
		t.Errorf("refusal = %q", s)
	}
	if s := parts[1].(types.CitationPart).Citation.Quote; strings.Contains(s, "ada@") {
		t.Errorf("quote = %q", s)
	}
	if s := parts[2].(types.AudioOutPart).Transcript; strings.Contains(s, "ada@") {
		t.Errorf("transcript = %q", s)
	}
	if s := parts[3].(types.ThinkingPart).Text; s != "ada@example.com" {
		t.Errorf("thinking rewritten: %q", s)
	}
}

func TestMediaPolicy(t *testing.T) {
	image := []types.Message{types.UserMsg(types.Text("look"), pngImage())}
	nested := []types.Message{types.UserMsg(types.ToolResultPart{CallID: "c", Parts: []types.ToolOutputPart{types.Text("ok"), pngImage()}})}
	textDoc := []types.Message{types.UserMsg(docPart("text/plain", "ada@example.com"))}
	sensitiveVault := func() *MemoryVault {
		v := NewVault(nil)
		v.SetSensitive(true)
		return v
	}
	cases := []struct {
		name    string
		vault   *MemoryVault
		policy  MediaPolicy
		msgs    []types.Message
		refused bool
	}{
		{"default passes", NewVault(nil), MediaDefault, image, false},
		{"sensitive refuses by default", sensitiveVault(), MediaDefault, image, true},
		{"sensitive refuses nested media", sensitiveVault(), MediaDefault, nested, true},
		{"sensitive with pass", sensitiveVault(), MediaPass, image, false},
		{"refuse", NewVault(nil), MediaRefuse, image, true},
		{"refuse sends tokenized text media", NewVault(nil), MediaRefuse, textDoc, false},
		{"require_text without a converter", NewVault(nil), MediaRequireText, image, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inner := &fakeProvider{script: []types.Delta{types.PartStart{Index: 0, Kind: types.KindText}, types.PartEnd{Index: 0}}}
			p := must.Get(New(inner, Config{Vault: tc.vault}))
			p.Media = tc.policy
			ch, err := p.Stream(context.Background(), types.Request{Messages: tc.msgs})
			if tc.refused {
				if !errors.Is(err, ErrMediaRefused) {
					t.Fatalf("err = %v, want ErrMediaRefused", err)
				}
				if inner.got != nil {
					t.Fatal("a refused request was sent")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			collect(ch)
			if inner.got == nil {
				t.Fatal("not sent")
			}
		})
	}
}

// planningProvider stands in for a conversion decorator: it records the
// boundary its context carried.
type planningProvider struct {
	fakeProvider
	egress types.Egress
	had    bool
}

func (p *planningProvider) PlanConversions(context.Context, types.Request) (types.ConversionReport, types.ConversionEstimate, error) {
	return types.ConversionReport{}, types.ConversionEstimate{}, nil
}

func (p *planningProvider) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	p.egress, p.had = types.EgressFrom(ctx)
	return p.fakeProvider.Stream(ctx, req)
}

func TestProviderSetsTheBoundary(t *testing.T) {
	for _, tc := range []struct {
		policy      MediaPolicy
		requireText bool
	}{{MediaPass, false}, {MediaRequireText, true}} {
		inner := &planningProvider{}
		v := NewVault(nil)
		p := must.Get(New(inner, Config{Vault: v}))
		p.Media = tc.policy
		ch, err := p.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(pngImage())}})
		if err != nil {
			t.Fatalf("%s: %v", tc.policy, err)
		}
		collect(ch)
		if !inner.had || inner.egress.Vault != Vault(v) || inner.egress.RequireText != tc.requireText {
			t.Fatalf("%s: boundary %+v (set %v)", tc.policy, inner.egress, inner.had)
		}
		if inner.egress.IsOpaque(docPart("text/plain", "x")) || !inner.egress.IsOpaque(pngImage()) {
			t.Fatalf("%s: boundary counts media wrongly", tc.policy)
		}
	}
}

func TestRestoreByPartIndex(t *testing.T) {
	v := NewVault(nil)
	ctx := context.Background()
	if _, err := v.Tokenize(ctx, "ada@example.com bob@example.com"); err != nil {
		t.Fatal(err)
	}
	inner := &fakeProvider{script: []types.Delta{
		types.PartStart{Index: 0, Kind: types.KindText},
		types.PartStart{Index: 2, Kind: types.KindText},
		types.PartStart{Index: 3, Kind: types.KindRefusal},
		types.PartStart{Index: 4, Kind: types.KindAudioOut},
		types.PartStart{Index: 1, Kind: types.KindThinking},
		// Two text parts interleave, each with a placeholder split across
		// fragments of its own index.
		types.PartDelta{Index: 0, Text: "to <<EMA"},
		types.PartDelta{Index: 2, Text: "cc <<EMAIL"},
		types.PartDelta{Index: 0, Text: "IL_1>> ok"},
		types.PartDelta{Index: 1, Thinking: "<<EMAIL_1>>"},
		types.PartDelta{Index: 3, Refusal: "not <<EMAIL_"},
		types.PartDelta{Index: 4, Transcript: "say <<EM"},
		types.PartDelta{Index: 2, Text: "_2>>"},
		types.PartDelta{Index: 3, Refusal: "2>>"},
		types.PartDelta{Index: 4, Transcript: "AIL_1>>"},
		types.PartEnd{Index: 0, Part: types.TextPart{Text: "to <<EMAIL_1>> ok"}},
		types.PartEnd{Index: 1, Part: types.ThinkingPart{Text: "<<EMAIL_1>>", Signature: "s"}},
		types.PartEnd{Index: 2, Part: types.TextPart{Text: "cc <<EMAIL_2>>"}},
		types.PartEnd{Index: 3, Part: types.RefusalPart{Text: "not <<EMAIL_2>>"}},
		types.PartEnd{Index: 4, Part: types.AudioOutPart{Transcript: "say <<EMAIL_1>>"}},
		types.PartStart{Index: 5, Kind: types.KindCitation},
		types.PartEnd{Index: 5, Part: types.CitationPart{Citation: types.Citation{Quote: "<<EMAIL_2>> said"}}},
	}}
	ch, err := must.Get(New(inner, Config{Vault: v})).Stream(ctx, types.Request{Messages: []types.Message{types.UserMsg(types.Text("hi"))}})
	if err != nil {
		t.Fatal(err)
	}
	streamed := map[int]string{}
	ends := map[int]types.AssistantPart{}
	for _, d := range collect(ch) {
		switch x := d.(type) {
		case types.PartDelta:
			streamed[x.Index] += x.Text + x.Thinking + x.Refusal + x.Transcript
		case types.PartEnd:
			ends[x.Index] = x.Part
		}
	}
	want := map[int]string{0: "to ada@example.com ok", 1: "<<EMAIL_1>>", 2: "cc bob@example.com", 3: "not bob@example.com", 4: "say ada@example.com"}
	for i, w := range want {
		if streamed[i] != w {
			t.Errorf("part %d streamed %q, want %q", i, streamed[i], w)
		}
	}
	if ends[0].(types.TextPart).Text != want[0] || ends[2].(types.TextPart).Text != want[2] {
		t.Errorf("text ends = %#v %#v", ends[0], ends[2])
	}
	if ends[1].(types.ThinkingPart).Text != "<<EMAIL_1>>" {
		t.Error("thinking was rewritten")
	}
	if ends[3].(types.RefusalPart).Text != want[3] || ends[4].(types.AudioOutPart).Transcript != want[4] {
		t.Errorf("refusal/transcript ends = %#v %#v", ends[3], ends[4])
	}
	if q := ends[5].(types.CitationPart).Citation.Quote; q != "bob@example.com said" {
		t.Errorf("citation quote = %q", q)
	}
}

func TestSensitiveVaultRefusesAudioOut(t *testing.T) {
	script := []types.Delta{
		types.PartStart{Index: 0, Kind: types.KindText},
		types.PartDelta{Index: 0, Text: "here"},
		types.PartEnd{Index: 0},
		types.PartStart{Index: 1, Kind: types.KindAudioOut, MediaType: types.MediaWAV},
		types.PartDelta{Index: 1, Data: []byte("pcm")},
		types.PartEnd{Index: 1},
	}
	v := NewVault(nil)
	v.SetSensitive(true)
	p := must.Get(New(&fakeProvider{script: script}, Config{Vault: v}))
	p.Media = MediaPass
	ch, err := p.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("speak"))}})
	if err != nil {
		t.Fatal(err)
	}
	deltas := collect(ch)
	last, ok := deltas[len(deltas)-1].(types.ErrorDelta)
	if !ok || !errors.Is(last.Error, ErrAudioOutRefused) {
		t.Fatalf("last delta = %#v, want ErrAudioOutRefused", deltas[len(deltas)-1])
	}
	for _, d := range deltas {
		if pd, ok := d.(types.PartDelta); ok && pd.Index == 1 {
			t.Fatal("audio bytes were forwarded")
		}
	}

	p.AllowAudioOut = true
	ch, _ = p.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("speak"))}})
	for _, d := range collect(ch) {
		if _, ok := d.(types.ErrorDelta); ok {
			t.Fatal("audio refused although allowed")
		}
	}
}

// A snapshot written before the vault had a sensitivity setting loads, and
// sensitivity is configuration that a snapshot never carries.
func TestLoadVaultSnapshotV1(t *testing.T) {
	const golden = `{"v":1,"entries":[{"token":"\u003c\u003cEMAIL_1\u003e\u003e","label":"EMAIL","value":"ada@example.com"}],"counters":{"EMAIL":1}}`
	v, err := LoadVault(nil, []byte(golden))
	if err != nil {
		t.Fatal(err)
	}
	if got := v.Restore("<<EMAIL_1>>"); got != "ada@example.com" {
		t.Fatalf("restore = %q", got)
	}
	v.SetSensitive(true)
	snap, _ := v.Snapshot()
	if string(snap) != golden {
		t.Fatalf("snapshot = %s, want the golden form unchanged", snap)
	}
}
