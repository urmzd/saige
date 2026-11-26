package native_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	saigetypes "github.com/urmzd/saige/agent/types"

	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/turn/native"
)

// streamArguments feeds tr an open tool call's arguments in fragments of size
// bytes each, and returns every event the fragments became.
func streamArguments(t *testing.T, tr *native.Translator, path []string, index int, arguments string, size int) []event.ToolCall {
	t.Helper()
	var out []event.ToolCall
	for len(arguments) > 0 {
		n := min(size, len(arguments))
		var d saigetypes.Delta = saigetypes.PartDelta{Index: index, Args: arguments[:n]}
		for i := len(path) - 1; i >= 0; i-- {
			d = saigetypes.ToolExecDelta{ToolCallID: path[i], Inner: d}
		}
		arguments = arguments[n:]
		if c, ok := tr.Delta(d); ok {
			out = append(out, decodeCall(t, c))
		}
	}
	return out
}

// document is a file_write call's arguments, as a model streams them, whose
// content is n bytes long.
func document(n int) string {
	return `{"path":"report.md","content":"` + strings.Repeat("s3cret ", n/7+1)[:n] + `"}`
}

// A call whose arguments never reach the first step says nothing while they
// are written: the request and the execution are close enough together.
func TestArgumentsUnderOneStepReportNoProgress(t *testing.T) {
	t.Parallel()

	tr := native.NewTranslator()
	tr.Delta(saigetypes.PartStart{Index: 0, Kind: saigetypes.KindToolCall, ID: "c1", Name: "file_write"})
	arguments := document(event.ToolArgumentsStep - 100)
	if len(arguments) >= event.ToolArgumentsStep {
		t.Fatalf("the test's arguments are %d bytes, want under one step", len(arguments))
	}
	if got := streamArguments(t, tr, nil, 0, arguments, 16); len(got) != 0 {
		t.Errorf("arguments of %d bytes published %d progress events, want none: %+v", len(arguments), len(got), got)
	}
}

// While a model writes a long call there is one event per step its arguments
// pass, however small its fragments are, each naming the same call and
// carrying how much has arrived.
func TestArgumentProgressIsReportedOncePerStep(t *testing.T) {
	t.Parallel()

	const fragment = 100
	arguments := document(5*event.ToolArgumentsStep + 300)
	tr := native.NewTranslator()
	tr.Delta(saigetypes.PartStart{Index: 0, Kind: saigetypes.KindToolCall, ID: "c1", Name: "file_write"})
	got := streamArguments(t, tr, nil, 0, arguments, fragment)

	// The size is the bytes received when a step was passed: the end of
	// the fragment that passed it.
	var want []int
	for n := 1; n <= len(arguments)/event.ToolArgumentsStep; n++ {
		step := n * event.ToolArgumentsStep
		want = append(want, min((step+fragment-1)/fragment*fragment, len(arguments)))
	}
	if len(want) != 5 {
		t.Fatalf("the test expects %d steps, want 5", len(want))
	}
	var sizes []int
	for _, c := range got {
		sizes = append(sizes, c.ArgumentsBytes)
		if c.ID != "c1" || c.Name != "file_write" || c.Phase != event.PhaseRequested {
			t.Errorf("progress names %q %q in phase %q, want the call c1 file_write, requested", c.ID, c.Name, c.Phase)
		}
		if c.Parent != nil {
			t.Errorf("the turn's own call's progress has parent %v", c.Parent)
		}
	}
	if !slices.Equal(sizes, want) {
		t.Errorf("progress sizes are %v, want %v", sizes, want)
	}
}

// One fragment that passes several steps is one event with the size reached,
// and a fragment that passes none is silent.
func TestAFragmentPastSeveralStepsIsReportedOnce(t *testing.T) {
	t.Parallel()

	tr := native.NewTranslator()
	tr.Delta(saigetypes.PartStart{Index: 0, Kind: saigetypes.KindToolCall, ID: "c1", Name: "file_write"})
	big := strings.Repeat("x", 3*event.ToolArgumentsStep+10)
	c, ok := tr.Delta(saigetypes.PartDelta{Index: 0, Args: big})
	if !ok {
		t.Fatal("a fragment past three steps produced no event")
	}
	if got := decodeCall(t, c).ArgumentsBytes; got != len(big) {
		t.Errorf("progress reports %d bytes, want %d", got, len(big))
	}
	if c, ok := tr.Delta(saigetypes.PartDelta{Index: 0, Args: "xx"}); ok {
		t.Errorf("a fragment within the step already reported produced %s", raw(c))
	}
}

// Arguments can hold what a deployment scrubs, and a fragment cannot be
// scrubbed. Progress carries a size and nothing of the arguments, however the
// model cuts its fragments.
func TestArgumentProgressNeverCarriesArguments(t *testing.T) {
	t.Parallel()

	arguments := document(4 * event.ToolArgumentsStep)
	tr := native.NewTranslator()
	for index, size := range []int{1, 333, len(arguments)} {
		tr.Delta(saigetypes.PartStart{Index: index, Kind: saigetypes.KindToolCall, ID: "c" + string(rune('1'+index)), Name: "file_write"})
		got := streamArguments(t, tr, nil, index, arguments, size)
		if len(got) == 0 {
			t.Fatalf("fragments of %d bytes published no progress", size)
		}
		for _, c := range got {
			if c.Arguments != nil {
				t.Errorf("progress carries arguments: %s", c.Arguments)
			}
			b, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), "s3cret") || strings.Contains(string(b), "report.md") || strings.Contains(string(b), `"arguments"`) {
				t.Errorf("progress leaks the arguments: %s", b)
			}
		}
	}
}

// A nested agent's call reports its progress under the calls it came through,
// and two agents' calls with the same part index and id are sized apart.
func TestANestedCallsProgressKeepsItsParentPath(t *testing.T) {
	t.Parallel()

	under := func(path []string, d saigetypes.Delta) saigetypes.Delta {
		for i := len(path) - 1; i >= 0; i-- {
			d = saigetypes.ToolExecDelta{ToolCallID: path[i], Inner: d}
		}
		return d
	}
	tr := native.NewTranslator()
	paths := [][]string{nil, {"a"}, {"a", "b"}}
	for _, path := range paths {
		tr.Delta(under(path, saigetypes.PartStart{Index: 0, Kind: saigetypes.KindToolCall, ID: "c1", Name: "file_write"}))
	}
	for i, path := range paths {
		// Each agent's call is a step longer than the one above it.
		arguments := document((i + 1) * event.ToolArgumentsStep)
		got := streamArguments(t, tr, path, 0, arguments, 64)
		if len(got) != i+1 {
			t.Fatalf("the call at %v published %d progress events, want %d", path, len(got), i+1)
		}
		for _, c := range got {
			if !slices.Equal(c.Parent, path) {
				t.Errorf("progress of the call at %v has parent %v", path, c.Parent)
			}
			if c.ID != "c1" || c.Phase != event.PhaseRequested || c.Arguments != nil {
				t.Errorf("progress of the call at %v is %+v", path, c)
			}
		}
		if last := got[len(got)-1].ArgumentsBytes; last > len(arguments) || last < (i+1)*event.ToolArgumentsStep {
			t.Errorf("the call at %v last reported %d of %d bytes", path, last, len(arguments))
		}
	}
}

// Progress changes nothing about the events that follow it: the executing
// call and the done still carry the arguments whole, or their size when they
// are over the cap.
func TestExecutingAndDoneCarryTheArgumentsAfterProgress(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content int
		whole   bool
	}{
		{name: "arguments within the cap", content: 2 * event.ToolArgumentsStep, whole: true},
		{name: "arguments over the cap", content: event.MaxToolArguments + event.ToolArgumentsStep},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			arguments := document(tc.content)
			var compact map[string]any
			if err := json.Unmarshal([]byte(arguments), &compact); err != nil {
				t.Fatal(err)
			}
			want, err := json.Marshal(compact)
			if err != nil {
				t.Fatal(err)
			}

			tr := native.NewTranslator()
			tr.Delta(saigetypes.PartStart{Index: 0, Kind: saigetypes.KindToolCall, ID: "c1", Name: "file_write"})
			if got := streamArguments(t, tr, nil, 0, arguments, 200); len(got) == 0 {
				t.Fatal("the arguments published no progress")
			}
			if c, ok := tr.Delta(saigetypes.PartEnd{Index: 0}); ok {
				t.Fatalf("the end of the call's part produced %s", raw(c))
			}

			c, _ := tr.Delta(saigetypes.ToolExecStartDelta{ToolCallID: "c1", Name: "file_write"})
			call := decodeCall(t, c)
			c, _ = tr.Delta(saigetypes.ToolExecEndDelta{ToolCallID: "c1", Name: "file_write", Result: "ok"})
			done := decodeDone(t, c)
			if call.Phase != event.PhaseExecuting {
				t.Errorf("the call ran in phase %q", call.Phase)
			}
			if tc.whole {
				if string(call.Arguments) != string(want) || call.ArgumentsBytes != 0 {
					t.Errorf("executing carries %d bytes of arguments and reports %d, want the %d whole", len(call.Arguments), call.ArgumentsBytes, len(want))
				}
				if string(done.Arguments) != string(want) || done.ArgumentsBytes != 0 {
					t.Errorf("done carries %d bytes of arguments and reports %d, want the %d whole", len(done.Arguments), done.ArgumentsBytes, len(want))
				}
				return
			}
			if call.Arguments != nil || call.ArgumentsBytes != len(want) {
				t.Errorf("executing carries %d bytes of arguments and reports %d, want none and %d", len(call.Arguments), call.ArgumentsBytes, len(want))
			}
			if done.Arguments != nil || done.ArgumentsBytes != len(want) {
				t.Errorf("done carries %d bytes of arguments and reports %d, want none and %d", len(done.Arguments), done.ArgumentsBytes, len(want))
			}
		})
	}
}

// A fragment for a part that was never opened is not a call, and a call with
// no id cannot be told from a second call by a reader that joins on the id.
// Neither reports progress.
func TestProgressNeedsAnOpenCallWithAnID(t *testing.T) {
	t.Parallel()

	arguments := document(3 * event.ToolArgumentsStep)
	tr := native.NewTranslator()
	if got := streamArguments(t, tr, nil, 7, arguments, 128); len(got) != 0 {
		t.Errorf("fragments for a part never opened published %d events", len(got))
	}

	tr.Delta(saigetypes.PartStart{Index: 0, Kind: saigetypes.KindToolCall, Name: "file_write"})
	if got := streamArguments(t, tr, nil, 0, arguments, 128); len(got) != 0 {
		t.Errorf("a call with no id published %d progress events: %+v", len(got), got)
	}
}
