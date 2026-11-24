package slack

import "strings"

// mrkdwn is Slack's markup: not Markdown, despite the resemblance. Bold is a
// single asterisk, italic a single underscore, and a link is an angle-bracketed
// pair rather than the bracket-paren form — a body written in Markdown renders in
// Slack as a body full of punctuation.
type mrkdwn struct{}

func (mrkdwn) Bold(s string) string   { return "*" + s + "*" }
func (mrkdwn) Italic(s string) string { return "_" + s + "_" }
func (mrkdwn) Code(s string) string   { return "`" + s + "`" }

// Link renders <uri|text>, and falls back to the bare uri when there is nothing
// to label it with.
//
// The uri is encoded for the link construct: "<", ">" and "|" are
// percent-encoded, since any of them would end the construct or its uri
// early, and "&" is written as "&amp;", as Slack reads entities in message
// text, links included.
func (mrkdwn) Link(text, uri string) string {
	target := linkURI.Replace(uri)
	if text == "" || text == uri {
		return "<" + target + ">"
	}
	// A "|" inside the label would end it early and leave the rest as text.
	return "<" + target + "|" + strings.ReplaceAll(text, "|", "-") + ">"
}

// linkURI encodes the characters of a uri that Slack's link construct reads
// as delimiters or entities.
var linkURI = strings.NewReplacer("&", "&amp;", "<", "%3C", ">", "%3E", "|", "%7C")

// Item renders a list line. Slack has no list syntax that renders reliably
// across its clients, so a bullet character is used and the line stands on its
// own.
func (mrkdwn) Item(s string) string { return "• " + s }

// escape neutralizes the three characters Slack treats as markup delimiters in
// message text.
//
// It runs over prose that came from a model or from a tool result — never over
// the markup this package composes — which is what sink.MapText exists to
// separate. Without it, a tool that returned "a < b" would produce a body Slack
// renders as a broken entity, and an answer containing "<@U123>" would render as
// a real mention of a real person.
func escape(s string) string {
	if !strings.ContainsAny(s, "&<>") {
		return s
	}
	// Ampersand first: escaping it after "<" would double-escape the "&" that
	// "&lt;" just introduced.
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	return strings.ReplaceAll(s, ">", "&gt;")
}
