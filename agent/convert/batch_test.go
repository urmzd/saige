package convert

import (
	"context"
	"errors"
	"iter"
	"testing"

	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

// batchStub is a vendor batch provider that is also the provider it
// batches for, as an adapter is. It records what Submit is sent.
type batchStub struct {
	*stubProvider
	submitted [][]types.BatchRequest
}

func (b *batchStub) Submit(_ context.Context, reqs []types.BatchRequest, _ types.BatchSubmitOptions) (types.BatchHandle, error) {
	b.submitted = append(b.submitted, reqs)
	return types.BatchHandle{Provider: b.name, ID: "batch_1"}, nil
}
func (b *batchStub) Status(context.Context, types.BatchHandle) (types.BatchStatus, error) {
	return types.BatchStatus{State: types.BatchEnded}, nil
}
func (b *batchStub) Results(context.Context, types.BatchHandle) iter.Seq2[types.BatchResult, error] {
	return func(func(types.BatchResult, error) bool) {}
}
func (b *batchStub) Cancel(context.Context, types.BatchHandle) error { return nil }

func TestBatchConvertsEachRequestAtSubmit(t *testing.T) {
	inner := &batchStub{stubProvider: &stubProvider{name: "chat", offering: textOnly()}}
	c := &fake{action: types.ActExtract, media: types.ModalityDocument, text: "extracted text"}
	bp := must.Get(NewBatch(inner, Config{Policy: types.ConversionPolicy{Converters: []types.Converter{c}}}))
	d := per(types.ModalityDocument, types.ActExtract)
	reqs := []types.BatchRequest{
		{CustomID: "a", Messages: []types.Message{types.UserMsg(types.Text("summarize"), pdf([]byte("%PDF-1")))},
			Options: types.RequestOptions{Dials: types.Dials{Modality: &d}}},
		{CustomID: "b", Messages: []types.Message{types.UserMsg(types.Text("plain"))}},
	}
	if _, err := bp.Submit(context.Background(), reqs, types.BatchSubmitOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(inner.submitted) != 1 || len(inner.submitted[0]) != 2 {
		t.Fatalf("submitted = %+v", inner.submitted)
	}
	sent := inner.submitted[0][0]
	parts := sent.Messages[0].(types.UserMessage).Parts
	if len(parts) != 2 || parts[1].(types.TextPart).Text != "extracted text" {
		t.Fatalf("request a sent %#v, want the extracted text", parts)
	}
	if sent.Options.Dials.Modality != nil {
		t.Error("the modality dial reached the batch provider")
	}
	if _, ok := reqs[0].Messages[0].(types.UserMessage).Parts[1].(types.DocumentPart); !ok {
		t.Error("the caller's request was modified")
	}
	if got := inner.submitted[0][1].Messages[0].(types.UserMessage).Parts[0].(types.TextPart).Text; got != "plain" {
		t.Errorf("request b sent %q", got)
	}
}

func TestBatchRejectsBeforeUpload(t *testing.T) {
	inner := &batchStub{stubProvider: &stubProvider{name: "chat", offering: textOnly()}}
	c := &fake{action: types.ActExtract, media: types.ModalityDocument, text: "x"}
	bp := must.Get(NewBatch(inner, Config{Policy: types.ConversionPolicy{Converters: []types.Converter{c}}}))
	reqs := []types.BatchRequest{
		{CustomID: "ok", Messages: []types.Message{types.UserMsg(types.Text("plain"))}},
		{CustomID: "img", Messages: []types.Message{types.UserMsg(pngPart("x"))}},
	}
	_, err := bp.Submit(context.Background(), reqs, types.BatchSubmitOptions{})
	if !errors.Is(err, types.ErrModalityUnsupported) || !errors.Is(err, types.ErrInvalidModelConfig) {
		t.Fatalf("err = %v, want a rejection matching ErrInvalidModelConfig", err)
	}
	if len(inner.submitted) != 0 {
		t.Error("a rejected batch was uploaded")
	}
	if c.calls.Load() != 0 {
		t.Error("a conversion ran for a batch that was rejected")
	}
}

func TestBatchRuntimePolicyAndForwarding(t *testing.T) {
	inner := &batchStub{stubProvider: &stubProvider{name: "chat", offering: textOnly()}}
	c := &fake{action: types.ActExtract, media: types.ModalityDocument, text: "from runtime"}
	bp := must.Get(NewBatch(inner, Config{Policy: types.ConversionPolicy{}}))
	if n, ok := bp.(interface{ Name() string }); !ok || n.Name() != "chat" {
		t.Fatal("the decorator does not report the inner name")
	}
	if _, ok := bp.(types.BatchFinder); ok {
		t.Error("the decorator claims a lookup the inner provider lacks")
	}
	ctx := WithRuntime(context.Background(), Runtime{Policy: types.ConversionPolicy{Converters: []types.Converter{c},
		Dial: per(types.ModalityDocument, types.ActExtract)}})
	reqs := []types.BatchRequest{{CustomID: "a", Messages: []types.Message{types.UserMsg(pdf([]byte("%PDF-1")))}}}
	if _, err := bp.Submit(ctx, reqs, types.BatchSubmitOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := inner.submitted[0][0].Messages[0].(types.UserMessage).Parts[0].(types.TextPart).Text; got != "from runtime" {
		t.Fatalf("sent %q, want the runtime policy's conversion", got)
	}
}
