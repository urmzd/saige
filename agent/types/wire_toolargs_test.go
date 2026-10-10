package types

import (
	"reflect"
	"testing"
	"time"
)

func TestWireToolCallEndCarriesArgumentsError(t *testing.T) {
	tests := []v1ToolCallEnd{
		{ID: "c1", ArgumentsError: "unexpected end of JSON input"},
		{ID: "c2", Arguments: map[string]any{"path": "/tmp"}},
	}
	for _, in := range tests {
		b, err := MarshalDelta(in)
		if err != nil {
			t.Fatal(err)
		}
		out, err := UnmarshalDelta(b)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(out, in) {
			t.Errorf("round trip = %#v, want %#v", out, in)
		}
	}
}

func TestWireMarkerCarriesInterrupt(t *testing.T) {
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		in   MarkerDelta
	}{
		{"without interrupt", MarkerDelta{ToolCallID: "c1", ToolName: "write"}},
		{"with interrupt", MarkerDelta{ToolCallID: "c1", ToolName: "write", Interrupt: &Interrupt{
			ID: "int_1", RunID: "run", Path: []string{"p"}, Kind: InterruptApproval,
			CreatedAt: created, ExpiresAt: created.Add(time.Minute), Policy: InterruptPolicy{OnExpire: InterruptExpireDeny},
		}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := MarshalDelta(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			got, err := UnmarshalDelta(raw)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.in) {
				t.Fatalf("round trip = %#v, want %#v", got, tt.in)
			}
		})
	}
}
