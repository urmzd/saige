package privacy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"strings"
	"unicode/utf8"

	"github.com/urmzd/saige/agent/provider/wrapper"
	"github.com/urmzd/saige/agent/types"
)

// MediaPolicy is what Provider does with media it cannot tokenize: images,
// audio, video, PDFs and other binary documents. Text-bearing media (a
// document or file of text, CSV or JSON with its bytes inline) is always
// tokenized, so it is not governed by the policy.
type MediaPolicy string

const (
	// MediaDefault is MediaRefuse when the vault is Sensitive and
	// MediaPass otherwise.
	MediaDefault MediaPolicy = ""
	// MediaPass sends opaque media as it is.
	MediaPass MediaPolicy = "pass"
	// MediaRefuse does not send a request that carries opaque media. A
	// request the decorator cannot tokenize is not sent.
	MediaRefuse MediaPolicy = "refuse"
	// MediaRequireText sends opaque media only as text a conversion made
	// from it: a transcript, a description, an extracted text, or the
	// notice of an omitted part. The conversion decorators below Provider
	// refuse a view that still carries media, and a converter that would
	// send the media to an endpoint not cleared for personal data
	// (DataHandling.PIIOK). The converter's text is tokenized before it is
	// sent.
	MediaRequireText MediaPolicy = "require_text"
)

// ErrMediaRefused reports a request that was not sent because it carries
// media the media policy refuses.
var ErrMediaRefused = errors.New("privacy: media refused by policy")

// ErrAudioOutRefused reports a response that was stopped because the model
// produced audio under a Sensitive vault: the audio speaks the placeholders,
// and only its transcript can be restored.
var ErrAudioOutRefused = errors.New("privacy: audio output refused under a sensitive vault")

// Sensitivity is implemented by a vault that can be configured Sensitive.
// A Sensitive vault makes MediaRefuse the default media policy of Provider
// and refuses audio output unless Provider.AllowAudioOut is set.
type Sensitivity interface {
	Sensitive() bool
}

func sensitive(v Vault) bool {
	s, ok := v.(Sensitivity)
	return ok && s.Sensitive()
}

// textBearing reports whether media of type mt is text the detectors can
// read: text/*, CSV and JSON.
func textBearing(mt types.MediaType) bool {
	base, _, err := mime.ParseMediaType(string(mt))
	if err != nil {
		base = strings.ToLower(strings.TrimSpace(string(mt)))
	}
	switch {
	case strings.HasPrefix(base, "text/"):
		return true
	case base == "application/json", strings.HasSuffix(base, "+json"),
		base == "application/csv", base == "application/x-ndjson":
		return true
	}
	return false
}

// tokenizable reports whether p is text-bearing media whose bytes are in
// the request, so its text can be tokenized.
func tokenizable(p types.Part) bool {
	var src types.Source
	switch v := p.(type) {
	case types.DocumentPart:
		src = v.Source
	case types.FilePart:
		src = v.Source
	default:
		return false
	}
	return len(src.Inline) > 0 && textBearing(src.MediaType) && utf8.Valid(src.Inline)
}

// opaque reports whether p is media the vault cannot tokenize.
func opaque(p types.Part) bool { return types.IsMedia(p) && !tokenizable(p) }

// tokenizeSource tokenizes the text of a text-bearing source. A source in
// which nothing was found is returned unchanged. Otherwise the result holds
// only the tokenized bytes, with their own digest: the other locators (a
// URI, a workspace reference, vendor uploads) reach the original text, so
// they are dropped from the copy that is sent.
func tokenizeSource(ctx context.Context, v Vault, src types.Source) (types.Source, error) {
	text := string(src.Inline)
	tok, err := v.Tokenize(ctx, text)
	if err != nil || tok == text {
		return src, err
	}
	mt := src.MediaType
	if base, _, _ := mime.ParseMediaType(string(mt)); (base == "application/json" || strings.HasSuffix(base, "+json")) && !json.Valid([]byte(tok)) {
		// A span crossed JSON syntax: send the tokenized text as text
		// rather than invalid JSON or the original values.
		mt = types.MediaType("text/plain; charset=utf-8")
	}
	out := types.Bytes(mt, []byte(tok))
	out.Filename = src.Filename
	return out, nil
}

// tokenizeMedia tokenizes a text-bearing document or file part and returns
// any other part unchanged.
func tokenizeMedia(ctx context.Context, v Vault, p types.Part) (types.Part, error) {
	if !tokenizable(p) {
		return p, nil
	}
	switch x := p.(type) {
	case types.DocumentPart:
		src, err := tokenizeSource(ctx, v, x.Source)
		x.Source = src
		return x, err
	case types.FilePart:
		src, err := tokenizeSource(ctx, v, x.Source)
		x.Source = src
		return x, err
	}
	return p, nil
}

// refuseMedia returns an error naming the first opaque media part in msgs,
// at the top level or inside a tool result.
func refuseMedia(msgs []types.Message, why string) error {
	for mi, m := range msgs {
		for pi, part := range types.PartsOf(m) {
			if opaque(part) {
				return mediaRefused(fmt.Sprintf("%d.%d", mi, pi), part, why)
			}
			if tr, ok := part.(types.ToolResultPart); ok {
				for ni, np := range tr.Parts {
					if opaque(np) {
						return mediaRefused(fmt.Sprintf("%d.%d.%d", mi, pi, ni), np, why)
					}
				}
			}
		}
	}
	return nil
}

func mediaRefused(path string, p types.Part, why string) error {
	src, _ := types.SourceOf(p)
	return fmt.Errorf("%w: part %s (%s %s): %s", ErrMediaRefused, path, p.Kind(), src.MediaType, why)
}

// converts reports whether every path from p to an adapter passes through
// a conversion decorator (types.ConversionPlanner), which applies the
// require_text boundary to each attempt.
func converts(p types.Provider, depth int) bool {
	if p == nil || depth > 64 {
		return false
	}
	if _, ok := p.(types.ConversionPlanner); ok {
		return true
	}
	members := wrapper.Members(p)
	if len(members) == 0 {
		return false
	}
	for _, m := range members {
		if !converts(m, depth+1) {
			return false
		}
	}
	return true
}
