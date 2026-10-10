// Package main demonstrates media input with a conversion policy. It
// registers a file:// resolver that reads files from disk, attaches the file
// to a user message, and lets the agent resolve the URI. An image the model
// reads natively is sent as it is; a PDF or text document is extracted to
// text, because the policy permits extract for documents; anything else the
// model cannot take is rejected.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/convert"
	"github.com/urmzd/saige/agent/provider/ollama"
	"github.com/urmzd/saige/agent/types"
)

func main() {
	client := ollama.NewClient("http://localhost:11434", "llava", "")
	adapter := ollama.NewAdapter(client)

	// Show what the model takes natively, as its catalog offering declares.
	if offering, ok := convert.Target(adapter); ok {
		fmt.Println("Native input media:")
		for _, mt := range offering.Modalities.MediaTypes() {
			fmt.Printf("  - %s\n", mt)
		}
	}

	// file:// resolver that reads from the local filesystem.
	fileResolver := types.ResolverFunc(func(ctx context.Context, uri string) (types.ResolvedFile, error) {
		path := strings.TrimPrefix(uri, "file://")
		data, err := os.ReadFile(path)
		if err != nil {
			return types.ResolvedFile{}, fmt.Errorf("read file %s: %w", path, err)
		}
		// Detect media type from content.
		mediaType := types.MediaType(http.DetectContentType(data))
		return types.ResolvedFile{Data: data, MediaType: mediaType}, nil
	})

	// Build agent with the file resolver.
	agent := agentsdk.NewAgent(agentsdk.AgentConfig{
		Name:         "multimodal-agent",
		SystemPrompt: "You are a helpful assistant that can analyze images and files.",
		Provider:     adapter,
		Resolvers: map[string]types.Resolver{
			"file": fileResolver,
		},
		// Documents the model cannot read are extracted to text.
		Conversion: types.ConversionPolicy{
			Dial:       types.ModalityDial{Per: map[types.Modality][]types.ModalityAction{types.ModalityDocument: {types.ActExtract}}},
			Converters: []types.Converter{convert.Documents()},
		},
	})

	// Build a message with text and a file attachment.
	imagePath := "example.png"
	if len(os.Args) > 1 {
		imagePath = os.Args[1]
	}

	// The media type is inferred when the file is resolved.
	src := types.URL("file://" + imagePath)
	src.Filename = imagePath
	msg := types.UserMsg(types.Text("Describe what you see in this image."), types.Media(src))

	// Invoke the agent.
	stream := agent.Invoke(context.Background(), []types.Message{msg})

	for delta := range stream.Deltas() {
		switch d := delta.(type) {
		case types.PartDelta:
			fmt.Print(d.Text)
		case types.ErrorDelta:
			log.Fatal(d.Error)
		case types.DoneDelta:
			fmt.Println()
		}
	}

	if err := stream.Wait(); err != nil {
		log.Fatal(err)
	}
}
