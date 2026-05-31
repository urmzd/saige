// Package main demonstrates the response-cache provider decorator. Wrapping any
// types.Provider with cache.New memoizes ChatStream responses keyed by a
// deterministic hash of (model, messages, tools, schema). The first call streams
// live and is recorded; an identical second call replays the recorded deltas
// without touching the upstream provider, and reports UsageDelta{CacheHit: true}.
//
// The cache keys on the exact request, so a hit requires identical messages.
// This example calls the decorated provider directly with the same request twice
// to show the miss → hit transition cleanly.
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/urmzd/saige/agent/cache/memcache"
	"github.com/urmzd/saige/agent/provider/cache"
	"github.com/urmzd/saige/agent/provider/ollama"
	"github.com/urmzd/saige/agent/types"
)

func main() {
	base := ollama.NewAdapter(ollama.NewClient("http://localhost:11434", "llama3.2", ""))

	cached := cache.New(base, cache.Config{
		Cache:        memcache.New[cache.CachedResponse](),
		KeyNamespace: "demo",
	})

	request := []types.Message{
		types.NewSystemMessage("You are concise."),
		types.NewUserMessage("Name three primary colors."),
	}

	call := func(label string) {
		ch, err := cached.ChatStream(context.Background(), request, nil)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%s: ", label)
		for d := range ch {
			switch v := d.(type) {
			case types.TextContentDelta:
				fmt.Print(v.Content)
			case types.UsageDelta:
				if v.CacheHit {
					fmt.Print("  [served from cache]")
				}
			case types.ErrorDelta:
				log.Fatal(v.Error)
			}
		}
		fmt.Println()
	}

	call("first  (miss)")
	call("second (hit)")
}
