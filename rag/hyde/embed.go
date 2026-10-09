package hyde

import (
	"bytes"
	_ "embed"
	"fmt"
	"text/template"
)

// DefaultPromptTemplate is the default prompt for generating hypothetical documents.
//
//go:embed prompts/default.prompt
var DefaultPromptTemplate string

var defaultPromptTmpl = template.Must(template.New("default").Parse(DefaultPromptTemplate))

func renderPrompt(tmpl *template.Template, data any) (string, error) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("hyde: render prompt: %w", err)
	}
	return buf.String(), nil
}
