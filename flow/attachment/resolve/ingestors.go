package resolve

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	_ "image/gif"  // measure GIF
	_ "image/jpeg" // measure JPEG
	_ "image/png"  // measure PNG
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	types "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/flow/attachment"
	"github.com/urmzd/saige/flow/blob"
	"github.com/urmzd/saige/rag/extractor"
)

// Media is a recording or document handed to a derivation backend.
type Media struct {
	Data      []byte
	MediaType string
	Name      string
	// Digest is the content's SHA-256, for a backend that keeps its own
	// cache or logs what it processed.
	Digest string
}

// Transcript is speech turned into text.
type Transcript struct {
	Text string
	// Duration is the recording's length. A backend should report it: it is
	// how the duration cap is checked.
	Duration time.Duration
	// Language is a BCP 47 tag, when detected.
	Language string
}

// Transcriber turns audio, or the audio track of a video, into text.
//
// flow ships no implementation: transcription needs a model or a
// service, and which one is a deployment's choice. Name and Version key the
// stored transcript, so changing the backend or its model must change one of
// them. An implementation should wrap ErrUnavailable around failures that a
// retry could fix, and should refuse a recording longer than maxDuration
// before paying to process it.
type Transcriber interface {
	Name() string
	Version() string
	Transcribe(ctx context.Context, m Media, maxDuration time.Duration) (Transcript, error)
}

// FrameSampler picks representative frames from a video, at most max of
// them, in time order. Like Transcriber it is an interface only; decoding
// video needs a codec this module does not depend on.
type FrameSampler interface {
	Name() string
	Version() string
	Sample(ctx context.Context, m Media, max int, maxDuration time.Duration) ([]Frame, error)
}

// MediaInfo is what a Prober can say about a recording without decoding it.
type MediaInfo struct {
	Duration time.Duration
}

// Prober measures a recording. It is optional, and exists so the native
// audio and video variants can enforce a duration cap, which needs a
// container parser.
type Prober interface {
	Probe(ctx context.Context, m Media) (MediaInfo, error)
}

// ── Native ───────────────────────────────────────────────────────────

// Native passes a file to the model as itself: image, audio, video or
// document content. It applies only when the request's model accepts the
// file's media type, and declines otherwise so a derived variant can be
// tried.
type Native struct {
	// Prober, when set, is used to enforce the duration caps on audio and
	// video. Without it those are bounded by size alone.
	Prober Prober
}

// Name implements Ingestor.
func (Native) Name() string { return "native" }

// Version implements Ingestor.
func (Native) Version() string { return "1" }

// Ingest implements Ingestor.
func (n Native) Ingest(ctx context.Context, in *Input) (Variant, error) {
	var capBytes int64
	var capDuration time.Duration
	switch in.Kind {
	case attachment.KindImage:
		capBytes = in.Limits.NativeImageBytes
	case attachment.KindAudio:
		capBytes, capDuration = in.Limits.NativeAudioBytes, in.Limits.NativeAudioDuration
	case attachment.KindVideo:
		capBytes, capDuration = in.Limits.NativeVideoBytes, in.Limits.NativeVideoDuration
	case attachment.KindDocument:
		capBytes = in.Limits.NativeDocumentBytes
	default:
		return Variant{}, Decline("%s files have no native form", in.Kind)
	}
	if !in.Model.Supports(types.MediaType(in.MediaType)) {
		return Variant{}, Decline("the model does not accept %s input", in.MediaType)
	}
	if int64(len(in.Data)) > capBytes {
		return Variant{}, Decline("it is %s, over the %s limit for %s input", size(int64(len(in.Data))), size(capBytes), in.Kind)
	}

	var part types.UserPart
	src := types.Bytes(types.MediaType(in.MediaType), in.Data)
	src.Filename = in.Attachment.Name
	switch in.Kind {
	case attachment.KindImage:
		meta, err := measure(in.MediaType, in.Data, in.Limits.ImageSide)
		if err != nil {
			return Variant{}, err
		}
		part = types.Image(src, meta)
	case attachment.KindAudio, attachment.KindVideo:
		if n.Prober != nil {
			info, err := n.Prober.Probe(ctx, in.Media())
			if err != nil {
				if errors.Is(err, ErrUnavailable) {
					return Variant{}, err
				}
				return Variant{}, Decline("its length could not be measured")
			}
			if info.Duration > capDuration {
				return Variant{}, Decline("it is %s long, over the %s limit for %s input", clock(info.Duration), clock(capDuration), in.Kind)
			}
		}
		part = types.Media(src)
	default:
		part = types.Document(src, types.DocumentMeta{Title: in.Attachment.Name})
	}
	return Variant{Form: FormNative, Parts: []types.UserPart{types.Text(nativeHeader(in.Attachment, in.MediaType)), part}}, nil
}

// measure reads an image's dimensions where the standard library can, and
// refuses one that does not decode or is larger than a model will take. An
// image that would be rejected by the provider fails the whole request, so
// it is better turned into a note here. Formats the standard library cannot
// measure, such as WebP, are passed on unmeasured.
func measure(mediaType string, data []byte, maxSide int) (types.ImageMeta, error) {
	switch mediaType {
	case "image/png", "image/jpeg", "image/gif":
	default:
		return types.ImageMeta{}, nil
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return types.ImageMeta{}, Decline("it is not a readable %s image", strings.TrimPrefix(mediaType, "image/"))
	}
	if cfg.Width > maxSide || cfg.Height > maxSide {
		return types.ImageMeta{}, Decline("it is %dx%d pixels, over the %d pixel limit", cfg.Width, cfg.Height, maxSide)
	}
	return types.ImageMeta{Width: cfg.Width, Height: cfg.Height}, nil
}

// ── Inline text ──────────────────────────────────────────────────────

// Inline places a text file's content in the conversation, under a header
// naming the file, truncated to Limits.TextBytes.
type Inline struct{}

// Name implements Ingestor.
func (Inline) Name() string { return "inline" }

// Version implements Ingestor.
func (Inline) Version() string { return "1" }

// Ingest implements Ingestor.
func (i Inline) Ingest(ctx context.Context, in *Input) (Variant, error) {
	text, changed, ok := decodeText(in.Data)
	if !ok {
		// Sniffing saw text in the first bytes; the rest disagreed.
		return Variant{}, Decline("it is not valid text")
	}
	digest := blob.Digest(in.Attachment.Digest)
	if changed {
		// The index should read UTF-8, so the decoded form is stored. It is
		// content-addressed, so storing it again is free.
		info, err := in.put(ctx, []byte(text))
		if err != nil {
			return Variant{}, err
		}
		digest = info.Digest
	}
	body, truncated := block(in.Attachment, in.MediaType, "", text, in.Limits.TextBytes)
	return Variant{
		Form:      FormText,
		Parts:     []types.UserPart{types.Text(body)},
		Truncated: truncated,
		Document: &Document{
			Source: in.Attachment, Ingestor: i.Name(), Version: i.Version(),
			Digest: digest, Size: int64(len(text)),
		},
	}, nil
}

// decodeText returns data as UTF-8 text. It strips a UTF-8 byte order mark,
// decodes UTF-16 that announces itself with one, and reports false for
// anything that is not text: invalid UTF-8, or a NUL byte.
func decodeText(data []byte) (text string, changed, ok bool) {
	switch {
	case bytes.HasPrefix(data, []byte{0xef, 0xbb, 0xbf}):
		data, changed = data[3:], true
	case bytes.HasPrefix(data, []byte{0xff, 0xfe}), bytes.HasPrefix(data, []byte{0xfe, 0xff}):
		var order binary.ByteOrder = binary.LittleEndian
		if data[0] == 0xfe {
			order = binary.BigEndian
		}
		body := data[2:]
		if len(body)%2 != 0 {
			return "", false, false
		}
		units := make([]uint16, len(body)/2)
		for i := range units {
			units[i] = order.Uint16(body[2*i:])
		}
		data, changed = []byte(string(utf16.Decode(units))), true
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return "", false, false
	}
	return string(data), changed, true
}

// ── Text extraction ──────────────────────────────────────────────────

// Extract derives text from a document with a saige Extractor, chosen by
// media type. The extraction is stored and reused.
type Extract struct {
	name, version string
	byType        map[string]types.Extractor
}

// NewExtract returns an extracting ingestor. name and version key what it
// stores: change the version when an extractor's output changes.
func NewExtract(name, version string, byType map[string]types.Extractor) Extract {
	return Extract{name: name, version: version, byType: byType}
}

// DefaultExtract extracts PDF text with saige's document extractor, the same
// one saige's RAG ingestion uses, one section per page.
func DefaultExtract() Extract {
	return NewExtract("saige-extract", "1", map[string]types.Extractor{
		attachment.TypePDF: extractor.NewAuto().PartExtractor(),
	})
}

// Name implements Ingestor.
func (e Extract) Name() string { return e.name }

// Version implements Ingestor.
func (e Extract) Version() string { return e.version }

// Ingest implements Ingestor.
func (e Extract) Ingest(ctx context.Context, in *Input) (Variant, error) {
	ex, ok := e.byType[in.MediaType]
	if !ok {
		return Variant{}, Decline("no text extractor is configured for %s", in.MediaType)
	}
	d, err := in.Derive(ctx, e.name, e.version, func(ctx context.Context) (Derived, error) {
		text, err := extractText(ctx, ex, in.Data, in.MediaType)
		if err != nil {
			return Derived{}, err
		}
		return Derived{Text: text}, nil
	})
	if err != nil {
		return Variant{}, err
	}
	body, truncated := block(in.Attachment, in.MediaType, fmt.Sprintf("extracted-by=%q", e.name+"@"+e.version), d.Text, in.Limits.TextBytes)
	return Variant{Form: FormDerived, Parts: []types.UserPart{types.Text(body)}, Truncated: truncated, Document: in.document(e, d)}, nil
}

// extractText runs an extractor on untrusted bytes. Document parsers panic on
// malformed input more often than they should, and one hostile PDF must not
// take the process down, so a panic is turned into a declined variant.
func extractText(ctx context.Context, ex types.Extractor, data []byte, mediaType string) (text string, err error) {
	defer func() {
		if r := recover(); r != nil {
			text, err = "", Decline("its text could not be extracted")
		}
	}()
	parts, err := ex.Extract(ctx, data, types.MediaType(mediaType))
	switch {
	case errors.Is(err, ErrUnavailable), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "", err
	case errors.Is(err, extractor.ErrNoText):
		return "", Decline("it contains no extractable text, which usually means it is a scan")
	case err != nil:
		return "", Decline("its text could not be extracted")
	}
	var texts []string
	for _, p := range parts {
		if t, ok := p.(types.TextPart); ok && strings.TrimSpace(t.Text) != "" {
			texts = append(texts, t.Text)
		}
	}
	if len(texts) == 0 {
		return "", Decline("it contains no extractable text, which usually means it is a scan")
	}
	return strings.Join(texts, "\n\n"), nil
}

// ── Transcription ────────────────────────────────────────────────────

// Transcribe is the derived-text variant for recordings: a transcript for
// audio, and a transcript plus sampled keyframes for video. The transcript
// and frames are stored and reused.
//
// Keyframes are shown only to a model that accepts their image type. To a
// text-only model a video is its transcript alone.
type Transcribe struct {
	t Transcriber
	s FrameSampler
}

// NewTranscribe returns a transcribing ingestor. s may be nil, in which case
// video yields a transcript only. t may be nil if s is not, in which case
// video yields keyframes only and audio is declined.
func NewTranscribe(t Transcriber, s FrameSampler) Transcribe { return Transcribe{t: t, s: s} }

// Name implements Ingestor.
func (Transcribe) Name() string { return "transcribe" }

// Version implements Ingestor. It is composed from the backends' names and
// versions, so swapping either one keys a new artifact.
func (x Transcribe) Version() string {
	v := "1"
	if x.t != nil {
		v += ";t=" + x.t.Name() + "@" + x.t.Version()
	}
	if x.s != nil {
		v += ";s=" + x.s.Name() + "@" + x.s.Version()
	}
	return v
}

// Ingest implements Ingestor.
func (x Transcribe) Ingest(ctx context.Context, in *Input) (Variant, error) {
	video := in.Kind == attachment.KindVideo
	switch {
	case in.Kind != attachment.KindAudio && !video:
		return Variant{}, Decline("%s files cannot be transcribed", in.Kind)
	case x.t == nil && (!video || x.s == nil):
		return Variant{}, Decline("no transcriber is configured")
	case int64(len(in.Data)) > in.Limits.TranscribeBytes:
		return Variant{}, Decline("it is %s, over the %s limit for transcription", size(int64(len(in.Data))), size(in.Limits.TranscribeBytes))
	}
	sample := video && x.s != nil
	// The keyframe count shapes what is stored, so it is part of the key.
	version := x.Version()
	if sample {
		version += fmt.Sprintf(";k=%d", in.Limits.Keyframes)
	}
	limit := in.Limits.TranscribeDuration

	d, err := in.Derive(ctx, x.Name(), version, func(ctx context.Context) (Derived, error) {
		var d Derived
		if x.t != nil {
			tr, err := x.t.Transcribe(ctx, in.Media(), limit)
			if err != nil {
				return Derived{}, fmt.Errorf("transcribe: %w", err)
			}
			if tr.Duration > limit {
				return Derived{}, Decline("it is %s long, over the %s limit for transcription", clock(tr.Duration), clock(limit))
			}
			d.Text, d.Duration, d.Language = strings.TrimSpace(tr.Text), tr.Duration, tr.Language
		}
		if sample {
			frames, err := x.s.Sample(ctx, in.Media(), in.Limits.Keyframes, limit)
			if err != nil {
				return Derived{}, fmt.Errorf("sample frames: %w", err)
			}
			for _, f := range frames {
				if len(d.Frames) < in.Limits.Keyframes && int64(len(f.Data)) <= in.Limits.KeyframeBytes {
					d.Frames = append(d.Frames, f)
				}
			}
		}
		return d, nil
	})
	if err != nil {
		return Variant{}, err
	}

	var frames []Frame
	for _, f := range d.Frames {
		if len(frames) < in.Limits.Keyframes && in.Model.Supports(types.MediaType(f.MediaType)) {
			frames = append(frames, f)
		}
	}
	if d.Text == "" && len(frames) == 0 {
		return Variant{}, Decline("no speech was found in it and no keyframes the model accepts were produced")
	}

	v := Variant{Form: FormDerived, Document: in.document(x, d)}
	extra := fmt.Sprintf("derived=%q", "transcript")
	if d.Duration > 0 {
		extra += fmt.Sprintf(" duration=%q", clock(d.Duration))
	}
	if len(frames) > 0 {
		extra += fmt.Sprintf(" keyframes=\"%d\"", len(frames))
	}
	text := d.Text
	if text == "" {
		text = "(no speech was transcribed)"
	}
	body, truncated := block(in.Attachment, in.MediaType, extra, text, in.Limits.TranscriptBytes)
	v.Parts, v.Truncated = append(v.Parts, types.Text(body)), truncated
	for _, f := range frames {
		src := types.Bytes(types.MediaType(f.MediaType), f.Data)
		v.Parts = append(v.Parts,
			types.Text(fmt.Sprintf("[Keyframe at %s of %q follows.]", clock(f.At), in.Attachment.Label())),
			types.Image(src))
	}
	return v, nil
}
