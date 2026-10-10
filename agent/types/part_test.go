package types

import (
	"reflect"
	"testing"
)

// Role seals are compile-time guarantees: these assignments are the
// accepted placements of each part.
var (
	_ SystemPart     = TextPart{}
	_ UserPart       = TextPart{}
	_ AssistantPart  = TextPart{}
	_ ToolOutputPart = TextPart{}
	_ ToolOutputPart = JSONPart{}

	_ UserPart       = ImagePart{}
	_ ToolOutputPart = ImagePart{}
	_ UserPart       = AudioPart{}
	_ ToolOutputPart = AudioPart{}
	_ UserPart       = VideoPart{}
	_ UserPart       = DocumentPart{}
	_ ToolOutputPart = DocumentPart{}
	_ UserPart       = FilePart{}
	_ ToolOutputPart = FilePart{}

	_ SystemPart = ToolResultPart{}
	_ UserPart   = ToolResultPart{}

	_ AssistantPart = ThinkingPart{}
	_ AssistantPart = ToolCallPart{}
	_ AssistantPart = ServerToolCallPart{}
	_ AssistantPart = ServerToolResultPart{}
	_ AssistantPart = CitationPart{}
	_ AssistantPart = AudioOutPart{}
	_ AssistantPart = ImageOutPart{}
	_ AssistantPart = VideoOutPart{}
	_ AssistantPart = RefusalPart{}

	_ SystemPart    = ConfigPart{}
	_ UserPart      = ConfigPart{}
	_ SystemPart    = HandoffPart{}
	_ UserPart      = HandoffPart{}
	_ UserPart      = FeedbackPart{}
	_ UserPart      = SteerPart{}
	_ AssistantPart = TruncationPart{}
	_ SystemPart    = RoutePart{}
	_ AssistantPart = RoutePart{}
	_ SystemPart    = ApprovalPart{}
	_ SystemPart    = CompactionPart{}
	_ SystemPart    = GuardrailPart{}
	_ UserPart      = GuardrailPart{}
	_ AssistantPart = GuardrailPart{}
)

// The seals also exclude: a role can never hold a part meant for another.
func TestRoleSealsExclude(t *testing.T) {
	tests := []struct {
		p                                Part
		system, user, assistant, toolOut bool
	}{
		{Text("x"), true, true, true, true},
		{JSONPart{}, false, false, false, true},
		{ImagePart{}, false, true, false, true},
		{VideoPart{}, false, true, false, false},
		{ToolResultPart{}, true, true, false, false},
		{ToolCallPart{}, false, false, true, false},
		{ThinkingPart{}, false, false, true, false},
		{CitationPart{}, false, false, true, false},
		{ImageOutPart{}, false, false, true, false},
		{RefusalPart{}, false, false, true, false},
		{ConfigPart{}, true, true, false, false},
		{TruncationPart{}, false, false, true, false},
		{ApprovalPart{}, true, false, false, false},
		{FeedbackPart{}, false, true, false, false},
	}
	for _, tt := range tests {
		_, s := tt.p.(SystemPart)
		_, u := tt.p.(UserPart)
		_, a := tt.p.(AssistantPart)
		_, o := tt.p.(ToolOutputPart)
		if s != tt.system || u != tt.user || a != tt.assistant || o != tt.toolOut {
			t.Errorf("%T: system=%v user=%v assistant=%v tool output=%v", tt.p, s, u, a, o)
		}
	}
}

func TestIsMediaAndModality(t *testing.T) {
	tests := []struct {
		p     Part
		media bool
		mod   Modality
		ok    bool
	}{
		{Text("x"), false, ModalityText, true},
		{ToolCallPart{}, false, ModalityText, true},
		{Image(URL("u")), true, ModalityImage, true},
		{AudioOutPart{}, true, ModalityAudio, true},
		{Video(URL("u")), true, ModalityVideo, true},
		{Document(URL("u")), true, ModalityDocument, true},
		{File(URL("u")), true, ModalityFile, true},
		{ConfigPart{}, false, "", false},
		{CitationPart{}, false, "", false},
	}
	for _, tt := range tests {
		if IsMedia(tt.p) != tt.media {
			t.Errorf("IsMedia(%T) = %v", tt.p, !tt.media)
		}
		if m, ok := PartModality(tt.p); m != tt.mod || ok != tt.ok {
			t.Errorf("PartModality(%T) = %q, %v", tt.p, m, ok)
		}
	}
}

func TestMediaDispatch(t *testing.T) {
	tests := []struct {
		mt   MediaType
		want PartKind
	}{
		{MediaPNG, KindImage},
		{"IMAGE/JPEG; q=1", KindImage},
		{MediaWAV, KindAudio},
		{MediaMP4, KindVideo},
		{MediaPDF, KindDocument},
		{MediaCSV, KindDocument},
		{"text/plain; charset=utf-8", KindDocument},
		{MediaDOCX, KindFile},
		{"", KindFile},
	}
	for _, tt := range tests {
		if got := Media(URL("u", tt.mt)).Kind(); got != tt.want {
			t.Errorf("Media(%q) = %s, want %s", tt.mt, got, tt.want)
		}
	}
}

func TestMessageAccessors(t *testing.T) {
	m := AssistantMsg(Text("a"), ToolCallPart{ID: "1", Name: "f"}, Text("b"), RoutePart{Model: "m"})
	if got := TextOf(m); got != "ab" {
		t.Errorf("TextOf = %q", got)
	}
	var idx []int
	for i, c := range Each[ToolCallPart](m) {
		idx = append(idx, i)
		if c.Name != "f" {
			t.Errorf("call = %+v", c)
		}
	}
	if !reflect.DeepEqual(idx, []int{1}) {
		t.Errorf("Each indices = %v", idx)
	}
	if n := len(PartsOf(&m)); n != 4 {
		t.Errorf("PartsOf(pointer) = %d parts", n)
	}
	if PartsOf(nil) != nil {
		t.Error("PartsOf(nil) is not nil")
	}
	for range Each[TextPart](m) {
		break // stopping early must not panic
	}
	sys := ToolResults(ToolOK("c", Text("ok")), ToolErr("d", "bad"))
	if len(sys.Parts) != 2 || !sys.Parts[1].(ToolResultPart).IsError || sys.Parts[1].(ToolResultPart).Text() != "bad" {
		t.Errorf("ToolResults = %#v", sys)
	}
	if u := UserToolResults(ToolOK("c")); len(u.Parts) != 1 {
		t.Errorf("UserToolResults = %#v", u)
	}
	if j, err := JSON(map[string]int{"a": 1}); err != nil || string(j.JSON) != `{"a":1}` {
		t.Errorf("JSON = %s, %v", j.JSON, err)
	}
	if _, err := JSON(func() {}); err == nil {
		t.Error("JSON accepted a function")
	}
}

func TestPairServerTools(t *testing.T) {
	parts := []AssistantPart{
		ServerToolCallPart{ID: "a", ToolKind: ServerToolWebSearch},
		Text("between"),
		ServerToolResultPart{CallID: "b", ToolKind: ServerToolCodeExecution, Text: "orphan"},
		ServerToolResultPart{CallID: "a", Text: "ra"},
	}
	pairs := PairServerTools(parts)
	if len(pairs) != 2 {
		t.Fatalf("pairs = %#v", pairs)
	}
	if pairs[0].Call.ID != "a" || pairs[0].Result == nil || pairs[0].Result.Text != "ra" {
		t.Errorf("pair 0 = %#v", pairs[0])
	}
	if pairs[1].Call.ID != "b" || pairs[1].Call.ToolKind != ServerToolCodeExecution || pairs[1].Result.Text != "orphan" {
		t.Errorf("pair 1 = %#v", pairs[1])
	}
}

func TestPartDeltasStreamEveryKind(t *testing.T) {
	for _, p := range partCases() {
		ap, ok := p.(AssistantPart)
		if !ok || IsMetadata(p) {
			continue
		}
		asm := NewPartAssembler()
		for _, d := range PartDeltas(4, ap) {
			asm.Push(d)
		}
		got := asm.Parts()
		if asm.Violations() != 0 || len(got) != 1 || !reflect.DeepEqual(got[0], ap) {
			t.Errorf("%T streamed back as %#v (violations %d)", p, got, asm.Violations())
		}
	}
	msg := AssistantMsg(Text("a"), ToolCallPart{ID: "c", Name: "f"})
	ds := MessageDeltas(msg)
	if start, ok := ds[0].(PartStart); !ok || start.Index != 0 || start.Kind != KindText {
		t.Errorf("first delta = %#v", ds[0])
	}
	if start, ok := ds[3].(PartStart); !ok || start.Index != 1 || start.ID != "c" || start.Name != "f" {
		t.Errorf("tool start = %#v", ds[3])
	}
}

func TestClonePartIsDeep(t *testing.T) {
	orig := ToolOK("c", Image(Bytes(MediaPNG, []byte{1, 2})), JSONPart{JSON: []byte(`{"a":1}`)})
	orig.Citations = []Citation{{Meta: map[string]any{"k": []any{"v"}}}}
	cp := ClonePart(orig)
	if !reflect.DeepEqual(cp, orig) {
		t.Fatalf("clone differs: %#v", cp)
	}
	cp.Parts[0].(ImagePart).Source.Inline[0] = 9
	cp.Parts[1].(JSONPart).JSON[2] = 'b'
	cp.Citations[0].Meta["k"].([]any)[0] = "changed"
	if orig.Parts[0].(ImagePart).Source.Inline[0] != 1 || string(orig.Parts[1].(JSONPart).JSON) != `{"a":1}` ||
		orig.Citations[0].Meta["k"].([]any)[0] != "v" {
		t.Fatalf("mutating the clone changed the original: %#v", orig)
	}
	call := ToolCallPart{Arguments: map[string]any{"m": map[string]any{"x": 1}}}
	c2 := ClonePart(call)
	c2.Arguments["m"].(map[string]any)["x"] = 2
	if call.Arguments["m"].(map[string]any)["x"] != 1 {
		t.Fatal("tool call arguments were shared")
	}
	if CloneParts[Part](nil) != nil {
		t.Fatal("CloneParts(nil) is not nil")
	}
}
