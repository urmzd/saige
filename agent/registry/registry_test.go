package registry

import (
	"testing"
	"time"
)

func TestRegisterAppendsRatherThanOverwrites(t *testing.T) {
	r := New[string]()

	e1 := r.Register("greeting", "hello", WithSource("init"))
	e2 := r.Register("greeting", "hi", WithSource("operator"), WithNote("shorter"))

	if e1.Revision != 1 || e2.Revision != 2 {
		t.Fatalf("revisions = %d, %d; want 1, 2", e1.Revision, e2.Revision)
	}
	if got, _ := r.Get("greeting"); got != "hi" {
		t.Errorf("Get = %q, want the newest revision", got)
	}
	if e, _ := r.At("greeting", 1); e.Value != "hello" {
		t.Error("the earlier revision must remain resolvable, or a rollback has nothing to go back to")
	}
	if n := len(r.History("greeting")); n != 2 {
		t.Errorf("history length = %d, want 2", n)
	}
}

func TestProvenanceIsRecorded(t *testing.T) {
	r := New[int]()
	at := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	e := r.Register("x", 1, WithSource("config.yaml"), WithNote("bumped limit"), WithTime(at))

	if e.Source != "config.yaml" || e.Note != "bumped limit" || !e.RecordedAt.Equal(at) {
		t.Errorf("entry = %+v, want the supplied provenance", e)
	}
}

func TestUnknownNameResolvesToNothing(t *testing.T) {
	r := New[string]()
	if _, ok := r.Get("absent"); ok {
		t.Error("an unregistered name must not resolve")
	}
	if len(r.History("absent")) != 0 {
		t.Error("an unregistered name must have no history")
	}
}

func TestPinFreezesResolutionAgainstLaterRevisions(t *testing.T) {
	r := New[string]()
	r.Register("m", "v1")
	r.Register("m", "v2")

	if err := r.Pin("m", 1); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	r.Register("m", "v3")

	if got, _ := r.Get("m"); got != "v1" {
		t.Errorf("Get = %q, want the pinned v1: a pin must survive later registrations", got)
	}
	if e, _ := r.Latest("m"); e.Value != "v3" {
		t.Errorf("Latest = %q, want v3: a pin must not hide the newest revision", e.Value)
	}

	r.Unpin("m")
	if got, _ := r.Get("m"); got != "v3" {
		t.Errorf("after Unpin, Get = %q, want v3", got)
	}
}

func TestPinRejectsUnknownNameAndOutOfRangeRevision(t *testing.T) {
	r := New[string]()
	if err := r.Pin("absent", 1); err == nil {
		t.Error("pinning an unknown name must error")
	}
	r.Register("m", "v1")
	if err := r.Pin("m", 5); err == nil {
		t.Error("pinning a revision that does not exist must error, not silently resolve to the latest")
	}
	if err := r.Pin("m", 0); err == nil {
		t.Error("revision 0 must be rejected: revisions start at 1")
	}
}

func TestRollbackStepsBackOneRevisionAtATime(t *testing.T) {
	r := New[string]()
	r.Register("m", "v1")
	r.Register("m", "v2")
	r.Register("m", "v3")

	e, err := r.Rollback("m")
	if err != nil || e.Value != "v2" {
		t.Fatalf("Rollback = %q, %v; want v2, nil", e.Value, err)
	}
	if got, _ := r.Get("m"); got != "v2" {
		t.Errorf("Get = %q, want v2 after rollback", got)
	}

	// Rolling back again steps from the pinned revision, not from latest.
	if e, err = r.Rollback("m"); err != nil || e.Value != "v1" {
		t.Fatalf("second Rollback = %q, %v; want v1, nil", e.Value, err)
	}
	if _, err := r.Rollback("m"); err == nil {
		t.Error("rolling back past revision 1 must error rather than wrap around")
	}
}

func TestAllReturnsTheResolvingEntryPerName(t *testing.T) {
	r := New[string]()
	r.Register("b", "b1")
	r.Register("a", "a1")
	r.Register("a", "a2")
	_ = r.Pin("a", 1)

	all := r.All()
	if len(all) != 2 {
		t.Fatalf("All returned %d entries, want 2 (one per name)", len(all))
	}
	if all[0].Name != "a" || all[0].Value != "a1" {
		t.Errorf("All[0] = %+v, want the pinned a1 first (sorted by name)", all[0])
	}
	if all[1].Value != "b1" {
		t.Errorf("All[1] = %+v, want b1", all[1])
	}
}

func TestConcurrentRegistrationsAllGetDistinctRevisions(t *testing.T) {
	r := New[int]()
	const n = 50
	done := make(chan struct{})
	for i := 0; i < n; i++ {
		go func(i int) {
			r.Register("m", i)
			close(make(chan struct{}))
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < n; i++ {
		<-done
	}

	hist := r.History("m")
	if len(hist) != n {
		t.Fatalf("history length = %d, want %d", len(hist), n)
	}
	seen := map[Revision]bool{}
	for _, e := range hist {
		if seen[e.Revision] {
			t.Fatalf("revision %d assigned twice", e.Revision)
		}
		seen[e.Revision] = true
	}
}
