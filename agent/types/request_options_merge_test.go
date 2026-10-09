package types_test

import (
	"reflect"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func f64(v float64) *float64 { return &v }
func i64(v int64) *int64     { return &v }
func str(v string) *string   { return &v }

func TestRequestOptionsMergeFieldLevel(t *testing.T) {
	base := types.RequestOptions{Temperature: f64(0.3), Seed: i64(7), ReasoningBudget: i64(2048), StopSequences: []string{"x"}}
	over := types.RequestOptions{Temperature: f64(0.9), ReasoningEffort: str("high")}
	got := base.Merge(over)
	if *got.Temperature != 0.9 || *got.Seed != 7 || got.StopSequences[0] != "x" {
		t.Fatalf("field merge: %+v", got)
	}
	if got.ReasoningBudget != nil || got.ReasoningEffort == nil || *got.ReasoningEffort != "high" {
		t.Fatalf("reasoning must replace whole: %+v", got)
	}
	*got.Seed = 99
	if *base.Seed != 7 {
		t.Fatal("merge aliases the base")
	}
	if names := got.OptionNames(); !reflect.DeepEqual(names, []string{"temperature", "seed", "stop", "reasoning"}) {
		t.Fatalf("names: %v", names)
	}
}

func TestRequestOptionsWithout(t *testing.T) {
	o := types.RequestOptions{Temperature: f64(1), ReasoningEnabled: new(bool), ToolChoice: &types.ToolChoice{Mode: types.ToolChoiceNone}}
	got := o.Without("reasoning", "temperature", "unknown")
	if got.Temperature != nil || got.HasReasoning() || got.ToolChoice == nil {
		t.Fatalf("without: %+v", got)
	}
	if o.Temperature == nil {
		t.Fatal("without mutated the receiver")
	}
	if len(types.RequestOptions{}.OptionNames()) != 0 {
		t.Fatal("empty options report names")
	}
}
