package agent

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

func TestResolveMarkerErr(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newEventStream(ctx, cancel)
	ch, err := s.postInterrupt(s.newInterrupt(types.InterruptApproval, "gate", "call-1", nil, nil, 0, types.InterruptPolicy{}), "call-1")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		id   string
		want error
	}{
		{"unknown id", "nope", ErrUnknownMarker},
		{"pending id", "call-1", nil},
		{"second decision before consumption", "call-1", ErrMarkerResolved},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := s.ResolveMarkerErr(tt.id, Resolution{Approved: true, Message: "ok"})
			if !errors.Is(err, tt.want) || (tt.want == nil && err != nil) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
	if r := <-ch; !r.Decision.Approved || r.Decision.Message != "ok" {
		t.Errorf("resolution = %+v", r)
	}
	// An answered marker stays addressable after the wait, so a repeated
	// decision reports that it was already resolved.
	s.withdrawInterrupt("call-1")
	if err := s.ResolveMarkerErr("call-1", Resolution{}); !errors.Is(err, ErrMarkerResolved) {
		t.Errorf("answered id after the wait: %v", err)
	}
	// An unanswered marker is forgotten once its wait ends.
	if _, err := s.postInterrupt(s.newInterrupt(types.InterruptApproval, "gate", "call-2", nil, nil, 0, types.InterruptPolicy{}), "call-2"); err != nil {
		t.Fatal(err)
	}
	s.withdrawInterrupt("call-2")
	if err := s.ResolveMarkerErr("call-2", Resolution{}); !errors.Is(err, ErrUnknownMarker) {
		t.Errorf("unanswered id after the wait: %v", err)
	}
	// The compatibility wrappers log instead of failing.
	s.ResolveMarker("nope", true, nil)
	s.ResolveMarkerWithMessage("nope", false, nil, "no")
}

func TestStreamCloseNormalizesCancel(t *testing.T) {
	other := errors.New("boom")
	tests := []struct {
		name       string
		err        error
		wantStream bool
		wantCtx    bool
		wantSame   bool
	}{
		{name: "nil", err: nil, wantSame: true},
		{name: "unrelated", err: other, wantSame: true},
		{name: "context canceled", err: context.Canceled, wantStream: true, wantCtx: true},
		{name: "wrapped context canceled", err: errors.Join(other, context.Canceled), wantStream: true, wantCtx: true},
		{name: "stream canceled", err: types.ErrStreamCanceled, wantStream: true, wantSame: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s := newEventStream(ctx, cancel)
			s.close(tt.err)
			got := s.Wait()
			if tt.wantSame && got != tt.err {
				t.Fatalf("err = %v, want unchanged %v", got, tt.err)
			}
			if errors.Is(got, types.ErrStreamCanceled) != tt.wantStream || errors.Is(got, context.Canceled) != tt.wantCtx {
				t.Errorf("err = %v: stream=%v ctx=%v", got, errors.Is(got, types.ErrStreamCanceled), errors.Is(got, context.Canceled))
			}
		})
	}
}

func TestTerminalDeltasSurviveCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := newEventStream(ctx, cancel)
	s.Cancel()
	go func() {
		s.send(types.PartDelta{Index: 0, Text: "may be dropped"})
		s.send(types.ErrorDelta{Error: context.Canceled})
		s.send(types.DoneDelta{})
		s.close(context.Canceled)
	}()
	var gotErr error
	var gotDone bool
	for d := range s.Deltas() {
		switch v := d.(type) {
		case types.ErrorDelta:
			gotErr = v.Error
		case types.DoneDelta:
			gotDone = true
		}
	}
	if !errors.Is(gotErr, types.ErrStreamCanceled) || !gotDone {
		t.Fatalf("terminal deltas lost: err=%v done=%v", gotErr, gotDone)
	}
	if err := s.Wait(); !errors.Is(err, types.ErrStreamCanceled) {
		t.Errorf("Wait = %v", err)
	}
}

func TestTerminalSendDoesNotBlockAbandonedStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := newEventStream(ctx, cancel)
	for range cap(s.deltas) {
		s.deltas <- types.PartDelta{Index: 0}
	}
	s.Cancel()
	finished := make(chan struct{})
	go func() {
		s.send(types.ErrorDelta{Error: context.Canceled})
		s.send(types.DoneDelta{})
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(4 * terminalSendGrace):
		t.Fatal("terminal send blocked on a consumer that stopped reading")
	}
}

func TestInvokeCancelReportsStreamCanceled(t *testing.T) {
	provider := &delayedProvider{ready: make(chan struct{}), response: "never"}
	a := must.Get(New(Config{Provider: provider, SystemPrompt: "sys"}))
	stream := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("hi"))})
	stream.Cancel()
	var sawDone bool
	for d := range stream.Deltas() {
		if _, ok := d.(types.DoneDelta); ok {
			sawDone = true
		}
	}
	if err := stream.Wait(); !errors.Is(err, types.ErrStreamCanceled) {
		t.Errorf("Wait = %v, want ErrStreamCanceled", err)
	}
	if !sawDone {
		t.Error("DoneDelta dropped after cancel")
	}
}

func TestNewRemoteStream(t *testing.T) {
	remoteErr := errors.New("remote failed")
	tests := []struct {
		name    string
		waitErr error
		want    error
	}{
		{"success", nil, nil},
		{"failure", remoteErr, remoteErr},
		{"remote cancel", context.Canceled, types.ErrStreamCanceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := make(chan types.Delta, 4)
			want := []types.Delta{types.PartStart{Index: 0, Kind: types.KindText}, types.PartDelta{Index: 0, Text: "hi"}, types.PartEnd{Index: 0}, types.DoneDelta{}}
			for _, d := range want {
				in <- d
			}
			close(in)
			s := NewRemoteStream(in, func() error { return tt.waitErr }, nil, nil)
			var got []types.Delta
			for d := range s.Deltas() {
				got = append(got, d)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("deltas = %#v", got)
			}
			err := s.Wait()
			if !errors.Is(err, tt.want) || (tt.want == nil && err != nil) {
				t.Errorf("Wait = %v, want %v", err, tt.want)
			}
			if err := s.ResolveMarkerErr("x", Resolution{}); !errors.Is(err, ErrUnknownMarker) {
				t.Errorf("resolve without resolver: %v", err)
			}
		})
	}
}

func TestNewRemoteStreamCancelAndResolve(t *testing.T) {
	in := make(chan types.Delta)
	stop := make(chan struct{})
	var cancels int
	var mu sync.Mutex
	var resolved []string
	s := NewRemoteStream(in,
		func() error { return context.Canceled },
		func() {
			mu.Lock()
			cancels++
			mu.Unlock()
			close(stop)
		},
		func(id string, r Resolution) error {
			mu.Lock()
			defer mu.Unlock()
			resolved = append(resolved, id)
			if !r.Approved {
				return ErrUnknownMarker
			}
			return nil
		})
	go func() {
		in <- types.MarkerDelta{ToolCallID: "call-1"}
		<-stop
		close(in)
	}()
	if d := <-s.Deltas(); d.(types.MarkerDelta).ToolCallID != "call-1" {
		t.Fatalf("first delta = %#v", d)
	}
	if err := s.ResolveMarkerErr("call-1", Resolution{Approved: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveMarkerErr("call-2", Resolution{}); !errors.Is(err, ErrUnknownMarker) {
		t.Errorf("resolver error not returned: %v", err)
	}
	s.ResolveMarker("call-3", true, nil)
	s.Cancel()
	s.Cancel()
	for range s.Deltas() {
	}
	if err := s.Wait(); !errors.Is(err, types.ErrStreamCanceled) {
		t.Errorf("Wait = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if cancels != 1 {
		t.Errorf("remote cancel called %d times", cancels)
	}
	if !reflect.DeepEqual(resolved, []string{"call-1", "call-2", "call-3"}) {
		t.Errorf("resolved = %v", resolved)
	}
}

func TestReplayCarriesToolNamesAndIDs(t *testing.T) {
	msgs := []types.Message{
		types.AssistantMessage{Parts: []types.AssistantPart{
			types.ToolCallPart{ID: "c1", Name: "search", Arguments: map[string]any{"q": "go"}},
		}},
		types.SystemMessage{Parts: []types.SystemPart{types.ToolResultPart{CallID: "c1", Parts: []types.ToolOutputPart{types.Text("found")}}}},
		types.UserMessage{Parts: []types.UserPart{types.ToolResultPart{CallID: "c9", Parts: []types.ToolOutputPart{types.Text("orphan")}}}},
	}
	var got []types.Delta
	for d := range Replay(msgs).Deltas() {
		got = append(got, d)
	}
	want := []types.Delta{
		types.PartStart{Index: 0, Kind: types.KindToolCall, ID: "c1", Name: "search"},
		types.PartEnd{Index: 0, Part: types.ToolCallPart{ID: "c1", Name: "search", Arguments: map[string]any{"q": "go"}}},
		types.ToolExecStartDelta{ToolCallID: "c1", Name: "search"},
		types.ToolExecEndDelta{ToolCallID: "c1", Name: "search", Result: "found"},
		types.ToolExecStartDelta{ToolCallID: "c9"},
		types.ToolExecEndDelta{ToolCallID: "c9", Result: "orphan"},
		types.DoneDelta{},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %#v\nwant %#v", got, want)
	}
}
