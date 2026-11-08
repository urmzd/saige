package attachment

import (
	"bytes"
	"net/http"
	"path"
	"strings"
)

// SniffLen is how many leading bytes Sniff looks at. Passing more is harmless.
const SniffLen = 512

// Kind is the routing class of a media type: which family of ingestors a
// file is offered to. It is coarser than a media type on purpose. A resolver
// registers handling per kind, and a new audio codec should not need a new
// registration.
type Kind string

// Kinds.
const (
	KindImage    Kind = "image"
	KindAudio    Kind = "audio"
	KindVideo    Kind = "video"
	KindDocument Kind = "document" // paged or structured documents: PDF, office formats
	KindText     Kind = "text"     // anything readable as text: prose, code, logs, JSON
	KindArchive  Kind = "archive"
	KindUnknown  Kind = "unknown"
)

// Kinds lists every kind, in a fixed order.
func Kinds() []Kind {
	return []Kind{KindImage, KindAudio, KindVideo, KindDocument, KindText, KindArchive, KindUnknown}
}

// Media types Sniff reports beyond the ones net/http knows.
const (
	TypeOctetStream = "application/octet-stream"
	TypePDF         = "application/pdf"
	TypeDOCX        = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	TypeXLSX        = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	TypePPTX        = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
)

// KindOf classifies a media type. An unrecognized type is KindUnknown, which
// routes to an explicit "could not be read" note rather than to a guess.
func KindOf(mediaType string) Kind {
	mt, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(mediaType)), ";")
	mt = strings.TrimSpace(mt)
	switch mt {
	case TypePDF, TypeDOCX, TypeXLSX, TypePPTX, "application/msword", "application/rtf", "text/rtf",
		"application/vnd.oasis.opendocument.text", "application/epub+zip":
		return KindDocument
	case "application/json", "application/xml", "application/yaml", "application/x-yaml", "application/toml",
		"application/javascript", "application/x-ndjson", "application/x-sh", "application/sql":
		return KindText
	case "application/zip", "application/gzip", "application/x-gzip", "application/x-tar", "application/x-7z-compressed",
		"application/x-rar-compressed", "application/vnd.rar", "application/x-bzip2", "application/x-xz", "application/zstd":
		return KindArchive
	case "application/ogg":
		return KindAudio
	}
	switch {
	case strings.HasPrefix(mt, "image/"):
		return KindImage
	case strings.HasPrefix(mt, "audio/"):
		return KindAudio
	case strings.HasPrefix(mt, "video/"):
		return KindVideo
	case strings.HasPrefix(mt, "text/"), strings.HasSuffix(mt, "+json"), strings.HasSuffix(mt, "+xml"):
		return KindText
	}
	return KindUnknown
}

// Sniff reports the media type of content from its leading bytes. The result
// is a bare lowercase type with no parameters.
//
// The name is consulted only to refine a type the bytes have already
// established: text that the extension says is Markdown or JSON, or a ZIP
// container that the extension says is an office document. Content that does
// not look like text is never called text because of its name, and nothing is
// called an image, audio or video unless its signature says so. Content that
// matches nothing is application/octet-stream.
//
// Known limits, stated rather than hidden. An MP4 container holding only
// audio is reported by its brand, so a voice clip saved as .mp4 with a
// generic brand is video/mp4 unless its name ends in .m4a; a transcriber
// handles both. A WebM is audio when the codecs named in its first bytes are
// all audio ones, which holds for recordings browsers make and is not a
// parse of the container. Text is recognized
// by the absence of binary bytes in the sniffed prefix, so a resolver must
// still check the whole content before treating it as text.
func Sniff(head []byte, name string) string {
	if len(head) > SniffLen {
		head = head[:SniffLen]
	}
	ext := strings.ToLower(path.Ext(name))
	if mt := sniffSignature(head); mt != "" {
		if refined, ok := audioOnly[mt][ext]; ok {
			return refined
		}
		return mt
	}
	mt, _, _ := strings.Cut(http.DetectContentType(head), ";")
	switch mt {
	case "text/plain":
		if refined, ok := textTypes[ext]; ok {
			return refined
		}
	case "application/zip":
		if refined, ok := zipTypes[ext]; ok {
			return refined
		}
	case "application/ogg":
		return "audio/ogg"
	case "application/x-gzip":
		return "application/gzip"
	case "audio/wave":
		return "audio/wav"
	case "video/avi":
		return "video/x-msvideo"
	}
	return mt
}

// textTypes refines content already proven to be text. The table is fixed
// rather than read from the host's MIME database, so the same file sniffs the
// same on every machine a run might replay on.
var textTypes = map[string]string{
	".md": "text/markdown", ".markdown": "text/markdown",
	".csv": "text/csv", ".tsv": "text/tab-separated-values",
	".json": "application/json", ".jsonl": "application/x-ndjson", ".ndjson": "application/x-ndjson",
	".yaml": "application/yaml", ".yml": "application/yaml", ".toml": "application/toml",
	".xml": "application/xml", ".html": "text/html", ".htm": "text/html",
	// SVG is XML text that a browser would execute. It is typed as XML, so
	// it routes as text: a model reads its source and nothing renders it.
	".svg": "application/xml",
}

// audioOnly refines a media container already proven by its signature when
// the extension names the audio-only form of that same container: a voice
// clip saved as .m4a with a generic MP4 brand, or a .weba. The container is
// the same either way, so the name decides only which of its two uses this
// file is, never what kind of bytes it holds.
var audioOnly = map[string]map[string]string{
	"video/mp4":  {".m4a": "audio/mp4"},
	"video/webm": {".weba": "audio/webm"},
}

// zipTypes refines content already proven to be a ZIP container.
var zipTypes = map[string]string{
	".docx": TypeDOCX, ".xlsx": TypeXLSX, ".pptx": TypePPTX,
	".odt": "application/vnd.oasis.opendocument.text", ".epub": "application/epub+zip",
}

// sniffSignature recognizes the audio, video and archive containers net/http
// does not, or does not report precisely. It returns "" when none match.
//
//nolint:gocyclo // one case per file signature
func sniffSignature(b []byte) string {
	has := func(off int, sig string) bool {
		return len(b) >= off+len(sig) && string(b[off:off+len(sig)]) == sig
	}
	switch {
	case has(4, "ftyp") && len(b) >= 12:
		return isoBrand(string(b[8:12]))
	case has(0, "\x1a\x45\xdf\xa3"):
		// EBML. The doctype string follows within the header.
		if bytes.Contains(b, []byte("webm")) {
			// A recording made by a browser, such as a voice clip, names
			// its codecs in the track entries near the front. Audio codecs
			// and no video codec there is an audio file in a WebM
			// container.
			if hasAny(b, "A_OPUS", "A_VORBIS") && !hasAny(b, "V_VP8", "V_VP9", "V_AV1") {
				return "audio/webm"
			}
			return "video/webm"
		}
		return "video/x-matroska"
	case has(0, "RIFF") && has(8, "WAVE"):
		return "audio/wav"
	case has(0, "RIFF") && has(8, "AVI "):
		return "video/x-msvideo"
	case has(0, "FORM") && (has(8, "AIFF") || has(8, "AIFC")):
		return "audio/aiff"
	case has(0, "fLaC"):
		return "audio/flac"
	case has(0, "OggS"):
		if bytes.Contains(b, []byte("theora")) || bytes.Contains(b, []byte("\x01video")) {
			return "video/ogg"
		}
		return "audio/ogg"
	case has(0, "ID3"):
		return "audio/mpeg"
	case has(0, "#!AMR"):
		return "audio/amr"
	case has(0, "FLV\x01"):
		return "video/x-flv"
	case has(0, "\x30\x26\xb2\x75\x8e\x66\xcf\x11"):
		return "video/x-ms-asf"
	case has(0, "\x00\x00\x01\xba"), has(0, "\x00\x00\x01\xb3"):
		return "video/mpeg"
	case len(b) > 188 && b[0] == 0x47 && b[188] == 0x47:
		return "video/mp2t"
	case has(257, "ustar"):
		return "application/x-tar"
	case has(0, "7z\xbc\xaf\x27\x1c"):
		return "application/x-7z-compressed"
	case has(0, "\xfd7zXZ\x00"):
		return "application/x-xz"
	case has(0, "\x28\xb5\x2f\xfd"):
		return "application/zstd"
	case has(0, "BZh") && len(b) > 3 && b[3] >= '1' && b[3] <= '9':
		return "application/x-bzip2"
	}
	// 0xFF 0xFE is also the UTF-16 little-endian byte order mark. Text in that
	// encoding has a zero high byte two bytes later, which an MPEG frame
	// header's fourth byte almost never is, so that is read as text.
	utf16 := len(b) >= 4 && b[0] == 0xff && b[1] == 0xfe && b[3] == 0
	if len(b) >= 4 && b[0] == 0xff && b[1]&0xe0 == 0xe0 && !utf16 {
		// An MPEG audio frame sync with no ID3 tag in front of it.
		version, layer := b[1]>>3&3, b[1]>>1&3
		bitrate, rate := b[2]>>4, b[2]>>2&3
		switch {
		case layer == 0 && b[1]&0xf0 == 0xf0 && rate != 3:
			return "audio/aac" // ADTS
		case version != 1 && layer != 0 && bitrate != 0 && bitrate != 15 && rate != 3:
			return "audio/mpeg"
		}
	}
	return ""
}

// isoBrand maps an ISO base media major brand to a media type.
func isoBrand(brand string) string {
	switch {
	case brand == "M4A " || brand == "M4B " || brand == "M4P ":
		return "audio/mp4"
	case brand == "qt  ":
		return "video/quicktime"
	case brand == "heic" || brand == "heix" || brand == "mif1" || brand == "msf1":
		return "image/heic"
	case brand == "avif" || brand == "avis":
		return "image/avif"
	case strings.HasPrefix(brand, "3gp") || strings.HasPrefix(brand, "3g2"):
		return "video/3gpp"
	}
	return "video/mp4"
}

func hasAny(b []byte, subs ...string) bool {
	for _, s := range subs {
		if bytes.Contains(b, []byte(s)) {
			return true
		}
	}
	return false
}
