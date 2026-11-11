package resolve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	types "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/flow/attachment"
	"github.com/urmzd/saige/flow/blob"
)

// Input is one file as an Ingestor sees it.
type Input struct {
	// Attachment is the reference, with Size corrected to the stored size.
	Attachment attachment.Attachment
	// MediaType and Kind are sniffed from the stored bytes. Route on these,
	// not on Attachment.MediaType.
	MediaType string
	Kind      attachment.Kind
	// Model is what the target model reads natively.
	Model types.ContentSupport
	// Limits are the resolver's budgets, defaults filled in.
	Limits Limits
	// Data is the file's content, verified against its digest. Ingestors
	// must not modify it.
	Data []byte

	store  blob.Store
	tenant string
}

// Media is the file as handed to a derivation backend.
func (in *Input) Media() Media {
	return Media{Data: in.Data, MediaType: in.MediaType, Name: in.Attachment.Name, Digest: in.Attachment.Digest}
}

// Derived is what a derivation produced from a file: text, and for video,
// frames. A compute function fills Text, Frames, Duration and Language;
// Derive fills the digests.
type Derived struct {
	// Text is the full derived text: a transcript, an extraction.
	Text string
	// TextDigest is where Text is stored. Zero when there is no text.
	TextDigest blob.Digest
	// Duration is the media's length, when the backend reported it.
	Duration time.Duration
	// Language is a BCP 47 tag for a transcript, when known.
	Language string
	// Frames are sampled keyframes, in time order.
	Frames []Frame
}

// Frame is one sampled video frame.
type Frame struct {
	// At is the frame's offset into the recording.
	At time.Duration
	// MediaType is sniffed from Data by Derive; a sampler need not set it.
	MediaType string
	// Data is the encoded image.
	Data []byte
	// Digest is where Data is stored.
	Digest blob.Digest
}

// manifest is the stored record of a derivation: pointers to its parts. It
// is a blob like any other, and the reference for (source, ingestor, version)
// points at it.
type manifest struct {
	V        int             `json:"v"`
	Text     *manifestBlob   `json:"text,omitempty"`
	Duration time.Duration   `json:"duration_ns,omitempty"`
	Language string          `json:"language,omitempty"`
	Frames   []manifestFrame `json:"frames,omitempty"`
}

type manifestBlob struct {
	Digest blob.Digest `json:"digest"`
	Size   int64       `json:"size"`
}

type manifestFrame struct {
	At        time.Duration `json:"at_ns"`
	MediaType string        `json:"media_type"`
	manifestBlob
}

// refKey names a derivation. The ingestor's name and version are hashed
// together so that neither can be spelled to collide with another's key and
// the key's length does not depend on them.
func refKey(source, name, version string) string {
	sum := sha256.Sum256([]byte(name + "\x00" + version))
	return "attachment/derived/v1/" + source + "/" + hex.EncodeToString(sum[:])
}

// Derive returns the artifact that the ingestor (name, version) derives from
// this file, computing it only if no usable one is stored.
//
// The result is stored in the tenant's blob store and recorded under a
// reference keyed by (source digest, name, version). The first recorded
// result wins: if two resolutions race, both return the same artifact, and
// every later one reads it. That is what makes a derived variant stable
// across the replays of a durable run even when compute is not deterministic.
//
// An error from compute is returned unchanged and nothing is recorded, so a
// failure is retried on the next resolution rather than remembered.
func (in *Input) Derive(ctx context.Context, name, version string, compute func(ctx context.Context) (Derived, error)) (Derived, error) {
	key := refKey(in.Attachment.Digest, name, version)
	if at, err := in.store.Ref(ctx, in.tenant, key); err == nil {
		if d, err := in.load(ctx, at); err == nil {
			return d, nil
		} else if !gone(err) {
			return Derived{}, err
		}
		// The record or one of its parts is gone. Derive again; PutRef
		// replaces a reference whose blob no longer exists.
	} else if !errors.Is(err, blob.ErrNotFound) {
		return Derived{}, fmt.Errorf("resolve: look up derived artifact: %w", err)
	}

	d, err := compute(ctx)
	if err != nil {
		return Derived{}, err
	}
	d.Text = strings.ToValidUTF8(d.Text, "�")

	m := manifest{V: 1, Duration: d.Duration, Language: d.Language}
	if d.Text != "" {
		info, err := in.put(ctx, []byte(d.Text))
		if err != nil {
			return Derived{}, err
		}
		m.Text, d.TextDigest = &manifestBlob{Digest: info.Digest, Size: info.Size}, info.Digest
	}
	frames := d.Frames[:0:0]
	for _, f := range d.Frames {
		// A frame is an image because its bytes say so, whatever the
		// sampler labelled it.
		f.MediaType = attachment.Sniff(f.Data, "")
		if attachment.KindOf(f.MediaType) != attachment.KindImage {
			continue
		}
		info, err := in.put(ctx, f.Data)
		if err != nil {
			return Derived{}, err
		}
		f.Digest = info.Digest
		frames = append(frames, f)
		m.Frames = append(m.Frames, manifestFrame{At: f.At, MediaType: f.MediaType, manifestBlob: manifestBlob{Digest: info.Digest, Size: info.Size}})
	}
	d.Frames = frames

	raw, err := json.Marshal(m)
	if err != nil {
		return Derived{}, fmt.Errorf("resolve: encode derived artifact: %w", err)
	}
	info, err := in.put(ctx, raw)
	if err != nil {
		return Derived{}, err
	}
	winner, err := in.store.PutRef(ctx, in.tenant, key, info.Digest)
	if err != nil {
		return Derived{}, fmt.Errorf("resolve: record derived artifact: %w", err)
	}
	if winner != info.Digest {
		// Another resolution recorded first. Use its artifact so everyone
		// agrees; ours stays in the store unreferenced.
		if won, err := in.load(ctx, winner); err == nil {
			return won, nil
		} else if !gone(err) {
			return Derived{}, err
		}
	}
	return d, nil
}

// put stores a derived blob. Its size was already bounded by what produced
// it, so the store's cap is set to exactly what is being written.
func (in *Input) put(ctx context.Context, b []byte) (blob.Info, error) {
	info, err := in.store.Put(ctx, in.tenant, strings.NewReader(string(b)), blob.PutOptions{MaxBytes: int64(len(b)) + 1})
	if err != nil {
		return blob.Info{}, fmt.Errorf("resolve: store derived artifact: %w", err)
	}
	return info, nil
}

// load reads a recorded derivation back.
func (in *Input) load(ctx context.Context, at blob.Digest) (Derived, error) {
	info, err := in.store.Stat(ctx, in.tenant, at)
	if err != nil {
		return Derived{}, err
	}
	raw, err := readBlob(ctx, in.store, in.tenant, at, info.Size)
	if err != nil {
		return Derived{}, err
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil || m.V != 1 {
		return Derived{}, fmt.Errorf("%w: derived artifact record %s is unreadable", blob.ErrCorrupt, at)
	}
	d := Derived{Duration: m.Duration, Language: m.Language}
	if m.Text != nil {
		b, err := readBlob(ctx, in.store, in.tenant, m.Text.Digest, m.Text.Size)
		if err != nil {
			return Derived{}, err
		}
		d.Text, d.TextDigest = string(b), m.Text.Digest
	}
	for _, f := range m.Frames {
		b, err := readBlob(ctx, in.store, in.tenant, f.Digest, f.Size)
		if err != nil {
			return Derived{}, err
		}
		d.Frames = append(d.Frames, Frame{At: f.At, MediaType: f.MediaType, Data: b, Digest: f.Digest})
	}
	return d, nil
}

// gone reports an error that means a stored artifact is unusable and should
// be derived again, as opposed to the store itself failing.
func gone(err error) bool {
	return errors.Is(err, blob.ErrNotFound) || errors.Is(err, blob.ErrCorrupt) || errors.Is(err, blob.ErrInvalid)
}

// document describes derived text for an index.
func (in *Input) document(ing Ingestor, d Derived) *Document {
	if d.TextDigest == "" {
		return nil
	}
	return &Document{
		Source: in.Attachment, Ingestor: ing.Name(), Version: ing.Version(),
		Digest: d.TextDigest, Size: int64(len(d.Text)), Duration: d.Duration,
	}
}
