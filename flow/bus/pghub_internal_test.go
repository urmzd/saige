package bus

import "testing"

// A publish wakes the readers of its tenant and no one else.
func TestPgHubWakesOnlyTheTenantsReaders(t *testing.T) {
	h := &pgHub{count: 1}
	a1, _ := h.subscribe("acme")
	a2, _ := h.subscribe("acme")
	b, _ := h.subscribe("globex")

	h.wake("acme")

	if len(a1) != 1 || len(a2) != 1 {
		t.Fatalf("acme readers woken = %d, %d; want both", len(a1), len(a2))
	}
	if len(b) != 0 {
		t.Fatal("a publish to acme woke a globex reader")
	}
}

// An empty payload wakes everyone: the payload only saves queries, it never
// decides what a reader sees.
func TestPgHubEmptyPayloadWakesAll(t *testing.T) {
	h := &pgHub{count: 1}
	a, _ := h.subscribe("acme")
	b, _ := h.subscribe("globex")

	h.wake("")

	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("woken = %d, %d; want both", len(a), len(b))
	}
}

func TestPgHubUnsubscribeStopsListening(t *testing.T) {
	h := &pgHub{}
	stopped := false
	h.cancel = func() { stopped = true }
	h.count = 1
	_, un := h.subscribe("acme")
	h.count--
	un()
	un() // idempotent
	if h.count != 0 || len(h.subs) != 0 || !stopped {
		t.Fatalf("count=%d subs=%d stopped=%v", h.count, len(h.subs), stopped)
	}
}
