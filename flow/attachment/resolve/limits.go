package resolve

import "time"

// Limits are the budgets a Resolver enforces. They are per variant because
// the variants cost different things: a native image is paid for in request
// bytes and vision tokens, a transcript in text tokens, a transcription in a
// backend's time and money. A zero field means its default.
//
// Limits bound what is sent and what is attempted. They never change what a
// derivation stores: a transcript is stored whole and TranscriptBytes only
// bounds how much of it is shown.
type Limits struct {
	// Files is how many attachments a run reads. Further ones become notes.
	Files int
	// SourceBytes is the largest file that is loaded at all, for any
	// variant. A larger one becomes a note without being read.
	SourceBytes int64

	// NativeImageBytes and ImageSide bound an image sent natively: its
	// encoded size, and its longer side in pixels where the format can be
	// measured without a decoder dependency (PNG, JPEG, GIF).
	NativeImageBytes int64
	ImageSide        int
	// NativeAudioBytes and NativeAudioDuration bound audio sent natively.
	// The duration cap is enforced only when a Prober is configured, since
	// measuring a recording needs a container parser; otherwise the size cap
	// is the bound.
	NativeAudioBytes    int64
	NativeAudioDuration time.Duration
	// NativeVideoBytes and NativeVideoDuration bound video sent natively.
	NativeVideoBytes    int64
	NativeVideoDuration time.Duration
	// NativeDocumentBytes bounds a document, such as a PDF, sent natively.
	NativeDocumentBytes int64

	// TextBytes is how much of one file's text is shown inline, for a text
	// file and for text extracted from a document. Longer text is cut at a
	// line or character boundary and marked.
	TextBytes int
	// TranscribeBytes and TranscribeDuration bound a recording handed to a
	// Transcriber or FrameSampler. The duration is passed to the backend so
	// it can refuse early, and checked against what it reports.
	TranscribeBytes    int64
	TranscribeDuration time.Duration
	// TranscriptBytes is how much of a transcript is shown. The rule for a
	// long one: keep the beginning, cut at a line boundary, and say how much
	// was left out. The whole transcript stays in the store for retrieval.
	TranscriptBytes int
	// Keyframes is how many sampled frames of a video are requested and
	// shown; KeyframeBytes bounds each one.
	Keyframes     int
	KeyframeBytes int64

	// RunNativeBytes bounds the media bytes one run sends across all files.
	RunNativeBytes int64
	// RunTextBytes bounds the attachment text one run sends across all
	// files, notes and headers included.
	RunTextBytes int64
}

// DefaultLimits returns the limits a Resolver uses when given none. They are
// chosen to sit inside what current hosted models accept in one request, and
// to keep a run's attachment text to tens of thousands of tokens.
func DefaultLimits() Limits {
	return Limits{
		Files:               16,
		SourceBytes:         25 << 20,
		NativeImageBytes:    4 << 20,
		ImageSide:           8000,
		NativeAudioBytes:    20 << 20,
		NativeAudioDuration: 30 * time.Minute,
		NativeVideoBytes:    20 << 20,
		NativeVideoDuration: 10 * time.Minute,
		NativeDocumentBytes: 20 << 20,
		TextBytes:           100_000,
		TranscribeBytes:     25 << 20,
		TranscribeDuration:  2 * time.Hour,
		TranscriptBytes:     60_000,
		Keyframes:           8,
		KeyframeBytes:       1 << 20,
		RunNativeBytes:      24 << 20,
		RunTextBytes:        400_000,
	}
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	pick := func(v, def int64) int64 {
		if v > 0 {
			return v
		}
		return def
	}
	return Limits{
		Files:               int(pick(int64(l.Files), int64(d.Files))),
		SourceBytes:         pick(l.SourceBytes, d.SourceBytes),
		NativeImageBytes:    pick(l.NativeImageBytes, d.NativeImageBytes),
		ImageSide:           int(pick(int64(l.ImageSide), int64(d.ImageSide))),
		NativeAudioBytes:    pick(l.NativeAudioBytes, d.NativeAudioBytes),
		NativeAudioDuration: time.Duration(pick(int64(l.NativeAudioDuration), int64(d.NativeAudioDuration))),
		NativeVideoBytes:    pick(l.NativeVideoBytes, d.NativeVideoBytes),
		NativeVideoDuration: time.Duration(pick(int64(l.NativeVideoDuration), int64(d.NativeVideoDuration))),
		NativeDocumentBytes: pick(l.NativeDocumentBytes, d.NativeDocumentBytes),
		TextBytes:           int(pick(int64(l.TextBytes), int64(d.TextBytes))),
		TranscribeBytes:     pick(l.TranscribeBytes, d.TranscribeBytes),
		TranscribeDuration:  time.Duration(pick(int64(l.TranscribeDuration), int64(d.TranscribeDuration))),
		TranscriptBytes:     int(pick(int64(l.TranscriptBytes), int64(d.TranscriptBytes))),
		Keyframes:           int(pick(int64(l.Keyframes), int64(d.Keyframes))),
		KeyframeBytes:       pick(l.KeyframeBytes, d.KeyframeBytes),
		RunNativeBytes:      pick(l.RunNativeBytes, d.RunNativeBytes),
		RunTextBytes:        pick(l.RunTextBytes, d.RunTextBytes),
	}
}
