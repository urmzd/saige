// Building messages from typed parts, offline. Each modality has its own
// part, and every media part holds a Source: inline bytes, an https URL, a
// workspace artifact reference or a vendor upload. The program prints each
// message in the shared part codec, the form stores, the wire and the
// durable journal use, and checks which sources an untrusted client may
// send.
package main

import (
	"fmt"
	"log"

	"github.com/urmzd/saige/agent/types"
)

func main() {
	png := []byte("\x89PNG\r\n\x1a\n") // stand-in bytes; read a real file in practice

	// A user turn: text, then media. Bytes, URL, Artifact and VendorFileID
	// build a Source; Image, Document, Audio, Video and File wrap it, and
	// Media picks the part from the media type.
	user := types.UserMsg(
		types.Text("Compare the chart with the report."),
		types.Image(types.Bytes(types.MediaPNG, png), types.ImageMeta{Detail: "high"}),
		types.Document(types.URL("https://example.com/report.pdf", types.MediaPDF),
			types.DocumentMeta{Title: "Q3 report"}),
		types.Media(types.URL("https://example.com/call.mp3", types.MediaMP3)), // an AudioPart
	)

	// One Source can hold several locators at once. Each adapter picks the
	// one it can use, so a request that fails over still reaches the media.
	both := types.Bytes(types.MediaPNG, png).With(types.URL("https://example.com/chart.png"))
	fmt.Printf("locators: inline=%t uri=%q sha256=%.12s\n", len(both.Inline) > 0, both.URI, both.Digest)

	// A tool result holds output parts: text, JSON, images, documents,
	// audio and files.
	row, err := types.JSON(map[string]any{"rows": 3})
	if err != nil {
		log.Fatal(err)
	}
	result := types.ToolResults(types.ToolOK("call_1", types.Text("3 rows"), row, types.Image(both)))

	for _, m := range []types.Message{user, result} {
		show(m)
	}

	// Parts from an untrusted client (a request body, an editor) may carry
	// inline bytes, an https URL or a saige-artifact:// reference, and
	// nothing else.
	untrusted := []types.UserPart{
		types.Image(types.URL("https://example.com/a.png", types.MediaPNG)),
		types.Image(types.URL("http://example.com/a.png", types.MediaPNG)),
		types.Document(types.VendorFileID("anthropic", "", "file_123", types.MediaPDF)),
	}
	for i, p := range untrusted {
		if err := types.CheckClientParts([]types.UserPart{p}); err != nil {
			fmt.Printf("client part %d refused: %v\n", i, err)
		} else {
			fmt.Printf("client part %d accepted\n", i)
		}
	}
}

// show writes each part of m in the shared part codec. Inline bytes are
// never persisted, so the codec keeps the digest and size instead.
func show(m types.Message) {
	fmt.Println(m.Role())
	for _, p := range types.PartsOf(m) {
		b, err := types.MarshalPart(p)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("  %s\n", b)
	}
}
