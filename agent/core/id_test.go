package core

import "testing"

func TestNewIDUniqueness(t *testing.T) {
	seen := make(map[string]bool)
	for range 100 {
		id := NewID()
		if id == "" {
			t.Fatal("NewID returned empty string")
		}
		if seen[id] {
			t.Fatalf("duplicate ID: %s", id)
		}
		seen[id] = true
	}
}

func TestNewIDFormat(t *testing.T) {
	id := NewID()
	// UUID v4 format: 8-4-4-4-12
	if len(id) != 36 {
		t.Errorf("ID length = %d, want 36", len(id))
	}
}
