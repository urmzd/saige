package runtime_test

import "github.com/urmzd/saige/flow/event"

// lifecycle reads any run lifecycle body: each type's fields are a subset.
type lifecycle struct {
	Attempt   int              `json:"attempt,omitempty"`
	Error     string           `json:"error,omitempty"`
	Terminal  bool             `json:"terminal,omitempty"`
	Reason    string           `json:"reason,omitempty"`
	By        string           `json:"by,omitempty"`
	Text      string           `json:"text,omitempty"`
	Usage     *event.RunUsage  `json:"usage,omitempty"`
	Question  *event.Question  `json:"question,omitempty"`
	Questions []event.Question `json:"questions,omitempty"`
	Answer    *event.Answer    `json:"answer,omitempty"`
	Answers   []event.Answer   `json:"answers,omitempty"`
}
