// Package resolvetest provides deterministic stand-ins for the backends
// pkg/attachment/resolve only defines interfaces for: a Transcriber, a
// FrameSampler and a Prober. They do no real media work. Each derives its
// output from the digest of its input, so the same recording always yields
// the same transcript and frames, and each counts its calls, so a test can
// prove that a derivation ran once and was then read back from the store.
package resolvetest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"sync/atomic"
	"time"

	"github.com/urmzd/saige/flow/attachment/resolve"
)

// Transcriber is a fake resolve.Transcriber.
type Transcriber struct {
	// ID and Rev are reported as Name and Version. Empty means "fake", "1".
	ID, Rev string
	// Duration is reported for every recording. Zero means one minute.
	Duration time.Duration
	// Lines is how many transcript lines to produce. Zero means three.
	Lines int
	// Err, when set, is returned instead of a transcript.
	Err error
	// Drift, when set, makes every call return different text, modelling a
	// real backend that is not deterministic.
	Drift bool

	calls atomic.Int64
}

// Calls reports how many times Transcribe ran.
func (t *Transcriber) Calls() int { return int(t.calls.Load()) }

// Name implements resolve.Transcriber.
func (t *Transcriber) Name() string { return or(t.ID, "fake") }

// Version implements resolve.Transcriber.
func (t *Transcriber) Version() string { return or(t.Rev, "1") }

// Transcribe implements resolve.Transcriber.
func (t *Transcriber) Transcribe(_ context.Context, m resolve.Media, _ time.Duration) (resolve.Transcript, error) {
	n := t.calls.Add(1)
	if t.Err != nil {
		return resolve.Transcript{}, t.Err
	}
	lines := t.Lines
	if lines == 0 {
		lines = 3
	}
	tag := Tag(m.Data)
	var b strings.Builder
	for i := 1; i <= lines; i++ {
		fmt.Fprintf(&b, "line %d of the transcript of %s", i, tag)
		if t.Drift {
			fmt.Fprintf(&b, " (take %d)", n)
		}
		b.WriteByte('\n')
	}
	d := t.Duration
	if d == 0 {
		d = time.Minute
	}
	return resolve.Transcript{Text: b.String(), Duration: d, Language: "en"}, nil
}

// Sampler is a fake resolve.FrameSampler. It returns small solid-colour PNGs,
// one every ten seconds, coloured from the input's digest.
type Sampler struct {
	ID, Rev string
	// Frames is how many it would produce if not capped. Zero means four.
	Frames int
	Err    error

	calls atomic.Int64
}

// Calls reports how many times Sample ran.
func (s *Sampler) Calls() int { return int(s.calls.Load()) }

// Name implements resolve.FrameSampler.
func (s *Sampler) Name() string { return or(s.ID, "fake-frames") }

// Version implements resolve.FrameSampler.
func (s *Sampler) Version() string { return or(s.Rev, "1") }

// Sample implements resolve.FrameSampler.
func (s *Sampler) Sample(_ context.Context, m resolve.Media, max int, _ time.Duration) ([]resolve.Frame, error) {
	s.calls.Add(1)
	if s.Err != nil {
		return nil, s.Err
	}
	n := s.Frames
	if n == 0 {
		n = 4
	}
	n = min(n, max)
	sum := sha256.Sum256(m.Data)
	frames := make([]resolve.Frame, 0, n)
	for i := 0; i < n; i++ {
		frames = append(frames, resolve.Frame{
			At:   time.Duration(i) * 10 * time.Second,
			Data: PNG(4, 4, color.RGBA{R: sum[0], G: sum[1], B: byte(i), A: 0xff}),
		})
	}
	return frames, nil
}

// Prober is a fake resolve.Prober that reports a fixed duration.
type Prober struct {
	Duration time.Duration
	Err      error
}

// Probe implements resolve.Prober.
func (p Prober) Probe(context.Context, resolve.Media) (resolve.MediaInfo, error) {
	return resolve.MediaInfo{Duration: p.Duration}, p.Err
}

// Tag is the short digest the fakes name an input by.
func Tag(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:4])
}

// PNG encodes a solid-colour image, for tests that need real image bytes.
func PNG(w, h int, c color.Color) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err) // encoding an in-memory RGBA image cannot fail
	}
	return buf.Bytes()
}

func or(s, def string) string {
	if s != "" {
		return s
	}
	return def
}
