package skills

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"testing/fstest"
)

func TestAllowList(t *testing.T) {
	trusted := SkillMeta{Name: "a", Trusted: true}
	untrusted := SkillMeta{Name: "u"}
	tests := []struct {
		name     string
		list     AllowList
		meta     SkillMeta
		want     bool
		wantJSON string
	}{
		{"zero value allows none", AllowList{}, trusted, false, `[]`},
		{"all allows trusted", AllowAll(), trusted, true, `"*"`},
		{"all excludes untrusted", AllowAll(), untrusted, false, `"*"`},
		{"named admits untrusted", AllowNames("u"), untrusted, true, `["u"]`},
		{"named excludes others", AllowNames("b"), trusted, false, `["b"]`},
		{"parse star", ParseAllowList(" * "), trusted, true, `"*"`},
		{"parse empty", ParseAllowList(""), trusted, false, `[]`},
		{"parse names", ParseAllowList("b, a"), trusted, true, `["a","b"]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.list.Permits(tt.meta); got != tt.want {
				t.Errorf("Permits = %v, want %v", got, tt.want)
			}
			data, err := json.Marshal(tt.list)
			if err != nil || string(data) != tt.wantJSON {
				t.Fatalf("Marshal = %s, %v; want %s", data, err, tt.wantJSON)
			}
			var back AllowList
			if err := json.Unmarshal(data, &back); err != nil {
				t.Fatal(err)
			}
			if back.Permits(tt.meta) != tt.want {
				t.Error("round trip changed the decision")
			}
		})
	}

	t.Run("json input", func(t *testing.T) {
		for in, wantErr := range map[string]bool{`null`: false, `"*"`: false, `[]`: false, `["x"]`: false, `"all"`: true, `3`: true} {
			var l AllowList
			if err := json.Unmarshal([]byte(in), &l); (err != nil) != wantErr {
				t.Errorf("Unmarshal(%s) err = %v, wantErr %v", in, err, wantErr)
			}
		}
	})
}

func TestAllowListIntersect(t *testing.T) {
	trustedA := SkillMeta{Name: "a", Trusted: true}
	untrustedU := SkillMeta{Name: "u"}
	tests := []struct {
		name string
		l, r AllowList
		meta SkillMeta
		want bool
	}{
		{"all and all", AllowAll(), AllowAll(), trustedA, true},
		{"all and names keeps trusted name", AllowAll(), AllowNames("a"), trustedA, true},
		{"all and names drops untrusted name", AllowAll(), AllowNames("u"), untrustedU, false},
		{"names and names", AllowNames("a", "u"), AllowNames("u"), untrustedU, true},
		{"none wins", AllowNone(), AllowAll(), trustedA, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.l.Intersect(tt.r).Permits(tt.meta); got != tt.want {
				t.Errorf("l∩r Permits = %v, want %v", got, tt.want)
			}
			if got := tt.r.Intersect(tt.l).Permits(tt.meta); got != tt.want {
				t.Errorf("r∩l Permits = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAllowListPolicy(t *testing.T) {
	all := []SkillMeta{{Name: "a", Trusted: true}, {Name: "b", Trusted: true}, {Name: "u"}}
	policy := AllowListPolicy{
		Default: AllowNone(),
		Owners: map[string]AllowList{
			"parent":     AllowNames("a", "u"),
			"child":      AllowAll(),
			"grandchild": AllowNames("a", "b", "u"),
			"loop-a":     AllowAll(),
			"loop-b":     AllowAll(),
		},
		Parents: map[string]string{"child": "parent", "grandchild": "child", "loop-a": "loop-b", "loop-b": "loop-a"},
	}
	tests := []struct {
		owner   string
		want    []string
		wantErr bool
	}{
		{owner: "stranger", want: nil},
		{owner: "parent", want: []string{"a", "u"}},
		{owner: "child", want: []string{"a"}},      // AllowAll admits trusted only, and the parent caps it
		{owner: "grandchild", want: []string{"a"}}, // capped by both ancestors
		{owner: "loop-a", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.owner, func(t *testing.T) {
			got, err := policy.Reachable(context.Background(), tt.owner, nil, all)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			var names []string
			for _, m := range got {
				names = append(names, m.Name)
			}
			if !slices.Equal(names, tt.want) {
				t.Errorf("Reachable = %v, want %v", names, tt.want)
			}
		})
	}
}

type failingSource struct{}

func (failingSource) Name() string { return "failing" }
func (failingSource) List(context.Context) ([]Skill, error) {
	return nil, context.DeadlineExceeded
}

func TestCatalog(t *testing.T) {
	first := NewFSSource("first", fstest.MapFS{
		"shared/SKILL.md": {Data: []byte(skillMD("shared", "From the first source."))},
		"only-a/SKILL.md": {Data: []byte(skillMD("only-a", "Parse PDF documents."))},
	}, ".")
	second := NewFSSource("second", fstest.MapFS{
		"shared/SKILL.md": {Data: []byte(skillMD("shared", "From the second source."))},
		"only-b/SKILL.md": {Data: []byte(skillMD("only-b", "Query spreadsheets."))},
		"bad/SKILL.md":    {Data: []byte("---\nname: bad\n---\n")},
	}, ".", Untrusted())

	cat, err := NewCatalog(context.Background(), []SkillSource{first, failingSource{}, second})
	if err != nil {
		t.Fatal(err)
	}
	metas, _ := cat.List(context.Background(), "")
	var names []string
	for _, m := range metas {
		names = append(names, m.Name)
	}
	if !slices.Equal(names, []string{"only-a", "only-b", "shared"}) {
		t.Errorf("List = %v", names)
	}
	shared, err := cat.Load(context.Background(), "", "shared")
	if err != nil || shared.Source != "first" || !shared.Trusted {
		t.Errorf("first definition should win: %+v, %v", shared.SkillMeta, err)
	}
	if s := cat.Shadowed(); len(s) != 1 || s[0].Source != "second" {
		t.Errorf("Shadowed = %+v", s)
	}
	if p := cat.Problems(); len(p) != 2 {
		t.Errorf("Problems = %v, want the failing source and the bad package", p)
	}
	if b, _ := cat.Load(context.Background(), "", "only-b"); b.Trusted {
		t.Error("a skill from an untrusted source must be untrusted")
	}
	hits, err := cat.Search(context.Background(), "", "pdf documents", 3)
	if err != nil || len(hits) != 1 || hits[0].Name != "only-a" {
		t.Errorf("Search = %+v, %v", hits, err)
	}
	if _, err := cat.Load(context.Background(), "", "missing"); err == nil {
		t.Error("Load of a missing skill must fail")
	}

	t.Run("canceled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := NewCatalog(ctx, []SkillSource{first}); err == nil {
			t.Fatal("want ctx error")
		}
	})
	t.Run("loaded copies are independent", func(t *testing.T) {
		a, _ := cat.Load(context.Background(), "", "shared")
		a.Resources = append(a.Resources, SkillResource{Path: "x"})
		b, _ := cat.Load(context.Background(), "", "shared")
		if len(b.Resources) != 0 {
			t.Error("a caller changed the catalog's snapshot")
		}
	})
}

func TestCatalogUntrustedNeverShadowsTrusted(t *testing.T) {
	src := func(name, desc string, opts ...SourceOption) SkillSource {
		return NewFSSource(name, fstest.MapFS{
			"deploy/SKILL.md": {Data: []byte(skillMD("deploy", desc))},
		}, ".", opts...)
	}
	tests := []struct {
		name         string
		sources      []SkillSource
		wantSource   string
		wantShadowed []string
	}{
		{"trusted first", []SkillSource{src("home", "Trusted."), src("project", "Swapped.", Untrusted())}, "home", []string{"project"}},
		{"untrusted first", []SkillSource{src("project", "Swapped.", Untrusted()), src("home", "Trusted.")}, "home", []string{"project"}},
		{"both trusted", []SkillSource{src("a", "First."), src("b", "Second.")}, "a", []string{"b"}},
		{"both untrusted", []SkillSource{src("a", "First.", Untrusted()), src("b", "Second.", Untrusted())}, "a", []string{"b"}},
		{"trusted after two untrusted", []SkillSource{src("a", "x", Untrusted()), src("b", "y", Untrusted()), src("home", "Trusted.")}, "home", []string{"b", "a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cat, err := NewCatalog(context.Background(), tt.sources)
			if err != nil {
				t.Fatal(err)
			}
			got, err := cat.Load(context.Background(), "", "deploy")
			if err != nil || got.Source != tt.wantSource {
				t.Fatalf("deploy from %q (%v), want %q", got.Source, err, tt.wantSource)
			}
			var shadowed []string
			for _, m := range cat.Shadowed() {
				shadowed = append(shadowed, m.Source)
			}
			if !slices.Equal(shadowed, tt.wantShadowed) {
				t.Errorf("Shadowed = %v, want %v", shadowed, tt.wantShadowed)
			}
		})
	}
}
