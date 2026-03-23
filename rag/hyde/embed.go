package hyde

import (
	"bytes"
	_ "embed"
	"text/template"
)

// DefaultPromptTemplate is the default prompt for generating hypothetical documents.
//
//go:embed prompts/default.prompt
var DefaultPromptTemplate string

var defaultPromptTmpl = template.Must(template.New("default").Parse(DefaultPromptTemplate))

func renderPrompt(tmpl *template.Template, data any) string {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		panic("render prompt: " + err.Error())
	}
	return buf.String()
}
