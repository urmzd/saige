package eval

import (
	"bytes"
	_ "embed"
	"fmt"
	"text/template"
)

//go:embed prompts/judge.prompt
var judgePromptRaw string

//go:embed prompts/judge_pairwise.prompt
var judgePairwisePromptRaw string

var (
	judgeTmpl         = template.Must(template.New("judge").Option("missingkey=error").Parse(judgePromptRaw))
	judgePairwiseTmpl = template.Must(template.New("judge_pairwise").Option("missingkey=error").Parse(judgePairwisePromptRaw))
)

// renderPrompt executes a judge template. A missing key or execution error
// is returned rather than producing an empty or partial prompt.
func renderPrompt(tmpl *template.Template, data any) (string, error) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render %s prompt: %w", tmpl.Name(), err)
	}
	return buf.String(), nil
}
