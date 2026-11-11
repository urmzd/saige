// Package resolve turns attachment references into model input.
//
// A tag carries attachments as small references (see pkg/attachment). When a
// turn is about to run, something has to decide what each file becomes in the
// conversation: an image the model looks at, text it reads, a transcript of a
// recording, or a line saying a file was there and could not be read. That
// decision is this package. It is the only part of the attachment path that
// knows about saige, and it emits saige's own part types so the agent loop
// accepts the result unchanged.
//
// # Routing is a registry, not a switch
//
// Each file is sniffed and classified into a kind (image, audio, video,
// document, text, archive, unknown). A Resolver holds an ordered list of
// Ingestors per kind. An Ingestor looks at one file and at what the target
// model accepts, and either produces a Variant of model input or declines.
// The first variant that fits the budget is used. A deployment changes
// behaviour by changing the registry: prefer a transcript over native audio
// by listing the transcribing ingestor first, add OCR for scanned documents
// by registering one more.
//
// # Variants, and why model capability is an input
//
// The same file has several possible forms:
//
//	native   the bytes, as image, audio, video or document content
//	text     the file's own text, inline
//	derived  text computed from the file: extracted from a PDF, transcribed
//	         from audio, a transcript plus sampled keyframes for video
//	note     one line saying the file was attached and could not be read
//
// Which is right depends on the model. Sending audio to a model that cannot
// hear fails the whole request; sending a transcript to one that can hear
// throws information away. So a Request carries the model's native media
// support and nothing is assumed: the zero value means a text-only model, and
// every file then arrives as text or as a note.
//
// The note is the floor, and it is always reached rather than skipped. A file
// that nothing could read still produces a part naming it and saying why. An
// agent that silently loses an attachment answers a different question from
// the one it was asked, and the person who attached the file cannot tell.
//
// # Derived artifacts are computed once
//
// Transcribing or extracting is slow, costs money and is not always
// deterministic. Its output is stored back in the blob store and recorded
// under a reference keyed by (source digest, ingestor name, ingestor
// version). The first result recorded wins and every later resolution reads
// it. That is a correctness property, not only a cache: a durable run is
// replayed from the top, and a transcript that came out slightly differently
// on a second attempt would change the prompt, diverge the replay and miss
// the provider's prompt cache. Budgets that shape presentation (how much of a
// transcript is shown) are applied when rendering, never when deriving, so
// the stored artifact is the whole text and is also what a retrieval index
// should read: see Result.Documents.
//
// # Where attachments go in the conversation
//
// Prompt caching rewards a stable prefix: system prompt, tool definitions,
// then earlier turns, each identical to the request before. Attachments are
// the most volatile content a turn has, so the rule is:
//
//   - Never put attachment content in the system prompt or ahead of earlier
//     turns. One image there invalidates the cache for everything after it,
//     on every request of every run.
//   - Attach a file to the message it arrived with, after that message's own
//     text. Result.Message does exactly this, and Result.Append adds that
//     message at the end of a history.
//   - Keep it byte-stable. Resolution is a pure function of the references,
//     the stored bytes, the limits and the model's capabilities, and files
//     are rendered in list order, so the message built for attempt two, or
//     for the fifth tool-loop iteration of the same turn, is identical to the
//     first and the cache covers it.
package resolve

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	types "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/flow/attachment"
	"github.com/urmzd/saige/flow/blob"
)

// Sentinel errors.
var (
	// ErrNotApplicable is what an Ingestor returns, wrapped with a reason,
	// when it has no variant for a file: the model does not accept the
	// modality, the file is over a cap, the content could not be decoded.
	// The resolver moves on to the next ingestor and keeps the reason for the
	// note. Use Decline to build one.
	ErrNotApplicable = errors.New("resolve: not applicable")
	// ErrUnavailable is what an Ingestor wraps when it failed for a reason
	// that retrying could fix, such as a transcription backend being down.
	// Resolve returns it instead of writing a note, so the durable run
	// retries the turn rather than answering without the file.
	ErrUnavailable = errors.New("resolve: ingestor unavailable")
)

// Decline returns an ErrNotApplicable carrying a reason. The reason is shown
// to the model in the fallback note, so it should be a plain clause that
// reads after "could not be read:".
func Decline(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrNotApplicable, fmt.Sprintf(format, args...))
}

// Form is the shape a file took in the conversation.
type Form string

// Forms.
const (
	FormNative  Form = "native"
	FormText    Form = "text"
	FormDerived Form = "derived"
	FormNote    Form = "note"
)

// Ingestor produces one variant of model input for a file.
//
// Name and Version identify the ingestor in outcomes and, for an ingestor
// that derives, key what it stored. Bump Version whenever the same input
// would now produce different output; that is what retires old artifacts.
type Ingestor interface {
	Name() string
	Version() string
	// Ingest returns a variant for in, or an error wrapping ErrNotApplicable
	// to pass, or one wrapping ErrUnavailable to have the turn retried.
	Ingest(ctx context.Context, in *Input) (Variant, error)
}

// Variant is one form of a file as model input.
type Variant struct {
	Form Form
	// Parts is the model input, in order.
	Parts []types.UserPart
	// Document, when set, names the file's full text in the blob store for a
	// retrieval index. It is the whole text even when Parts shows a
	// truncated view of it.
	Document *Document
	// Truncated reports that Parts shows less than the whole content.
	Truncated bool
}

// Document is text a file yielded, addressed in the blob store: the file's
// own bytes for a UTF-8 text file, or a derived artifact for an extraction or
// a transcript. It is the form a retrieval index consumes. Nothing here
// depends on an index: a caller reads the text with ReadDocument and feeds
// whatever it likes.
type Document struct {
	// Source is the attachment the text came from.
	Source attachment.Attachment `json:"source"`
	// Ingestor and Version say what produced the text. Together with the
	// source digest they are a stable identity for an index entry.
	Ingestor string `json:"ingestor"`
	Version  string `json:"version"`
	// Digest is the UTF-8 text blob, in the same tenant's store.
	Digest blob.Digest `json:"digest"`
	// Size is the text's length in bytes.
	Size int64 `json:"size"`
	// Duration is the media length for a transcript, when known.
	Duration time.Duration `json:"duration,omitempty"`
}

// ReadDocument loads a document's text.
func ReadDocument(ctx context.Context, store blob.Store, tenant string, doc Document) (string, error) {
	b, err := readBlob(ctx, store, tenant, doc.Digest, doc.Size)
	if err != nil {
		return "", fmt.Errorf("resolve: read document for %q: %w", doc.Source.Label(), err)
	}
	return string(b), nil
}

// Request is one resolution: the files that arrived with one message, for one
// tenant, aimed at one model.
type Request struct {
	// Tenant scopes every blob read and write. It comes from the run.
	Tenant string
	// Attachments are resolved and rendered in this order.
	Attachments []attachment.Attachment
	// Model is what the target model reads natively. The zero value is a
	// text-only model.
	Model types.ContentSupport
	// Budget carries the run's remaining allowance across calls. Nil means a
	// fresh budget, which is right when a run resolves once.
	Budget *Budget
}

// Outcome records what happened to one attachment.
type Outcome struct {
	Attachment attachment.Attachment `json:"attachment"`
	// MediaType and Kind are what the stored bytes were found to be, which
	// is what routing used. They can differ from the reference's claim.
	MediaType string          `json:"media_type"`
	Kind      attachment.Kind `json:"kind"`
	Form      Form            `json:"form"`
	// Ingestor is "name@version" of the ingestor whose variant was used.
	// Empty for a note.
	Ingestor string `json:"ingestor,omitempty"`
	// Reason says why the file became a note, or is empty.
	Reason    string `json:"reason,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	// NativeBytes and TextBytes are what the variant spent from the budget.
	NativeBytes int64 `json:"native_bytes,omitempty"`
	TextBytes   int64 `json:"text_bytes,omitempty"`
}

// Result is the model input for a request's attachments.
type Result struct {
	// Parts is every file's model input, in attachment order. It never has
	// fewer files than the request: one that could not be read is a note.
	Parts []types.UserPart
	// Outcomes has one entry per attachment, in order.
	Outcomes []Outcome
	// Documents lists the full text of every file that yielded text.
	Documents []Document
}

// Message builds the user message these attachments arrived with: the
// message's own text first, then the files. See the package comment for why
// the order is this way round.
func (r Result) Message(text string) types.UserMessage {
	parts := make([]types.UserPart, 0, len(r.Parts)+1)
	if text != "" {
		parts = append(parts, types.Text(text))
	}
	return types.UserMessage{Parts: append(parts, r.Parts...)}
}

// Append returns history with the attachments' message added at the end. The
// history is not modified and nothing is inserted ahead of it, which is the
// property prompt caching depends on.
func (r Result) Append(history []types.Message, text string) []types.Message {
	out := make([]types.Message, 0, len(history)+1)
	out = append(out, history...)
	return append(out, r.Message(text))
}

// Budget is a run's remaining attachment allowance. A run that resolves
// attachments more than once (a resumed run, a later message) shares one
// Budget across the calls so the caps are per run, as they are documented.
// It is safe for concurrent use.
type Budget struct {
	mu     sync.Mutex
	files  int
	native int64
	text   int64
}

// NewBudget returns an unspent budget.
func NewBudget() *Budget { return &Budget{} }

func (b *Budget) takeFile(limit int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.files >= limit {
		return false
	}
	b.files++
	return true
}

func (b *Budget) spend(native, text int64, l Limits) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.native+native > l.RunNativeBytes {
		return Decline("it would exceed this run's %s budget for media", size(l.RunNativeBytes))
	}
	if b.text+text > l.RunTextBytes {
		return Decline("it would exceed this run's %s budget for attached text", size(l.RunTextBytes))
	}
	b.native += native
	b.text += text
	return nil
}

// Resolver resolves attachments against one blob store. It is safe for
// concurrent use if its ingestors are.
type Resolver struct {
	store    blob.Store
	limits   Limits
	registry map[attachment.Kind][]Ingestor
}

// Option configures a Resolver.
type Option func(*Resolver)

// WithLimits overrides limits. Zero fields keep their defaults.
func WithLimits(l Limits) Option {
	return func(r *Resolver) { r.limits = l.withDefaults() }
}

// WithIngestors replaces the ingestors for a kind, in preference order. An
// empty list makes every file of that kind a note.
func WithIngestors(kind attachment.Kind, ingestors ...Ingestor) Option {
	return func(r *Resolver) { r.registry[kind] = append([]Ingestor(nil), ingestors...) }
}

// WithFallback appends ingestors for a kind, after the ones already there.
func WithFallback(kind attachment.Kind, ingestors ...Ingestor) Option {
	return func(r *Resolver) { r.registry[kind] = append(r.registry[kind], ingestors...) }
}

// WithTranscription adds the derived-text variant for audio and video, after
// the native one: a transcript, plus sampled keyframes for video when a
// sampler is given. Either argument may be nil, but not both. Without this
// option a recording the model cannot take natively becomes a note.
func WithTranscription(t Transcriber, s FrameSampler) Option {
	return func(r *Resolver) {
		if t == nil && s == nil {
			return
		}
		if t != nil {
			r.registry[attachment.KindAudio] = append(r.registry[attachment.KindAudio], NewTranscribe(t, nil))
		}
		r.registry[attachment.KindVideo] = append(r.registry[attachment.KindVideo], NewTranscribe(t, s))
	}
}

// New returns a Resolver with the default registry:
//
//	image     native
//	audio     native
//	video     native
//	document  native, then text extraction (PDF, through saige)
//	text      inline
//	archive   (nothing: a note)
//	unknown   (nothing: a note)
//
// Native variants apply only to media types the request's model accepts, so
// with a text-only model this registry yields extracted text, inline text and
// notes.
func New(store blob.Store, opts ...Option) *Resolver {
	r := &Resolver{
		store:  store,
		limits: Limits{}.withDefaults(),
		registry: map[attachment.Kind][]Ingestor{
			attachment.KindImage:    {Native{}},
			attachment.KindAudio:    {Native{}},
			attachment.KindVideo:    {Native{}},
			attachment.KindDocument: {Native{}, DefaultExtract()},
			attachment.KindText:     {Inline{}},
		},
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Ingestors returns the registered ingestors for a kind, in order.
func (r *Resolver) Ingestors(kind attachment.Kind) []Ingestor {
	return append([]Ingestor(nil), r.registry[kind]...)
}

// Resolve turns the request's attachments into model input.
//
// It returns an error only when no honest result exists: the request is
// malformed, the context ended, the blob store failed in a way that is not
// "that blob is gone", or an ingestor reported ErrUnavailable. Everything
// else, including a missing blob, a corrupt file and an unsupported format,
// is a note in the result, because those do not get better on retry and the
// run should proceed knowing what it could not see.
func (r *Resolver) Resolve(ctx context.Context, req Request) (Result, error) {
	if r.store == nil {
		return Result{}, errors.New("resolve: no blob store")
	}
	if req.Tenant == "" {
		return Result{}, errors.New("resolve: no tenant")
	}
	budget := req.Budget
	if budget == nil {
		budget = NewBudget()
	}
	var res Result
	for _, a := range req.Attachments {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		out, parts, doc, err := r.one(ctx, req, budget, a)
		if err != nil {
			return Result{}, fmt.Errorf("resolve: attachment %q: %w", a.Label(), err)
		}
		res.Parts = append(res.Parts, parts...)
		res.Outcomes = append(res.Outcomes, out)
		if doc != nil {
			res.Documents = append(res.Documents, *doc)
		}
	}
	return res, nil
}

// one resolves a single attachment. A returned error aborts the request; a
// file that merely could not be read comes back as a note with a nil error.
func (r *Resolver) one(ctx context.Context, req Request, budget *Budget, a attachment.Attachment) (Outcome, []types.UserPart, *Document, error) {
	out := Outcome{Attachment: a, MediaType: a.MediaType, Kind: a.Kind(), Form: FormNote}
	note := func(reason string) (Outcome, []types.UserPart, *Document, error) {
		out.Reason = reason
		return out, []types.UserPart{types.Text(noteText(a, out.MediaType, reason))}, nil, nil
	}

	if err := a.Validate(); err != nil {
		// The reference itself is malformed. Its fields are not safe to
		// print, so the note describes it without them.
		out.Attachment = attachment.Attachment{}
		out.Reason = "its reference is malformed"
		return out, []types.UserPart{types.Text("[A file was attached but could not be read: its reference is malformed.]")}, nil, nil
	}
	if a.Unread != "" {
		// The file never reached the store: the surface it was attached on
		// refused it, or it was over a cap at intake. There is nothing to
		// read and nothing to spend, only the fact to report.
		return note(a.Unread)
	}
	if !budget.takeFile(r.limits.Files) {
		return note(fmt.Sprintf("this run already has %d attachments, which is the limit", r.limits.Files))
	}

	digest := blob.Digest(a.Digest)
	info, err := r.store.Stat(ctx, req.Tenant, digest)
	switch {
	case errors.Is(err, blob.ErrNotFound):
		return note("its content is no longer stored")
	case errors.Is(err, blob.ErrCorrupt):
		return note("its stored content is damaged")
	case err != nil:
		return out, nil, nil, err
	}
	// The stored size is the truth; the reference's size is a claim.
	a.Size = info.Size
	out.Attachment = a
	if info.Size > r.limits.SourceBytes {
		return note(fmt.Sprintf("it is %s, over the %s limit for a single file", size(info.Size), size(r.limits.SourceBytes)))
	}
	data, err := readBlob(ctx, r.store, req.Tenant, digest, info.Size)
	switch {
	case errors.Is(err, blob.ErrNotFound):
		return note("its content is no longer stored")
	case errors.Is(err, blob.ErrCorrupt):
		return note("its stored content is damaged")
	case err != nil:
		return out, nil, nil, err
	}

	// Route on what the bytes are, not on what the reference says they are.
	// A reference can arrive on the wire, and a media type is exactly the
	// field an attacker would set to steer bytes into the wrong decoder.
	out.MediaType = attachment.Sniff(data, a.Name)
	out.Kind = attachment.KindOf(out.MediaType)

	in := &Input{
		Attachment: a, MediaType: out.MediaType, Kind: out.Kind,
		Model: req.Model, Limits: r.limits, Data: data,
		store: r.store, tenant: req.Tenant,
	}
	var reasons []string
	for _, ing := range r.registry[out.Kind] {
		v, err := ing.Ingest(ctx, in)
		if err == nil {
			native, text := cost(v.Parts)
			err = budget.spend(native, text, r.limits)
			if err == nil {
				out.Form, out.Ingestor = v.Form, ing.Name()+"@"+ing.Version()
				out.Truncated, out.NativeBytes, out.TextBytes = v.Truncated, native, text
				return out, v.Parts, v.Document, nil
			}
		}
		switch {
		case ctx.Err() != nil:
			return out, nil, nil, ctx.Err()
		case errors.Is(err, ErrUnavailable):
			return out, nil, nil, fmt.Errorf("ingestor %s: %w", ing.Name(), err)
		case errors.Is(err, ErrNotApplicable):
			reasons = append(reasons, reasonOf(err))
		case errors.Is(err, blob.ErrNotFound), errors.Is(err, blob.ErrCorrupt), errors.Is(err, blob.ErrTooLarge), errors.Is(err, blob.ErrInvalid):
			reasons = append(reasons, "a stored artifact for it is missing or damaged")
		default:
			// An ingestor's own failure on this file. It is not known to be
			// transient, so it is a reason, not a retry.
			reasons = append(reasons, fmt.Sprintf("%s failed", ing.Name()))
		}
	}
	if len(reasons) == 0 {
		return note(fmt.Sprintf("files of type %s are not supported", out.MediaType))
	}
	return note(joinReasons(reasons))
}

// cost is what a variant spends: media bytes and text bytes.
func cost(parts []types.UserPart) (native, text int64) {
	for _, p := range parts {
		if t, ok := p.(types.TextPart); ok {
			text += int64(len(t.Text))
			continue
		}
		if src, ok := types.SourceOf(p); ok {
			native += max(src.Size, int64(len(src.Inline)))
		}
	}
	return native, text
}

// readBlob loads a whole blob, verifying it against its digest whatever the
// backend does, and refusing one that is not the size it was reported to be.
func readBlob(ctx context.Context, store blob.Store, tenant string, d blob.Digest, want int64) ([]byte, error) {
	rc, _, err := store.Get(ctx, tenant, d)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, io.LimitReader(rc, want+1)); err != nil {
		return nil, err
	}
	if int64(buf.Len()) != want || blob.Sum(buf.Bytes()) != d {
		return nil, fmt.Errorf("%w: %s does not match its digest", blob.ErrCorrupt, d)
	}
	return buf.Bytes(), nil
}
