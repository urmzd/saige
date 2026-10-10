package openai

import (
	"fmt"

	"github.com/openai/openai-go/v3"
	"github.com/urmzd/saige/agent/types"
)

// audioOutput is the spoken answer requested with WithAudioOutput.
type audioOutput struct {
	voice, format string
}

// audioFormats maps each Chat Completions audio output format to the media
// type of the bytes it produces. pcm16 is raw 16-bit little-endian mono PCM
// at 24 kHz.
var audioFormats = map[string]types.MediaType{
	"wav":   types.MediaWAV,
	"mp3":   types.MediaMP3,
	"aac":   "audio/aac",
	"flac":  "audio/flac",
	"opus":  "audio/opus",
	"pcm16": "audio/pcm",
}

// WithAudioOutput asks an audio-capable model on Chat Completions (such as
// gpt-audio) to answer with speech as well as text: it sends modalities
// ["text", "audio"] and audio {voice, format}. The answer streams as an
// audio_out part whose bytes arrive as PartDelta.Data and whose transcript
// arrives as PartDelta.Transcript; its server ID and expiry are kept, so a
// later turn references the audio instead of resending it. Formats are wav,
// mp3, aac, flac, opus and pcm16. The Responses API has no audio output and
// rejects the option.
func WithAudioOutput(voice, format string) Option {
	return func(c *config) { c.params.audio = &audioOutput{voice: voice, format: format} }
}

// WithReasoningSummary asks the Responses API for summaries of the model's
// reasoning: "auto", "concise" or "detailed". Summaries stream as thinking
// parts marked Summary. Chat Completions returns no reasoning and rejects
// the option.
func WithReasoningSummary(mode string) Option {
	return func(c *config) { c.params.reasoningSummary = &mode }
}

// checkAudioOutput validates WithAudioOutput.
func (a *Adapter) checkAudioOutput() error {
	o := a.params.audio
	if o == nil {
		return nil
	}
	caps := a.Capabilities()
	if o.voice == "" {
		return caps.OptionError("audio_output", "a voice is required")
	}
	if _, ok := audioFormats[o.format]; !ok {
		return caps.OptionError("audio_output", fmt.Sprintf("format %q: use wav, mp3, aac, flac, opus or pcm16", o.format))
	}
	return nil
}

// applyAudioOutput encodes WithAudioOutput. The caller has checked it.
func (a *Adapter) applyAudioOutput(p *openai.ChatCompletionNewParams) {
	o := a.params.audio
	if o == nil {
		return
	}
	p.Modalities = []string{"text", "audio"}
	p.Audio = openai.ChatCompletionAudioParam{
		Voice:  openai.ChatCompletionAudioParamVoiceUnion{OfString: openai.String(o.voice)},
		Format: openai.ChatCompletionAudioParamFormat(o.format),
	}
}

// audioOutMeta is the media type and metadata of the requested audio.
func (a *Adapter) audioOutMeta() (types.MediaType, types.AudioMeta) {
	if a.params.audio == nil {
		return "", types.AudioMeta{}
	}
	f := a.params.audio.format
	meta := types.AudioMeta{Format: f}
	if f == "pcm16" {
		meta.SampleRate, meta.Channels = 24000, 1
	}
	return audioFormats[f], meta
}
