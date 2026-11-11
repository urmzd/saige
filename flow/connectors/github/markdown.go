package github

import "strings"

// markdown is GitHub Flavored Markdown, the surface's native alphabet — and the
// one the model is already writing in, which is why the answer itself passes
// through untouched and only what this package composes is normalized here.
type markdown struct{}

func (markdown) Bold(s string) string   { return "**" + s + "**" }
func (markdown) Italic(s string) string { return "_" + s + "_" }

// Code renders an inline span. A backtick inside would close the span early and
// leave the rest of the line as markup, so it is replaced: the alternative — a
// longer fence — has to be computed from the content and still fails on the
// pathological case.
func (markdown) Code(s string) string {
	if strings.Contains(s, "`") {
		s = strings.ReplaceAll(s, "`", "'")
	}
	return "`" + s + "`"
}

// Link renders [text](uri). Brackets in the label would close it early.
func (markdown) Link(text, uri string) string {
	if text == "" {
		text = uri
	}
	text = strings.NewReplacer("[", "(", "]", ")").Replace(text)
	return "[" + text + "](" + uri + ")"
}

func (markdown) Item(s string) string { return "- " + s }
