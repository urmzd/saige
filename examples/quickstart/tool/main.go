// A typed tool: agent.Func derives the JSON schema from WeatherIn and
// decodes the model's arguments into it.
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/provider/preset"
	"github.com/urmzd/saige/agent/types"
)

type WeatherIn struct {
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
		func(rc agent.RunContext[agent.NoDeps], in WeatherIn) (string, error) {
			return "18C and sunny in " + in.City, nil
		})

	a, err := agent.New(agent.Config{Tools: types.NewToolRegistry(weather)}, agent.WithPreset(bundle))
	if err != nil {
		log.Fatal(err)
	}
	text, err := agent.CollectText(a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("What's the weather in Lisbon?"))}))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(text)
}
