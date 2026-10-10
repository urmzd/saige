package tui

import (
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

// TestFilterBadgeCountsVisibleEntries checks that the badge counts only
// entries the template draws: hidden usage and routing notices are in
// neither the shown count nor the total.
func TestFilterBadgeCountsVisibleEntries(t *testing.T) {
	text := func(s string) *strings.Builder {
		var b strings.Builder
		b.WriteString(s)
		return &b
	}
	tmpl := TemplateDefault
	tmpl.ShowUsage, tmpl.ShowRouting = false, false
	m := runnerModel{template: tmpl, filter: parseFilter("chart")}
	m.act.entries = []activityEntry{
		{kind: activityUser, text: "draw a chart"},
		{kind: activityText, content: text("here is the chart"), final: true},
		{kind: activityText, content: text("")}, // nothing drawn
		{kind: activityUsage, usage: &types.UsageDelta{PromptTokens: 1}},
		{kind: activityNotice, text: "routed for the chart"},
	}
	if got := plain(m.headerView()); !strings.Contains(got, "2/2") {
		t.Fatalf("header = %q, want 2/2", got)
	}
	tmpl.ShowRouting = true
	m.template = tmpl
	if got := plain(m.headerView()); !strings.Contains(got, "3/3") {
		t.Fatalf("header = %q, want 3/3 with routing shown", got)
	}
}
