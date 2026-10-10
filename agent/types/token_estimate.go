package types

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"time"
)

// Rough constants for EstimateTokens. They are deliberately simple: the
// estimate only decides when to compact, and the provider's reported prompt
// tokens take over after the first turn.
const (
	estimateCharsPerToken   = 4
	estimateMessageOverhead = 4    // role and framing tokens per message
	estimateFileTokens      = 1000 // an attached file or rich block of unknown size
)

// EstimateTokens approximates the input tokens of messages at four characters
// per token. It covers text, thinking, tool arguments and tool results, and
// charges a flat amount per media part. EstimateTokensFor prices media by an
// offering's token rules instead.
func EstimateTokens(messages []Message) int {
	return EstimateTokensFor(messages, nil)
}

// EstimateTokensFor approximates the input tokens of messages as
// EstimateTokens does, pricing each media part by the token rule the
// offering declares for its modality:
//
//   - an image costs the rule's per-image count, or its tiles, or its
//     pixels divided by PerPixels, with the pixels capped at the modality's
//     MaxPixels, since the vendor scales a larger image down;
//   - a document costs its pages times PerPage;
//   - audio and video cost their seconds (a video's clip, when set) times
//     PerSecond.
//
// Base is added to each. A part whose rule does not apply, or whose size is
// unknown, falls back to the flat per-media amount. Text-bearing media (a
// text, CSV or JSON document or file) counts its characters wherever its
// size is known. A nil offering prices every media part flat.
func EstimateTokensFor(messages []Message, o *Offering) int {
	e := estimator{offering: o}
	for _, m := range messages {
		e.fixed += estimateMessageOverhead
		for _, p := range PartsOf(m) {
			e.part(p)
		}
	}
	return e.total()
}

// EstimateMediaTokens returns the input tokens one media part costs on o,
// by the rules of EstimateTokensFor. It reports false for a part that is
// not media.
func EstimateMediaTokens(p Part, o *Offering) (int, bool) {
	if !IsMedia(p) {
		return 0, false
	}
	e := estimator{offering: o}
	e.part(p)
	return e.total(), true
}

type estimator struct {
	offering     *Offering
	chars, fixed int
}

func (e *estimator) total() int {
	return e.fixed + (e.chars+estimateCharsPerToken-1)/estimateCharsPerToken
}

func (e *estimator) part(p Part) {
	switch v := p.(type) {
	case TextPart:
		e.chars += len(v.Text)
	case JSONPart:
		e.chars += len(v.JSON)
	case ThinkingPart:
		e.chars += len(v.Text)
	case RefusalPart:
		e.chars += len(v.Text)
	case ToolCallPart:
		args, _ := json.Marshal(v.Arguments)
		e.chars += len(v.Name) + len(args)
	case ServerToolCallPart:
		in, _ := json.Marshal(v.Input)
		e.chars += len(v.Name) + len(in)
	case ServerToolResultPart:
		// A replayed result is sent as its native payload, which carries
		// the search results or program output the model read.
		if len(v.Result) > 0 {
			e.chars += len(v.Result)
		} else {
			e.chars += len(v.Text)
		}
	case ToolResultPart:
		for _, o := range v.Parts {
			e.part(o)
		}
	case AudioOutPart:
		if v.Transcript != "" && len(v.Source.Inline) == 0 {
			// Replayed by vendor ID or transcript, not as audio.
			e.chars += len(v.Transcript)
			return
		}
		e.media(v)
	default:
		if IsMedia(p) {
			e.media(p)
		}
	}
}

func (e *estimator) media(p Part) {
	src, _ := SourceOf(p)
	if textBearing(src.MediaType) {
		if n := sourceSize(src); n > 0 {
			e.chars += int(n)
			return
		}
	}
	m, _ := PartModality(p)
	var lim ModalityLimit
	if e.offering != nil {
		lim = e.offering.Modalities.In[m]
	}
	if n, ok := ruleTokens(p, lim); ok {
		e.fixed += n
		return
	}
	e.fixed += estimateFileTokens
}

// ruleTokens applies lim's token rule to p. It reports false when the rule
// declares nothing that applies to p, or p lacks the size the rule needs.
func ruleTokens(p Part, lim ModalityLimit) (int, bool) {
	r := lim.Tokens
	if r == (TokenRule{}) {
		return 0, false
	}
	switch v := p.(type) {
	case ImagePart:
		return imageTokens(r, lim, v.Width, v.Height)
	case ImageOutPart:
		return imageTokens(r, lim, v.Width, v.Height)
	case DocumentPart:
		if r.PerPage > 0 && v.Pages > 0 {
			return r.Base + v.Pages*r.PerPage, true
		}
	case AudioPart:
		return secondsTokens(r, v.Duration)
	case AudioOutPart:
		return secondsTokens(r, v.Duration)
	case VideoPart:
		d := v.Duration
		if v.ClipEnd > v.ClipStart {
			d = v.ClipEnd - v.ClipStart
		}
		return secondsTokens(r, d)
	case VideoOutPart:
		return secondsTokens(r, v.Duration)
	}
	return 0, false
}

func imageTokens(r TokenRule, lim ModalityLimit, w, h int) (int, bool) {
	if r.PerImage > 0 {
		return r.Base + r.PerImage, true
	}
	if w <= 0 || h <= 0 {
		return 0, false
	}
	if lim.MaxPixels > 0 && w*h > lim.MaxPixels {
		// The vendor scales the image down to fit, keeping its aspect ratio.
		f := math.Sqrt(float64(lim.MaxPixels) / float64(w*h))
		w, h = max(1, int(float64(w)*f)), max(1, int(float64(h)*f))
	}
	switch {
	case r.Tile > 0 && r.PerTile > 0:
		tiles := ((w + r.Tile - 1) / r.Tile) * ((h + r.Tile - 1) / r.Tile)
		return r.Base + tiles*r.PerTile, true
	case r.PerPixels > 0:
		return r.Base + (w*h+r.PerPixels-1)/r.PerPixels, true
	}
	return 0, false
}

func secondsTokens(r TokenRule, d time.Duration) (int, bool) {
	if r.PerSecond <= 0 || d <= 0 {
		return 0, false
	}
	return r.Base + int(math.Ceil(d.Seconds()))*r.PerSecond, true
}

// textBearing reports whether media of type mt is text a model reads as
// characters: text/*, JSON or CSV.
func textBearing(mt MediaType) bool {
	base, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(string(mt))), ";")
	base = strings.TrimSpace(base)
	return strings.HasPrefix(base, "text/") || base == string(MediaJSON) || base == "application/csv" ||
		strings.HasSuffix(base, "+json")
}

func sourceSize(s Source) int64 {
	if len(s.Inline) > 0 {
		return int64(len(s.Inline))
	}
	return s.Size
}

// EstimatingTokenizer is a Tokenizer backed by EstimateTokensFor. It needs
// no network call and is the default when no provider tokenizer is
// configured. Offering, when set, prices media by its token rules.
type EstimatingTokenizer struct {
	Offering *Offering
}

// CountTokens returns EstimateTokensFor(messages, t.Offering).
func (t EstimatingTokenizer) CountTokens(_ context.Context, messages []Message) (int, error) {
	return EstimateTokensFor(messages, t.Offering), nil
}
