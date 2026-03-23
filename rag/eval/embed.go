package eval

import (
	"bytes"
	_ "embed"
	"text/template"
)

//go:embed prompts/faithfulness_decompose.prompt
var faithfulnessDecomposeRaw string

//go:embed prompts/faithfulness_verify.prompt
var faithfulnessVerifyRaw string

//go:embed prompts/answer_relevancy.prompt
var answerRelevancyRaw string

//go:embed prompts/answer_correctness.prompt
var answerCorrectnessRaw string

//go:embed prompts/llm_judge.prompt
var llmJudgeRaw string

var (
	faithfulnessDecomposeTmpl = template.Must(template.New("faithfulness_decompose").Parse(faithfulnessDecomposeRaw))
	faithfulnessVerifyTmpl    = template.Must(template.New("faithfulness_verify").Parse(faithfulnessVerifyRaw))
	answerRelevancyTmpl       = template.Must(template.New("answer_relevancy").Parse(answerRelevancyRaw))
	answerCorrectnessTmpl     = template.Must(template.New("answer_correctness").Parse(answerCorrectnessRaw))
	llmJudgeTmpl              = template.Must(template.New("llm_judge").Parse(llmJudgeRaw))
)

func renderPrompt(tmpl *template.Template, data any) string {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		panic("render prompt: " + err.Error())
	}
	return buf.String()
}
