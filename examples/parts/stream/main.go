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
