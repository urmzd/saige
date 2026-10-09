package eval

import (
	"testing"

	"github.com/urmzd/saige/agent/types"
	topeval "github.com/urmzd/saige/eval"
)

func TestCollectRecordsServingRoutes(t *testing.T) {
	ch := make(chan types.Delta, 8)
	ch <- types.RouteDelta{Profile: "p/a", Preset: "p", ConfigHash: "ha", CatalogRevision: "r"}
	ch <- types.RouteDelta{Profile: "p/b", Preset: "p", ConfigHash: "hb", CatalogRevision: "r", Reason: "failover"}
	ch <- types.TextContentDelta{Content: "x"}
	ch <- types.UsageDelta{PromptTokens: 1}
	ch <- types.RouteDelta{Profile: "q/x", Preset: "q", ConfigHash: "hx", CatalogRevision: "r"}
	ch <- types.UsageDelta{PromptTokens: 1}
	close(ch)
	run := CollectAgentRun(ch)
	if len(run.Routes) != 2 || run.Routes[0].Profile != "p/b" || run.Routes[1].ConfigHash != "hx" {
		t.Fatalf("routes %+v", run.Routes)
	}
	var p topeval.Provenance
	run.AddProvenance(&p)
	if p.Catalog == nil || p.Catalog.Configs["p/b"] != "hb" || len(p.Catalog.Presets) != 2 {
		t.Fatalf("provenance %+v", p.Catalog)
	}
}
