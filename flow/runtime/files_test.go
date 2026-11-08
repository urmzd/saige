package runtime_test

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	saigetypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/durable"
	"github.com/urmzd/saige/sandbox/controlplane"
	"github.com/urmzd/saige/sandbox/metrics"
	"github.com/urmzd/saige/sandbox/node/inproc"
	"github.com/urmzd/saige/sandbox/tool"
	"github.com/urmzd/saige/sandbox/workspace"

	"github.com/urmzd/saige/flow/attachment"
	"github.com/urmzd/saige/flow/attachment/intake"
	"github.com/urmzd/saige/flow/attachment/resolve"
	"github.com/urmzd/saige/flow/blob"
	"github.com/urmzd/saige/flow/runtime"
	"github.com/urmzd/saige/flow/thread"
	"github.com/urmzd/saige/flow/turn/native"
)

// A file attached in one turn is part of the conversation from then on: the
// next run in the thread is shown it again, after the message it came with,
// and the whole of the first run's request is a byte prefix of the second
// run's, which is what keeps the thread's prompt cache warm. A file that could
// not be fetched is shown as a note the same way, on both turns.
func TestAFileAttachedInOneTurnIsStillSeenInTheNextWithAByteIdenticalPrefix(t *testing.T) {
	ctx := context.Background()
	files := blob.NewMemory()
	trace, err := intake.Store(ctx, files, "acme", strings.NewReader("panic: nil map write\n\tat main.go:42\n"), intake.Meta{Name: "trace.txt"})
	if err != nil {
		t.Fatal(err)
	}
	lost := attachment.Missing("screen.mov", tag("docs-bot", "x", "").Source, "", "it is larger than the 25.0 MB limit for one file")

	model := &agenttest.ScriptedProvider{Responses: [][]saigetypes.Delta{
		agenttest.TextResponse("A nil map is written at main.go:42."),
		agenttest.TextResponse("Initialize the map before line 42."),
	}}
	sp := newSpecs()
	sp.revise("docs-bot", "You debug things.")
	ws, err := workspace.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tools := tool.NewRegistry()
	plane := controlplane.NewMemory(inproc.NewFactory(tools, ws), metrics.Nop())
	sandbox, err := runtime.NewSandbox(plane, tools, sp, runtime.WithAgentOptions(
		native.WithAttachments(resolve.New(files)),
		native.WithProvider(native.ProviderOffline, func(native.Spec) (saigetypes.Provider, error) { return model, nil }),
	))
	if err != nil {
		t.Fatal(err)
	}

	w := newWorld(t)
	w.specs = sp
	rt, err := runtime.New(durable.New(w.ledger, w.queue), sp, w.bus, runtime.WithRuntime(sandbox), runtime.WithThreads(w.threads))
	if err != nil {
		t.Fatal(err)
	}
	w.work(t, rt, nil)

	first := tag("docs-bot", "Ev-files-1", "why does this crash?")
	first.Attachments = []attachment.Attachment{trace, lost}
	w.result(t, rt, w.accept(t, rt, first))
	second := tag("docs-bot", "Ev-files-2", "and how do I fix it?")
	w.result(t, rt, w.accept(t, rt, second))

	asked := model.Requests()
	if len(asked) != 2 {
		t.Fatalf("the model was called %d times, want once per turn", len(asked))
	}
	one, two := asked[0].Messages, asked[1].Messages
	if len(two) != len(one)+2 {
		t.Fatalf("the second request has %d messages, want the first request's %d plus its answer and the new question", len(two), len(one))
	}
	if a, b := encode(t, one), encode(t, two[:len(one)]); !bytes.Equal(a, b) {
		t.Fatalf("the first request is not a byte prefix of the second:\n first %s\nsecond %s", a, b)
	}
	replayed := encode(t, two[len(one)-1:len(one)])
	for _, want := range []string{"why does this crash?", "panic: nil map write", `name=\"screen.mov\"`, "larger than the 25.0 MB limit"} {
		if !strings.Contains(string(replayed), want) {
			t.Errorf("the second turn was not shown %q with the first question:\n%s", want, replayed)
		}
	}
	if strings.Contains(encode(t, two[len(two)-1:]).String(), "panic") {
		t.Error("the earlier file was attached to the new question as well")
	}

	entries := journal(t, w.threads, first.key())
	if len(entries) != 4 || !reflect.DeepEqual(entries[0].Attachments, first.Attachments) || entries[2].Attachments != nil {
		t.Fatalf("journal = %+v, want the files on the first question only", entries)
	}
}

// A stopped turn keeps its journal contract with files on its request: the
// request is journaled with its attachments, then exactly one assistant
// entry of the partial answer and the fixed marker.
func TestAStoppedTurnJournalsItsFilesWithTheRequestAndKeepsTheMarker(t *testing.T) {
	w := newWorld(t)
	exec := newPatient("Half an ")
	rt := w.runtime(t, exec)
	w.work(t, rt, nil)

	tg := tag("docs-bot", "evt-files-stop", "explain this log")
	shot := attachment.Attachment{Digest: strings.Repeat("d", 64), MediaType: "text/plain", Name: "build.log", Size: 12}
	tg.Attachments = []attachment.Attachment{shot, attachment.Missing("big.zip", tg.Source, "", "its host is not one files are fetched from")}
	runID := w.accept(t, rt, tg)
	exec.inFlight(t)
	w.stop(t, rt, "acme", runID, "enough", "U1")
	out := w.result(t, rt, runID)
	if out.Stopped == nil {
		t.Fatalf("Output = %+v, want stopped", out)
	}

	var mine []thread.Entry
	for _, e := range journal(t, w.threads, tg.key()) {
		if e.RunID == runID {
			mine = append(mine, e)
		}
	}
	if got, want := said(mine), []string{"user: explain this log", "assistant: Half an\n\n" + runtime.StopMarker}; !reflect.DeepEqual(got, want) {
		t.Fatalf("journal = %q, want %q", got, want)
	}
	if !reflect.DeepEqual(mine[0].Attachments, tg.Attachments) || mine[1].Attachments != nil {
		t.Fatalf("attachments journaled as %+v and %+v, want the request's files on the request only", mine[0].Attachments, mine[1].Attachments)
	}
}

type encoded []byte

func (e encoded) String() string { return string(e) }

func encode(t *testing.T, msgs []saigetypes.Message) encoded {
	t.Helper()
	b, err := json.Marshal(msgs)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
