package types

import (
	"reflect"
	"testing"
)

func TestCompatConstructorsBuildParts(t *testing.T) {
	if got := NewUserMessage("hi"); !reflect.DeepEqual(got, UserMsg(Text("hi"))) {
		t.Errorf("NewUserMessage = %#v", got)
	}
	if got := NewSystemMessage("s"); !reflect.DeepEqual(got, SystemMsg(Text("s"))) {
		t.Errorf("NewSystemMessage = %#v", got)
	}
	if got := NewAssistantMessage("a"); !reflect.DeepEqual(got, AssistantMsg(Text("a"))) {
		t.Errorf("NewAssistantMessage = %#v", got)
	}
	r := ToolOK("c", Text("ok"))
	if got := NewToolResultMessage(r); !reflect.DeepEqual(got, ToolResults(r)) {
		t.Errorf("NewToolResultMessage = %#v", got)
	}
	if got := NewUserToolResultMessage(r); !reflect.DeepEqual(got, UserToolResults(r)) {
		t.Errorf("NewUserToolResultMessage = %#v", got)
	}
	msg := NewUserMessageWithFiles("look", FileContent{URI: "file:///a.png", MediaType: MediaPNG, Data: []byte{1}})
	img, ok := msg.Parts[1].(ImagePart)
	if len(msg.Parts) != 2 || !ok || img.Source.URI != "file:///a.png" || len(img.Source.Inline) != 1 || img.Source.Digest == "" {
		t.Errorf("NewUserMessageWithFiles = %#v", msg)
	}
	if !IsMetadataContent(ConfigContent{Model: "m"}) || IsMetadataContent("text") || RouteContentFrom(RouteDelta{Model: "m"}).Model != "m" {
		t.Error("metadata shims disagree with the part API")
	}
}

func TestToolResultBlockPart(t *testing.T) {
	tests := []struct {
		b    ToolResultBlock
		want PartKind
	}{
		{ToolResultBlock{Kind: ToolResultBlockText, Text: "t"}, KindText},
		{ToolResultBlock{Kind: ToolResultBlockJSON, JSON: []byte(`{}`)}, KindJSON},
		{ToolResultBlock{Kind: ToolResultBlockImage, MediaType: MediaPNG, Data: []byte{1}}, KindImage},
		{ToolResultBlock{Kind: ToolResultBlockFile, MediaType: MediaPDF, URI: "u"}, KindDocument},
		{ToolResultBlock{Kind: ToolResultBlockFile, MediaType: MediaWAV, URI: "u"}, KindAudio},
		{ToolResultBlock{Kind: ToolResultBlockFile, MediaType: MediaMP4, URI: "u"}, KindFile},
		{ToolResultBlock{Kind: ToolResultBlockFile, MediaType: "application/zip", URI: "u"}, KindFile},
	}
	for _, tt := range tests {
		if got := tt.b.Part().Kind(); got != tt.want {
			t.Errorf("%+v.Part() = %s, want %s", tt.b, got, tt.want)
		}
	}
}
