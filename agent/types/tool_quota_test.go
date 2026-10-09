package types

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestBudgetToolQuota(t *testing.T) {
	tests := []struct {
		name      string
		quota     int
		calls     int
		admitted  int
		remaining int
		limited   bool
	}{
		{name: "under quota", quota: 3, calls: 2, admitted: 2, remaining: 1, limited: true},
		{name: "at quota", quota: 2, calls: 4, admitted: 2, remaining: 0, limited: true},
		{name: "no quota counts calls", quota: 0, calls: 5, admitted: 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := NewBudget(BudgetPolicy{}).ToolQuota("search", tt.quota)
			admitted := 0
			for range tt.calls {
				err := b.ReserveToolCall("search")
				switch {
				case err == nil:
					admitted++
				case !errors.Is(err, ErrToolQuotaExceeded):
					t.Fatalf("err = %v", err)
				}
			}
			if admitted != tt.admitted || b.ToolCalls("search") != tt.admitted {
				t.Errorf("admitted = %d, ToolCalls = %d, want %d", admitted, b.ToolCalls("search"), tt.admitted)
			}
			rem, ok := b.ToolQuotaRemaining("search")
			if rem != tt.remaining || ok != tt.limited {
				t.Errorf("remaining = %d, %v; want %d, %v", rem, ok, tt.remaining, tt.limited)
			}
			if err := b.ReserveToolCall("other"); err != nil {
				t.Errorf("a quota on one tool limited another: %v", err)
			}
		})
	}

	t.Run("removing a quota lifts it", func(t *testing.T) {
		b := NewBudget(BudgetPolicy{}).ToolQuota("x", 1)
		_ = b.ReserveToolCall("x")
		if err := b.ReserveToolCall("x"); err == nil {
			t.Fatal("quota not enforced")
		}
		b.ToolQuota("x", 0)
		if err := b.ReserveToolCall("x"); err != nil {
			t.Fatalf("quota not removed: %v", err)
		}
	})
}

func TestBudgetToolQuotaConcurrent(t *testing.T) {
	b := NewBudget(BudgetPolicy{}).ToolQuota("x", 7)
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b.ReserveToolCall("x") == nil {
				admitted.Add(1)
			}
		}()
	}
	wg.Wait()
	if admitted.Load() != 7 {
		t.Fatalf("admitted %d concurrent calls, quota 7", admitted.Load())
	}
}

func TestCapabilityGate(t *testing.T) {
	tests := []struct {
		name   string
		policy CapabilityPolicy
		class  ToolCapability
		want   GateOutcome
	}{
		{"default read allows", nil, ToolCapabilityRead, GateAllow},
		{"default write asks", nil, ToolCapabilityWrite, GateRequireApproval},
		{"default destructive denies", nil, ToolCapabilityDestructive, GateDeny},
		{"default unknown asks", nil, ToolCapabilityUnknown, GateRequireApproval},
		{"zero value is unknown", nil, "", GateRequireApproval},
		{"unrecognized is unknown", nil, "admin", GateRequireApproval},
		{"custom allows write", CapabilityPolicy{ToolCapabilityWrite: GateAllow}, ToolCapabilityWrite, GateAllow},
		{"missing class asks", CapabilityPolicy{ToolCapabilityRead: GateAllow}, ToolCapabilityDestructive, GateRequireApproval},
		{"policy keyed by zero value means unknown", CapabilityPolicy{"": GateDeny}, ToolCapabilityUnknown, GateDeny},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := CapabilityGate(tt.policy).Check(context.Background(), ToolDef{Name: "t", Capability: tt.class}, nil)
			if d.Outcome != tt.want {
				t.Errorf("outcome = %v, want %v (%s)", d.Outcome, tt.want, d.Reason)
			}
			if d.Outcome != GateAllow && d.Reason == "" {
				t.Error("a refusal or approval needs a reason")
			}
		})
	}

	t.Run("later changes to the map do not leak in", func(t *testing.T) {
		p := CapabilityPolicy{ToolCapabilityWrite: GateAllow}
		g := CapabilityGate(p)
		p[ToolCapabilityWrite] = GateDeny
		if d := g.Check(context.Background(), ToolDef{Capability: ToolCapabilityWrite}, nil); d.Outcome != GateAllow {
			t.Errorf("outcome = %v", d.Outcome)
		}
	})
}
