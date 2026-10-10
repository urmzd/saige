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
