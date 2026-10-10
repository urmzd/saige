package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// mediaInfo is what the transcript shows of a media part. The terminal never
// draws the media itself: it shows a labelled placeholder with the type,
// size and name, and where the bytes are kept.
type mediaInfo struct {
	kind          types.PartKind
	mediaType     types.MediaType
	size          int64
	filename      string
	digest        string
	locator       string // a URI or saige-artifact:// ref
	unresolved    string
	transcript    string
	width, height int
	duration      time.Duration
	streaming     bool
}

// mediaOf describes a media part.
func mediaOf(p types.Part) mediaInfo {
	src, _ := types.SourceOf(p)
	m := mediaInfo{kind: p.Kind(), mediaType: src.MediaType, size: src.Size, filename: src.Filename,
		digest: src.Digest, locator: firstOf(src.URI, src.Ref), unresolved: src.Unresolved}
	if m.size == 0 {
		m.size = int64(len(src.Inline))
	}
	switch v := p.(type) {
	case types.ImagePart:
		m.width, m.height = v.Width, v.Height
	case types.ImageOutPart:
		m.width, m.height = v.Width, v.Height
	case types.VideoPart:
		m.width, m.height, m.duration = v.Width, v.Height, v.Duration
	case types.VideoOutPart:
		m.width, m.height, m.duration = v.Width, v.Height, v.Duration
	case types.AudioPart:
		m.duration = v.Duration
	case types.AudioOutPart:
		m.duration, m.transcript = v.Duration, v.Transcript
	}
	return m
}

// label renders the placeholder, such as
// "[image image/png 1024x768 · 120 KB · chart.png · saige-artifact://9f1c2a…]",
// or "[image/png streaming… 120 KB]" while the bytes arrive.
func (m mediaInfo) label() string {
	mt := string(m.mediaType)
	if mt == "" {
		mt = "unknown type"
	}
	if m.streaming {
		s := "[" + mt + " streaming…"
		if m.size > 0 {
			s += " " + humanBytes(m.size)
		}
		return s + "]"
	}
	head := strings.TrimSuffix(string(m.kind), "_out")
	if head == "" {
		head = "media"
	}
	head += " " + mt
	if m.width > 0 && m.height > 0 {
		head += fmt.Sprintf(" %dx%d", m.width, m.height)
	}
	fields := []string{head}
	if m.duration > 0 {
		fields = append(fields, m.duration.Round(100*time.Millisecond).String())
	}
	if m.size > 0 {
		fields = append(fields, humanBytes(m.size))
	}
	if m.filename != "" {
		fields = append(fields, m.filename)
	}
	switch {
	case m.unresolved != "":
		fields = append(fields, "not kept: "+m.unresolved)
	case m.locator != "":
		fields = append(fields, shortLocator(m.locator))
	case m.digest != "":
		fields = append(fields, "sha256:"+shortDigest(m.digest))
	}
	s := "[" + strings.Join(fields, " · ") + "]"
	if m.transcript != "" {
		s += " " + truncateRunes(strings.TrimSpace(m.transcript), 120)
	}
	return s
}

// shortLocator shortens an artifact ref's digest; other URIs are cut to a
// readable length.
func shortLocator(loc string) string {
	if d, ok := strings.CutPrefix(loc, types.ArtifactScheme); ok {
		return types.ArtifactScheme + shortDigest(d)
	}
	return truncateRunes(loc, 60)
}

func shortDigest(d string) string {
	if len(d) <= 12 {
		return d
	}
	return d[:12] + "…"
}

// humanBytes renders a byte count in B, KB, MB or GB (powers of 1024).
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	v, suffix := float64(n)/unit, "KB"
	for _, s := range []string{"MB", "GB", "TB"} {
		if v < unit {
			break
		}
		v, suffix = v/unit, s
	}
	if v >= 10 {
		return fmt.Sprintf("%.0f %s", v, suffix)
	}
	return fmt.Sprintf("%.1f %s", v, suffix)
}

func firstOf(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
