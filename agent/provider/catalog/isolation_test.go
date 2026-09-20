package catalog

import (
	"sync"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestPrefixInferenceIsNotAnExactDeclaration(t *testing.T) {
	for _, model := range []string{"gpt-5.2-unlisted-variant", "gpt-5.2-2099-01-01"} {
		c, known := Lookup("openai", model)
		if known || c.Known || c.Family != "gpt-5.2" {
			t.Fatalf("inferred model %q: known=%v caps=%+v", model, known, c)
		}
	}
	c, known := Lookup("openai", "gpt-5.2")
	if !known || !c.Known {
		t.Fatal("exact declaration lost")
	}
	Register(Entry{Provider: "isolation-exact", Prefix: "model", Caps: c})
	Register(Entry{Provider: "isolation-exact", Prefix: "model-variant", Caps: c.Without(types.CapTemperature)})
	variant, known := Lookup("isolation-exact", "model-variant")
	if !known || variant.Supports(types.CapTemperature) {
		t.Fatal("exact variant did not win")
	}
	Register(Entry{Provider: "isolation-exact", Prefix: "org/model:tag", Caps: c.Without(types.CapTemperature)})
	tag, known := Lookup("isolation-exact", "org/model:tag")
	if !known || tag.Family != "org/model:tag" || tag.Supports(types.CapTemperature) {
		t.Fatal("normalized family shadowed exact tag/path declaration")
	}

}

func isolationCaps() types.ModelCapabilities {
	c := caps(types.CapTemperature, types.CapReasoningEffort)
	c.ReasoningEfforts = []string{"high"}
	c.SamplingRequiresNoReasoning = []types.Capability{types.CapTemperature}
	c.Notes = []string{"original"}
	c.ServerTools = []types.ServerToolKind{types.ServerToolWebSearch}
	c.Media = media(types.MediaPNG)
	return c
}

func corrupt(c types.ModelCapabilities) {
	c.Caps[types.CapTemperature] = false
	c.ReasoningEfforts[0] = "corrupted"
	c.SamplingRequiresNoReasoning[0] = types.CapTopP
	c.Notes[0] = "corrupted"
	c.ServerTools[0] = types.ServerToolRemoteMCP
	c.Media.NativeTypes[types.MediaPNG] = false
}

func assertOriginal(t *testing.T, c types.ModelCapabilities) {
	t.Helper()
	if !c.Supports(types.CapTemperature) || c.ReasoningEfforts[0] != "high" ||
		c.SamplingRequiresNoReasoning[0] != types.CapTemperature || c.Notes[0] != "original" ||
		c.ServerTools[0] != types.ServerToolWebSearch || !c.Media.Supports(types.MediaPNG) {
		t.Fatalf("revision changed through an alias: %+v", c)
	}
}

func TestCatalogOwnsRegistrationHistoryRollbackAndBaseline(t *testing.T) {
	const provider = "isolation-boundaries"
	input := isolationCaps()
	registered := Register(Entry{Provider: provider, Prefix: "model", Caps: input})
	corrupt(input)
	corrupt(registered.Value.Caps)
	assertOriginal(t, MustLookup(provider, "model"))
	corrupt(History(provider, "model")[0].Value.Caps)
	corrupt(MustLookup(provider, "model"))
	assertOriginal(t, History(provider, "model")[0].Value.Caps)
	Register(Entry{Provider: provider, Prefix: "model", Caps: isolationCaps()})
	rolled, err := Rollback(provider, "model")
	if err != nil {
		t.Fatal(err)
	}
	corrupt(rolled.Caps)
	assertOriginal(t, MustLookup(provider, "model"))
	if Revisions(provider, "model") != 2 {
		t.Fatal("reads or mutations changed revision count")
	}
	baselineInput := isolationCaps()
	RegisterBaseline(provider, baselineInput)
	corrupt(baselineInput)
	assertOriginal(t, MustLookup(provider, "unknown"))
}

func TestCatalogSnapshotsCanMutateWhileOthersRead(t *testing.T) {
	const provider = "isolation-concurrent"
	input := isolationCaps()
	registered := Register(Entry{Provider: provider, Prefix: "model", Caps: input})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; n < 100; n++ {
				switch i {
				case 0:
					corrupt(input)
				case 1:
					corrupt(registered.Value.Caps)
				case 2:
					corrupt(History(provider, "model")[0].Value.Caps)
				case 3:
					c := MustLookup(provider, "model")
					if !c.Supports(types.CapTemperature) {
						t.Errorf("changed at read %d", n)
					}
				}
			}
		}(i)
	}
	wg.Wait()
	assertOriginal(t, MustLookup(provider, "model"))
}
