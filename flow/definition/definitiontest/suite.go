// Package definitiontest is the conformance suite every definition.Source
// passes. Memory and Postgres run it; a new backend runs it too.
package definitiontest

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/urmzd/saige/flow/definition"
)

// Content returns a valid .agent.md file for name at version with prompt.
func Content(name, version, prompt string) string {
	return fmt.Sprintf("---\napiVersion: saige/v1\nname: %s\nversion: %s\nmodel: offline/echo\n---\n%s\n", name, version, prompt)
}

// Record returns a valid record for tenant.
func Record(tenant, name, version, prompt string) definition.Record {
	return definition.Record{Tenant: tenant, Content: Content(name, version, prompt), CreatedBy: "ada"}
}

// Run holds a Source to the contract. newSource returns an empty source.
func Run(t *testing.T, newSource func(t *testing.T) definition.Source) {
	ctx := context.Background()

	t.Run("PutIsInsertOnly", func(t *testing.T) {
		s := newSource(t)
		first, err := s.Put(ctx, Record("acme", "docs-bot", "1.0.0", "Answer."))
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		if first.Name != "docs-bot" || first.Version != "1.0.0" || first.Digest == "" || first.PolicyHash == "" {
			t.Fatalf("put filled %+v", first)
		}
		again, err := s.Put(ctx, Record("acme", "docs-bot", "1.0.0", "Answer."))
		if err != nil {
			t.Fatalf("identical put: %v", err)
		}
		if again.Digest != first.Digest || !again.CreatedAt.Equal(first.CreatedAt) {
			t.Errorf("identical put wrote again: %+v, then %+v", first, again)
		}
		if _, err := s.Put(ctx, Record("acme", "docs-bot", "1.0.0", "Something else.")); !errors.Is(err, definition.ErrExists) {
			t.Errorf("put of other content under 1.0.0 = %v, want ErrExists", err)
		}
		other := Record("acme", "docs-bot", "1.0.0", "Answer.")
		other.Policy = definition.Policy{TaggedOnly: true}
		if _, err := s.Put(ctx, other); !errors.Is(err, definition.ErrExists) {
			t.Errorf("put of another policy under 1.0.0 = %v, want ErrExists", err)
		}
		got, err := s.Get(ctx, "acme", "docs-bot", "1.0.0")
		if err != nil || got.Digest != first.Digest || got.Content != first.Content {
			t.Errorf("get after refused rewrite = %+v, %v", got, err)
		}
	})

	t.Run("PutRefusesInvalid", func(t *testing.T) {
		s := newSource(t)
		for name, rec := range map[string]definition.Record{
			"no tenant":       {Content: Content("a", "1.0.0", "x"), CreatedBy: "ada"},
			"no author":       {Tenant: "acme", Content: Content("a", "1.0.0", "x")},
			"bad content":     {Tenant: "acme", Content: "not a definition", CreatedBy: "ada"},
			"name mismatch":   {Tenant: "acme", Name: "b", Content: Content("a", "1.0.0", "x"), CreatedBy: "ada"},
			"version differs": {Tenant: "acme", Version: "2.0.0", Content: Content("a", "1.0.0", "x"), CreatedBy: "ada"},
			"empty prompt":    {Tenant: "acme", Content: Content("a", "1.0.0", ""), CreatedBy: "ada"},
		} {
			if _, err := s.Put(ctx, rec); !errors.Is(err, definition.ErrInvalid) {
				t.Errorf("%s: put = %v, want ErrInvalid", name, err)
			}
		}
	})

	t.Run("TenantsAreIsolated", func(t *testing.T) {
		s := newSource(t)
		if _, err := s.Put(ctx, Record("acme", "docs-bot", "1.0.0", "Answer.")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Get(ctx, "globex", "docs-bot", "1.0.0"); !errors.Is(err, definition.ErrNotFound) {
			t.Errorf("cross-tenant get = %v, want ErrNotFound", err)
		}
		if _, err := s.Latest(ctx, "globex", "docs-bot"); !errors.Is(err, definition.ErrNotFound) {
			t.Errorf("cross-tenant latest = %v, want ErrNotFound", err)
		}
		if _, err := s.Versions(ctx, "globex", "docs-bot"); !errors.Is(err, definition.ErrNotFound) {
			t.Errorf("cross-tenant versions = %v, want ErrNotFound", err)
		}
		if err := s.Retire(ctx, "globex", "docs-bot", "eve"); !errors.Is(err, definition.ErrNotFound) {
			t.Errorf("cross-tenant retire = %v, want ErrNotFound", err)
		}
		list, _, err := s.List(ctx, "globex", definition.Page{})
		if err != nil || len(list) != 0 {
			t.Errorf("cross-tenant list = %v, %v", list, err)
		}
	})

	t.Run("LatestIsTheHighestVersion", func(t *testing.T) {
		s := newSource(t)
		for _, v := range []string{"1.2.0", "1.10.0", "1.9.3", "2.0.0-rc.1"} {
			if _, err := s.Put(ctx, Record("acme", "docs-bot", v, "v"+v)); err != nil {
				t.Fatal(err)
			}
		}
		latest, err := s.Latest(ctx, "acme", "docs-bot")
		if err != nil || latest.Version != "2.0.0-rc.1" {
			t.Errorf("latest = %s, %v; want 2.0.0-rc.1", latest.Version, err)
		}
		all, err := s.Versions(ctx, "acme", "docs-bot")
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, r := range all {
			got = append(got, r.Version)
		}
		if fmt.Sprint(got) != "[1.2.0 1.9.3 1.10.0 2.0.0-rc.1]" {
			t.Errorf("versions = %v, want semantic order", got)
		}
	})

	t.Run("RetireKeepsGetWorking", func(t *testing.T) {
		s := newSource(t)
		if _, err := s.Put(ctx, Record("acme", "docs-bot", "1.0.0", "Answer.")); err != nil {
			t.Fatal(err)
		}
		if err := s.Retire(ctx, "acme", "docs-bot", "ada"); err != nil {
			t.Fatalf("retire: %v", err)
		}
		if err := s.Retire(ctx, "acme", "docs-bot", "ada"); err != nil {
			t.Errorf("second retire: %v", err)
		}
		if _, err := s.Get(ctx, "acme", "docs-bot", "1.0.0"); err != nil {
			t.Errorf("get after retire: %v", err)
		}
		if all, err := s.Versions(ctx, "acme", "docs-bot"); err != nil || len(all) != 1 {
			t.Errorf("versions after retire = %d, %v", len(all), err)
		}
		if _, err := s.Latest(ctx, "acme", "docs-bot"); !errors.Is(err, definition.ErrRetired) {
			t.Errorf("latest after retire = %v, want ErrRetired", err)
		}
		if _, err := s.Put(ctx, Record("acme", "docs-bot", "2.0.0", "Again.")); !errors.Is(err, definition.ErrRetired) {
			t.Errorf("put after retire = %v, want ErrRetired", err)
		}
		if list, _, _ := s.List(ctx, "acme", definition.Page{}); len(list) != 0 {
			t.Errorf("list after retire = %v", list)
		}
		if err := s.Retire(ctx, "acme", "nobody", "ada"); !errors.Is(err, definition.ErrNotFound) {
			t.Errorf("retire of a missing name = %v", err)
		}
		if err := s.Retire(ctx, "acme", "docs-bot", " "); !errors.Is(err, definition.ErrInvalid) {
			t.Errorf("retire with no author = %v", err)
		}
	})

	t.Run("ListPagesWithATenantBoundToken", func(t *testing.T) {
		s := newSource(t)
		for _, n := range []string{"c", "a", "b", "e", "d"} {
			if _, err := s.Put(ctx, Record("acme", n, "1.0.0", "x")); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.Put(ctx, Record("acme", "b", "1.1.0", "y")); err != nil {
			t.Fatal(err)
		}
		var names []string
		token := ""
		for range 10 {
			page, next, err := s.List(ctx, "acme", definition.Page{Size: 2, Token: token})
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range page {
				names = append(names, r.Name+"@"+r.Version)
			}
			if next == "" {
				break
			}
			if _, _, err := s.List(ctx, "globex", definition.Page{Size: 2, Token: next}); !errors.Is(err, definition.ErrInvalid) {
				t.Errorf("another tenant's token = %v, want ErrInvalid", err)
			}
			token = next
		}
		if fmt.Sprint(names) != "[a@1.0.0 b@1.1.0 c@1.0.0 d@1.0.0 e@1.0.0]" {
			t.Errorf("listed %v", names)
		}
	})

	t.Run("PolicyRoundTrips", func(t *testing.T) {
		s := newSource(t)
		rec := Record("acme", "docs-bot", "1.0.0", "Answer.")
		rec.Policy = definition.Policy{
			WorkspaceAreas: []string{"notes/", "drafts/"},
			Knowledge:      []definition.Corpus{{Name: "docs", URI: "kb://docs", Options: map[string]string{"k": "v"}}},
			Subscriptions:  []definition.Subscription{{Channel: "slack://T01", Types: []string{"message."}}},
			TaggedOnly:     true,
		}
		put, err := s.Put(ctx, rec)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(ctx, "acme", "docs-bot", "1.0.0")
		if err != nil {
			t.Fatal(err)
		}
		if got.Policy.Hash() != put.PolicyHash || got.PolicyHash != put.PolicyHash || put.PolicyHash != rec.Policy.Hash() {
			t.Errorf("policy hash: put %s, stored %s, read back %s, want %s", put.PolicyHash, got.PolicyHash, got.Policy.Hash(), rec.Policy.Hash())
		}
	})
}
