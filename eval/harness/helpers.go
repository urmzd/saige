package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var thinkRE = regexp.MustCompile(`(?s)<think>.*?</think>`)

// Formats referenced from multiple places in this package.
const (
	formatMarkdown = "text/markdown"
	formatHTML     = "text/html"
)

// CleanArtifact strips <think> blocks and a single wrapping code fence from
// model output, returning the trimmed artifact text.
func CleanArtifact(text string) string {
	s := strings.TrimSpace(thinkRE.ReplaceAllString(text, ""))
	if strings.HasPrefix(s, "```") {
		if newline := strings.IndexByte(s, '\n'); newline >= 0 {
			s = s[newline+1:]
		}
	}
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "```") {
		s = strings.TrimSpace(strings.TrimSuffix(s, "```"))
	}
	return s
}

// ExtractJSONObject returns the substring from the first '{' to the last
// '}' of text, or text unchanged when no object braces are found.
func ExtractJSONObject(text string) string {
	start := strings.IndexByte(text, '{')
	end := strings.LastIndexByte(text, '}')
	if start >= 0 && end >= start {
		return text[start : end+1]
	}
	return text
}

// FormatExt maps a MIME-style format to a file extension for flow outputs.
// Unknown formats map to ".txt".
func FormatExt(format string) string {
	switch format {
	case formatHTML:
		return ".html"
	case "text/x-python":
		return ".py"
	case "application/javascript":
		return ".js"
	case "text/typescript":
		return ".ts"
	case "application/json":
		return ".json"
	case "text/x-yaml":
		return ".yaml"
	case "text/x-toml":
		return ".toml"
	case "text/x-rust":
		return ".rs"
	case "text/x-go":
		return ".go"
	case "text/css":
		return ".css"
	case "text/x-shellscript":
		return ".sh"
	case formatMarkdown:
		return ".md"
	case "image/svg+xml":
		return ".svg"
	case "application/xml":
		return ".xml"
	case "text/x-java":
		return ".java"
	case "text/x-ruby":
		return ".rb"
	case "application/sql":
		return ".sql"
	default:
		return ".txt"
	}
}

// WriteText writes value to path, creating parent directories (0750) and
// the file (0600) as needed.
func WriteText(path string, value string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(value), 0o600)
}

// WriteJSON writes value to path as indented JSON with a trailing newline.
func WriteJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return WriteText(path, string(data)+"\n")
}

// Pct returns the percentage saved going from base to next, rounded to one
// decimal place. A zero base yields 0.
func Pct(base uint64, next uint64) float64 {
	if base == 0 {
		return 0
	}
	return Round1((1 - float64(next)/float64(base)) * 100)
}

// Round1 rounds v to one decimal place.
func Round1(v float64) float64 {
	if v >= 0 {
		return float64(int(v*10+0.5)) / 10
	}
	return float64(int(v*10-0.5)) / 10
}

// Truncate trims whitespace and cuts s to at most maxLen bytes.
func Truncate(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}

// Rate returns the fraction of turns matching pred, 0 for an empty slice.
func Rate(turns []TurnResult, pred func(TurnResult) bool) float64 {
	if len(turns) == 0 {
		return 0
	}
	var count int
	for _, turn := range turns {
		if pred(turn) {
			count++
		}
	}
	return float64(count) / float64(len(turns))
}
