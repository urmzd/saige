package main

import (
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/cmd/internal/approvals"
)

func TestApprovalsCommands(t *testing.T) {
	agentsSandbox(t)
	dir := t.TempDir()
	store, err := approvals.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	put := func(maxGrant types.GrantScope) string {
		p := approvals.Pending{Token: approvals.NewToken(), Agent: "helper@1.0.0", Tool: "write_file",
			Arguments: map[string]any{"path": "a.txt"}, MaxGrant: maxGrant, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
		if err := store.Put(p); err != nil {
			t.Fatal(err)
		}
		return p.Token
	}
	if code, out := runCLI(t, "approvals", "list", "--dir", dir); code != 0 || !strings.Contains(out, "no held approvals") {
		t.Fatalf("empty list: %d %s", code, out)
	}
	a, b, c := put(""), put(types.GrantOnce), put("")
	if code, out := runCLI(t, "approvals", "list", "--dir", dir); code != 0 || !strings.Contains(out, a) || !strings.Contains(out, `{"path":"a.txt"}`) {
		t.Fatalf("list: %d %s", code, out)
	}
	if code, out := runCLI(t, "approvals", "approve", a, "--grant", "tool", "--dir", dir); code != 0 || !strings.Contains(out, "approved") {
		t.Fatalf("approve: %d %s", code, out)
	}
	if e, _ := store.Get(a); e.Decision == nil || !e.Decision.Approved || e.Decision.Grant.Scope != types.GrantTool {
		t.Fatalf("decision = %+v", e.Decision)
	}
	if code, out := runCLI(t, "approvals", "approve", b, "--grant", "session", "--dir", dir); code != exitInvalid || !strings.Contains(out, "allows grants up to") {
		t.Fatalf("grant beyond the cap: %d %s", code, out)
	}
	if code, out := runCLI(t, "approvals", "deny", c, "--message", "not now", "--dir", dir); code != 0 || !strings.Contains(out, "denied") {
		t.Fatalf("deny: %d %s", code, out)
	}
	if code, _ := runCLI(t, "approvals", "deny", c, "--dir", dir); code != exitInvalid {
		t.Fatal("a token decided twice")
	}
	if code, out := runCLI(t, "approvals", "list", "--dir", dir, "--format", "json"); code != 0 || !strings.Contains(out, `"approved": true`) {
		t.Fatalf("json list: %d %s", code, out)
	}
}
