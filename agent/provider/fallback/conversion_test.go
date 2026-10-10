package fallback

import (
	"context"
	"errors"
	"testing"

	"github.com/urmzd/saige/agent/convert"
	"github.com/urmzd/saige/agent/types"
)

// offered reports an offering and answers with its name.
type offered struct {
	name     string
	offering types.Offering
	calls    int
}

func (o *offered) Name() string                          { return o.name }
func (o *offered) Offering() types.Offering              { return o.offering }
func (o *offered) Capabilities() types.ModelCapabilities { return o.offering.Capabilities() }
func (o *offered) Stream(context.Context, types.Request) (<-chan types.Delta, error) {
	o.calls++
	ch := make(chan types.Delta, 1)
	ch <- types.PartDelta{Index: 0, Text: o.name}
	close(ch)
	return ch, nil
}

// A member whose conversion plan rejects the request never sent it, so the
// chain moves on even though the error is a configuration error.
func TestFallbackSkipsAMemberThatRejectsTheParts(t *testing.T) {
	text := &offered{name: "text", offering: types.Offering{ID: "text", Model: types.ModelInfo{Vendor: "a", Known: true}}}
	vision := &offered{name: "vision", offering: types.Offering{ID: "vision", Model: types.ModelInfo{Vendor: "b", Known: true},
		Modalities: types.Modalities{In: map[types.Modality]types.ModalityLimit{
			types.ModalityImage: {Media: []types.MediaType{types.MediaPNG}}}}}}
	f := New(convert.New(text, types.ConversionPolicy{}), convert.New(vision, types.ConversionPolicy{}))
	img := types.Image(types.Bytes(types.MediaPNG, []byte("png")))
	ch, err := f.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(img)}})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	if text.calls != 0 || vision.calls != 1 {
		t.Fatalf("calls text %d, vision %d", text.calls, vision.calls)
	}

	f = New(convert.New(text, types.ConversionPolicy{}))
	if _, err := f.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(img)}}); !errors.Is(err, types.ErrModalityUnsupported) {
		t.Fatalf("err = %v, want the rejection", err)
	}
}
