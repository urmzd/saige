# Message parts

A message is an ordered list of typed parts: text, an image, a document, a tool call, a citation, a refusal, and so on. Model output streams as parts too, and every store, the wire format and the durable journal encode a part the same way. This guide follows a part from the message you build to the bytes on disk (D-44).

- [Part kinds](#part-kinds)
- [Sources and locators](#sources-and-locators)
- [Building messages](#building-messages)
- [Sending each modality](#sending-each-modality)
- [Streaming: part deltas and the aggregator](#streaming-part-deltas-and-the-aggregator)
- [Wire format versions](#wire-format-versions)
- [Persistence and externalized media](#persistence-and-externalized-media)
- [When the model cannot take a part](#when-the-model-cannot-take-a-part)

## Part kinds

Each role accepts its own set of kinds, so a misplaced part is a compile error: `types.UserMsg` takes `UserPart`s, `types.AssistantMsg` takes `AssistantPart`s, and `types.SystemMsg` takes `SystemPart`s.

| Kind | Type | System | User | Assistant | Tool output |
| --- | --- | :-: | :-: | :-: | :-: |
| `text` | `TextPart` | yes | yes | yes | yes |
| `json` | `JSONPart` | | | | yes |
| `image` | `ImagePart` | | yes | | yes |
| `audio` | `AudioPart` | | yes | | yes |
| `video` | `VideoPart` | | yes | | |
| `document` | `DocumentPart` | | yes | | yes |
| `file` | `FilePart` | | yes | | yes |
| `tool_result` | `ToolResultPart` | yes | yes | | |
| `thinking` | `ThinkingPart` | | | yes | |
| `tool_call` | `ToolCallPart` | | | yes | |
| `server_tool_call`, `server_tool_result` | `ServerToolCallPart`, `ServerToolResultPart` | | | yes | |
| `citation` | `CitationPart` | | | yes | |
| `image_out`, `audio_out`, `video_out` | `ImageOutPart`, `AudioOutPart`, `VideoOutPart` | | | yes | |
| `refusal` | `RefusalPart` | | | yes | |

Metadata parts (`ConfigPart`, `RoutePart`, `SteerPart`, `TruncationPart`, `HandoffPart`, `FeedbackPart`, `ApprovalPart`, `GuardrailPart`, `CompactionPart`) record what the run decided. The tree stores them and the loop strips them before every provider call; `types.IsMetadata` tells them apart.

Read a message with `types.PartsOf(m)`, `types.TextOf(m)`, or `types.Each[types.ImagePart](m)` for the parts of one kind. `types.PartModality(p)` classifies a part, and `types.SourceOf(p)` returns the media source of any media part.

Kinds are part of the stored and wire contract. They are never renamed (D-35).

## Sources and locators

Every media part holds a `types.Source`: one logical piece of media and every way it can be reached.

| Constructor | Locator | Use it for |
| --- | --- | --- |
| `types.Bytes(mt, b)` | `Inline`, with `Digest` (SHA-256) and `Size` | Media you hold in memory |
| `types.URL(u, mt)` | `URI` | `https` media the vendor fetches, `gs://` on Vertex AI, or a scheme a `Resolver` reads |
| `types.Artifact(ref, mt)` | `Ref` (`saige-artifact://<sha256>`) | Media stored in the agent's workspace or a `saige serve` session |
| `types.VendorFileID(provider, endpoint, id, mt)` | `Files` | An upload to a vendor file store, scoped to the endpoint that owns it |

A source may carry several locators at once: `types.Bytes(mt, b).With(types.URL(u))` holds both. Each adapter picks the locator it can use, so a request that fails over from one vendor to another still reaches the media. One vendor's file ID is never sent to another vendor, or to another endpoint of the same vendor.

A `Resolver` fetches a URI scheme to bytes before the request is planned (`agent.WithResolvers(map[string]types.Resolver{"file": r})`). A source the agent cannot reach is marked `Unresolved` and rejected, never sent with its media missing.

**Untrusted clients.** Media from a client you do not control (a request body, an editor) may carry inline bytes, an `https` URL or a `saige-artifact://` reference, and nothing else: no vendor file IDs, no `http`, `gs`, `s3` or `file` URIs, no bare file names and no URLs on vendor file stores (D-47). Call `types.CheckClientSource` or `types.CheckClientParts` where such parts enter your host; `saige serve` and `saige acp` do.

## Building messages

`types.UserMsg(types.Text(...), ...)` builds a turn from parts. `types.Image`, `types.Audio`, `types.Video`, `types.Document` and `types.File` wrap a source with optional metadata (`ImageMeta.Detail`, `DocumentMeta.Title`, `VideoMeta.ClipStart`, and so on), and `types.Media(src)` picks the part from the media type. A tool returns `types.ToolOK(callID, parts...)` or `types.ToolErr(callID, msg)`, and `types.ToolResults` puts results in a message.

This program runs offline and prints each part in the shared part codec:

<!-- fsrc src="../examples/parts/build/main.go" fence="auto" -->
```go
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
```
<!-- /fsrc -->

A part encodes as its fields plus a `type` tag:

```json
{"type":"image","source":{"media_type":"image/png","size":8,"sha256":"4c4b6a3b..."},"image":{"detail":"high"}}
```

## Sending each modality

What a model takes is declared by its catalog offering: media types, size and count limits, and the locators its endpoint reads (see [the catalog](catalog.md)). Read it in code with `convert.Target(p)`, which returns the offering of the adapter behind any decorator stack. The built-in adapters send:

| Modality | Part | Anthropic | OpenAI Chat Completions | OpenAI Responses | Google | Ollama |
| --- | --- | --- | --- | --- | --- | --- |
| Image | `types.Image` | JPEG, PNG, GIF, WebP by file ID, https URL or bytes | JPEG, PNG, GIF, WebP by URL or bytes | by file ID, URL or bytes | PNG, JPEG, WebP, GIF, HEIC, HEIF by upload, URI or bytes | inline JPEG or PNG |
| Document | `types.Document` | PDF and `text/plain` by file ID, https URL (PDF) or bytes | PDF by bytes or file ID | PDF, text, code and office files by file ID, URL or bytes | PDF and text by upload, URI or bytes | no |
| Audio | `types.Audio` | no | WAV or MP3 bytes | no | yes | no |
| Video | `types.Video` | no | no | no | by upload, `https`, YouTube, or `gs://` on Vertex AI | no |
| File | `types.File` | as a code execution upload | no | no | no | no |
| Media in a tool result | `types.ToolOK(id, parts...)` | text, JSON, images, documents | text; images move to a user message after the tool results | text, images, files | images, PDFs, text on Gemini 3 | images |

Google sends inline bytes up to 20 MB; upload larger media and pass its URI. A part the adapter cannot send fails before the request with an error that names the part and matches `types.ErrModalityUnsupported`, or `types.ErrMediaUnavailable` when no locator can be reached.

### An image

Read the bytes and put the image after the question:

<!-- fsrc src="../examples/parts/image/main.go" fence="auto" -->
```go
// Sending an image: read the file, wrap its bytes in an ImagePart, and put
// it in a user message after the question. Runs on the "default" preset;
// every model it serves reads JPEG and PNG.
//
//	go run ./examples/parts/image photo.png
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/provider/preset"
	"github.com/urmzd/saige/agent/types"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: image FILE")
	}
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		log.Fatal(err)
	}
	mt := types.MediaType(http.DetectContentType(data)) // image/png, image/jpeg, ...

	ctx := context.Background()
	bundle, err := preset.Build(ctx, catalog.Default(), "default", nil, preset.Options{})
	if err != nil {
		log.Fatal(err)
	}
	defer bundle.Close(ctx)

	a, err := agent.New(agent.Config{SystemPrompt: "Answer in one sentence."}, agent.WithPreset(bundle))
	if err != nil {
		log.Fatal(err)
	}
	msg := types.UserMsg(types.Text("What is in this image?"), types.Image(types.Bytes(mt, data)))
	text, err := agent.CollectText(a.Invoke(ctx, []types.Message{msg}))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(text)
}
```
<!-- /fsrc -->

For an image the vendor fetches itself, send `types.Image(types.URL("https://...", types.MediaJPEG))` instead.

### A PDF, extracted when the model cannot read it

A document goes out natively when the serving model reads it. Permit `extract` for documents and register `convert.Documents()`, and a model that cannot read it gets the text instead. Pass `ollama` as the second argument to see the extract path on a local model that reads images but not PDFs:

<!-- fsrc src="../examples/parts/document/main.go" fence="auto" -->
```go
// Sending a PDF that the serving model may not read natively. The
// conversion policy permits extract for documents, so a model that reads
// PDFs gets the file and any other model gets its text. Without the policy
// such a request is rejected, never silently dropped. The optional second
// argument names the preset; "ollama" serves a model that reads images but
// not PDFs, so the text is extracted.
//
//	go run ./examples/parts/document report.pdf [ollama]
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/convert"
	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/provider/preset"
	"github.com/urmzd/saige/agent/types"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: document FILE.pdf [PRESET]")
	}
	name := types.PresetName("default")
	if len(os.Args) > 2 {
		name = types.PresetName(os.Args[2])
	}
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()
	bundle, err := preset.Build(ctx, catalog.Default(), name, nil, preset.Options{})
	if err != nil {
		log.Fatal(err)
	}
	defer bundle.Close(ctx)

	a, err := agent.New(agent.Config{SystemPrompt: "Answer in one sentence."},
		agent.WithPreset(bundle),
		agent.WithConversion(types.ConversionPolicy{
			Dial: types.ModalityDial{Per: map[types.Modality][]types.ModalityAction{
				types.ModalityDocument: {types.ActExtract},
			}},
			Converters: []types.Converter{convert.Documents()},
		}))
	if err != nil {
		log.Fatal(err)
	}

	src := types.Bytes(types.MediaPDF, data)
	src.Filename = filepath.Base(os.Args[1])
	msg := types.UserMsg(types.Text("Summarize this document."), types.Document(src))

	stream := a.Invoke(ctx, []types.Message{msg})
	tr, err := agent.Collect(stream, func(d types.Delta) {
		// Each attempt reports how it served the media: native, or the
		// action and converter that changed it.
		if c, ok := d.(types.ConversionDelta); ok {
			for _, dec := range c.Report.Decisions {
				fmt.Printf("[%s %s: %s %s]\n", dec.Kind, dec.MediaType, dec.Action, dec.Via)
			}
		}
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(tr.Text)
}
```
<!-- /fsrc -->

### Audio, video and files

Audio and video follow the image pattern: `types.Audio(types.Bytes(types.MediaWAV, b))`, or `types.Video(types.URL("gs://bucket/clip.mp4", types.MediaMP4), types.VideoMeta{ClipStart: 10 * time.Second})` on Vertex AI. Route them to a model that takes them (Google for video), or permit `transcribe` or `describe` with a converter that calls such a model. An opaque `types.File` is sent only where an endpoint accepts it, such as an Anthropic code execution upload.

## Streaming: part deltas and the aggregator

A model turn streams as part deltas:

| Delta | Carries |
| --- | --- |
| `types.PartStart` | `Index`, `Kind`, the `MediaType` of media, and the `ID` and `Name` of a tool call, so a UI can show the call before its arguments arrive |
| `types.PartDelta` | `Index` and exactly one payload: `Text`, `Thinking`, `Signature`, `Args` (a JSON fragment), `Refusal`, `Data` (media bytes) or `Transcript` |
| `types.PartEnd` | `Index` and, when the producer has it, the complete `Part`. A tool call's end always carries the complete `ToolCallPart` |

`Index` is the part's position in the final message, unique within one provider attempt. Deltas for different parts may interleave, so key any state by index. Server tool calls and their results are separate parts (`types.PairServerTools` joins them), and model citations arrive as `CitationPart`s anchored to the text they support.

`types.NewPartAssembler()` builds the turn from its deltas: `Push` each delta, then read `Parts()`, sorted by index. A delta that breaks the protocol (a delta or end for an index that is not open, a start for a closed one) is counted in `Violations()` and ignored, never guessed at. A stream that stops with parts still open reports `Truncated()`, and `Flush` returns what was complete. The agent's `DefaultAggregator` runs on it, and `agent.Collect` and `agent.CollectText` use that.

Around the model's parts, the agent streams its own deltas: `ToolExecStartDelta` and `ToolExecEndDelta` (with the result's `Parts` and `Citations`), `RouteDelta`, `ConversionDelta`, `UsageDelta`, `ErrorDelta` and `DoneDelta`.

<!-- fsrc src="../examples/parts/stream/main.go" fence="auto" -->
```go
// Reading a run as part deltas. Each model turn streams PartStart, PartDelta
// and PartEnd for every part, keyed by Index, the part's position in the
// final message. The program prints text as it arrives, rebuilds each turn
// with a PartAssembler, and writes every delta to stderr as a wire version 2
// envelope, the form saige serve streams.
//
//	go run ./examples/parts/stream 2>events.ndjson
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/provider/preset"
	"github.com/urmzd/saige/agent/types"
)

type CityIn struct {
	City string `json:"city" description:"City name"`
}

func main() {
	ctx := context.Background()
	bundle, err := preset.Build(ctx, catalog.Default(), "default", nil, preset.Options{})
	if err != nil {
		log.Fatal(err)
	}
	defer bundle.Close(ctx)

	weather := agent.Func("weather", "Current weather for a city",
		func(rc agent.RunContext[agent.NoDeps], in CityIn) (string, error) {
			return "18C and sunny in " + in.City, nil
		})
	a, err := agent.New(agent.Config{Tools: types.NewToolRegistry(weather)}, agent.WithPreset(bundle))
	if err != nil {
		log.Fatal(err)
	}

	enc, err := types.NewEncoder(types.EncodeOptions{}) // version 2; Version: 1 for older readers
	if err != nil {
		log.Fatal(err)
	}

	asm := types.NewPartAssembler()
	stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("Weather in Lisbon?"))})
	for d := range stream.Deltas() {
		envs, err := enc.Encode(d)
		if err != nil {
			log.Fatal(err)
		}
		for _, env := range envs {
			b, _ := json.Marshal(env)
			fmt.Fprintln(os.Stderr, string(b))
		}

		asm.Push(d)
		switch v := d.(type) {
		case types.PartStart:
			if v.Kind == types.KindToolCall {
				fmt.Printf("\n[calling %s]\n", v.Name)
			}
		case types.PartDelta:
			fmt.Print(v.Text) // empty for thinking, argument and media fragments
		case types.ToolExecEndDelta:
			fmt.Printf("[%s returned %q]\n", v.ToolCallID, v.Result)
			asm.Reset() // the next model turn numbers its parts from 0 again
		}
	}
	if err := stream.Wait(); err != nil {
		log.Fatal(err)
	}
	fmt.Println()
	for _, p := range asm.Parts() {
		fmt.Printf("final part: %s\n", p.Kind())
	}
}
```
<!-- /fsrc -->

A provider you write emits the same deltas: `types.PartDeltas(index, part)` and `types.MessageDeltas(msg)` turn finished parts into a well-formed sequence.

## Wire format versions

The wire format carries deltas between processes as envelopes, `{"v":2,"kind":"part.delta","data":{...}}`. It is a stable contract (D-35).

| Version | Model output kinds | Notes |
| --- | --- | --- |
| 2 (default) | `part.start`, `part.delta`, `part.end` | Media bytes in one field are limited to 256 KiB (`types.DefaultMaxInlineBytes`); a larger part fails with `types.ErrWireInlineTooLarge` |
| 1 | `text.*`, `reasoning.*`, `tool.call.*` and the server tool kinds | Output with no version 1 form, such as a refusal or a generated image, becomes an `error` envelope with code `wire_unrepresentable` |

Write with `types.NewEncoder(types.EncodeOptions{})`, or `{Version: 1}` for a client that reads only version 1. Read with `types.NewDecoder()`, which accepts both versions and upgrades version 1 envelopes to part deltas. Use one encoder and one decoder per stream: both keep state.

`saige serve` streams version 2 and names it in the `Saige-Wire-Version` header. A version 1 client adds `?wire=1` or `Accept: application/vnd.saige.events+json;v=1`. Media over the inline limit in a run's output is stored in the session and sent as its `saige-artifact://` ref, downloadable from `GET /v1/sessions/{sid}/artifacts/{sha256}`. A turn's input is `{"parts": [...]}` in the part codec; upload media first with `POST /v1/sessions/{sid}/artifacts` and send the ref it returns. See the [CLI reference](../cmd/saige/README.md).

## Persistence and externalized media

Trees, `pgstore` rows, `filewal` logs and durable journals store messages in the same part codec. A node message is `{"v":2,"parts":[...]}` (`tree.MessageFormatVersion`); `tree.MarshalMessage` and `tree.UnmarshalMessage` read and write it, and the reader also accepts the format earlier releases wrote.

**Bytes are never persisted.** A stored source keeps its URI, ref, vendor files, digest and size. So a conversation keeps its media across a reload, attach a workspace:

```go
a, err := agent.New(agent.Config{Provider: p, Store: store},
	agent.WithWorkspace(workspace.NewMemory())) // or workspace.NewDir(path)
```

Before a message is committed, the agent stores the bytes of its media in the workspace and records a `saige-artifact://<sha256>` ref on the source. When a stored conversation is loaded again, a source whose only locator is that ref gets its bytes back from the workspace (or a `Resolvers["saige-artifact"]` resolver) before the request is planned; a ref neither holds, or bytes that do not match the digest, mark the part unavailable. Without a workspace, a stored media part reads back with its digest and size only, and adapters reject it.

`saige tree migrate TARGET --write` rewrites node messages stored by earlier releases in a PostgreSQL database, a tree document or a file WAL. Rewriting is optional: readers migrate on read.

## When the model cannot take a part

A part the serving model cannot take natively is rejected by default, never dropped. Permit an action per modality (`convert`, `transcribe`, `describe`, `extract`, `omit`) and register converters, and each attempt converts the copy it sends while the conversation keeps the original parts. See [modality conversion](modality-conversion.md).
