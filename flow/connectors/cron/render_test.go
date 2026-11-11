package cron_test

import (
	"testing"

	"github.com/urmzd/saige/flow/connectors/internal/rendertest"
	"github.com/urmzd/saige/flow/event"
)

// A tick renders through the event catalog as its request text, schedule
// context included.
func TestRenderMatchesRequestText(t *testing.T) {
	s := nightly()
	s.Context = map[string]string{"team": "ops", "cron_schedule": "spoofed", "_host.pinned": "x"}
	clk := &clock{at: at(t, "2026-07-20T04:00:00Z")}
	c, tags := start(t, memory(t, s), clk)
	tick(t, c)
	clk.set(at(t, "2026-07-21T04:00:00Z"))
	tick(t, c)
	for _, tag := range drain(t, tags, 1) {
		if typ := rendertest.Check(t, tag); typ != event.TypeCronTick {
			t.Errorf("type = %s, want %s", typ, event.TypeCronTick)
		}
	}
}
