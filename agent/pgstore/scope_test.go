package pgstore

import (
	"context"
	"strings"
	"testing"
)

func TestScopedConversationID(t *testing.T) {
	tests := []struct {
		name         string
		scope, conv  string
		wantErr      bool
		wantScope    string
		wantConv     string
		wantSplitted bool
	}{
		{name: "scoped", scope: "tenant-a", conv: "conv-1", wantScope: "tenant-a", wantConv: "conv-1", wantSplitted: true},
		{name: "empty conversation stays scoped", scope: "tenant-a", conv: "", wantScope: "tenant-a", wantConv: "", wantSplitted: true},
		{name: "conversation may contain the separator", scope: "t", conv: "a\x1fb", wantScope: "t", wantConv: "a\x1fb", wantSplitted: true},
		{name: "empty scope is rejected", scope: "", conv: "conv-1", wantErr: true},
		{name: "separator in scope is rejected", scope: "a\x1fb", conv: "conv-1", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := ScopedConversationID(tt.scope, tt.conv)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if id == tt.conv {
				t.Fatalf("scoped ID %q equals the unscoped ID", id)
			}
			scope, conv, ok := SplitConversationID(id)
			if ok != tt.wantSplitted || scope != tt.wantScope || conv != tt.wantConv {
				t.Fatalf("split = (%q, %q, %v)", scope, conv, ok)
			}
		})
	}
	// Different tenants with the same conversation ID get different namespaces.
	a, _ := ScopedConversationID("a", "conv")
	b, _ := ScopedConversationID("b", "conv")
	if a == b {
		t.Fatal("tenants share a namespace")
	}
	// Scope boundaries cannot be shifted into the conversation ID.
	x, _ := ScopedConversationID("ab", "c")
	y, _ := ScopedConversationID("a", "bc")
	if x == y {
		t.Fatal("scope boundary is ambiguous")
	}
	for _, unscoped := range []string{"", "conv-1", "tenant/conv"} {
		if scope, conv, ok := SplitConversationID(unscoped); ok || scope != "" || conv != unscoped {
			t.Fatalf("unscoped %q split as (%q, %q, %v)", unscoped, scope, conv, ok)
		}
	}
	if _, _, ok := SplitConversationID("\x1fno-second-separator"); ok {
		t.Fatal("malformed ID split as scoped")
	}
	if !strings.HasPrefix(a, scopeSeparator) {
		t.Fatalf("scoped ID %q lacks the separator prefix", a)
	}
}

// TestScopedStoresIsolateTenants shows that two tenants using the same
// conversation ID do not see each other's branches or nodes.
func TestScopedStoresIsolateTenants(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	storeA, err := New(Config{Pool: pool, Scope: "tenant-a", ConversationID: "conv"})
	if err != nil {
		t.Fatal(err)
	}
	storeB, err := New(Config{Pool: pool, Scope: "tenant-b", ConversationID: "conv"})
	if err != nil {
		t.Fatal(err)
	}
	if storeA.ConversationID() == storeB.ConversationID() {
		t.Fatal("tenants share a conversation namespace")
	}
	saveConversation(t, storeA, "scope-root-a", "scope-tip-a")
	saveConversation(t, storeB, "scope-root-b", "scope-tip-b")
	tipB, err := storeB.LoadBranch(ctx, "main")
	if err != nil || tipB != "scope-tip-b" {
		t.Fatalf("tenant B main = %s, %v", tipB, err)
	}
	if _, err := storeB.LoadNode(ctx, "scope-tip-a"); err == nil {
		t.Fatal("tenant B loaded tenant A's node")
	}
	if err := storeA.DeleteConversation(ctx); err != nil {
		t.Fatal(err)
	}
	if tipB, err := storeB.LoadBranch(ctx, "main"); err != nil || tipB != "scope-tip-b" {
		t.Fatalf("deleting tenant A touched tenant B: %s, %v", tipB, err)
	}
}
