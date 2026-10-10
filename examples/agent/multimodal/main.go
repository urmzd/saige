// Package main demonstrates file upload with content negotiation. It registers
// a file:// resolver that reads files from disk, attaches a FileContent block
// to a user message, and lets the agent's file pipeline resolve the URI and
// check the provider's ContentNegotiator for native media type support.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/provider/ollama"
	"github.com/urmzd/saige/agent/types"
)

func main() {
	client := ollama.NewClient("http://localhost:11434", "llava", "")
	adapter := ollama.NewAdapter(client)

	// Check what content types the provider supports natively.
	support := adapter.ContentSupport()
	fmt.Println("Provider native types:")
	for mt, ok := range support.NativeTypes {
		if ok {
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
