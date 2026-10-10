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
