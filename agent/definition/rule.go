package definition

import (
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/urmzd/saige/agent/types"
)

// Rule is one approval rule in the permission syntax of Claude Code: a tool
// name, optionally ending in *, and an optional specifier in parentheses.
//
//	read_file              every call of read_file
//	mcp_github_*           every tool whose name starts with mcp_github_
//	Bash(git status:*)     shell commands that start with "git status"
//	Bash(npm test)         exactly the command "npm test"
//	Read(src/**)           file tools reading a path under src/
//	WebFetch(domain:go.dev) fetch_url for go.dev and its subdomains
//	deploy(env:staging)    calls of deploy whose env argument is "staging"
//	deploy(branch:fix/*)   calls whose branch argument starts with "fix/"
//	deploy(dir:/srv/app/**) calls whose dir argument is a path at or below /srv/app
//
// The Claude Code names Bash, Read, Write, Edit, Glob, Grep, LS and
// WebFetch name saige's own tools: bash and execute_code (shell only),
// read_file, write_file, edit_file, glob, grep, list_dir and fetch_url.
//
// Any other tool takes a field:pattern specifier, matched against one
// argument as a types.ArgMatch is: dots in the field step into nested
// objects, and a call without the argument never matches.
type Rule struct {
	// Tool is the tool name or pattern as written.
	Tool string
	// Specifier is the text in parentheses; HasSpecifier tells "Bash()"
	// from "Bash".
	Specifier    string
	HasSpecifier bool
}

// ruleAliases maps the Claude Code tool names to saige's.
var ruleAliases = map[string][]string{
	"bash":     {"bash", "execute_code"},
	"read":     {"read_file"},
	"write":    {"write_file"},
	"edit":     {"edit_file"},
	"glob":     {"glob"},
	"grep":     {"grep"},
	"ls":       {"list_dir"},
	"webfetch": {"fetch_url"},
}

// ParseRule parses one rule.
func ParseRule(s string) (Rule, error) {
	s = strings.TrimSpace(s)
	name, spec, has := strings.Cut(s, "(")
	r := Rule{Tool: strings.TrimSpace(name), HasSpecifier: has}
	if has {
		if !strings.HasSuffix(spec, ")") {
			return Rule{}, fmt.Errorf("rule %q: missing closing parenthesis", s)
		}
		r.Specifier = strings.TrimSpace(strings.TrimSuffix(spec, ")"))
		if r.Specifier == "" {
			return Rule{}, fmt.Errorf("rule %q: empty specifier; write %s to match every call", s, r.Tool)
		}
	}
	if r.Tool == "" {
		return Rule{}, fmt.Errorf("rule %q: missing tool name", s)
	}
	for i, c := range r.Tool {
		ok := c == '_' || c == '-' || c == '.' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '*' && i == len(r.Tool)-1
		if !ok {
			return Rule{}, fmt.Errorf("rule %q: tool names use letters, digits, _, - and ., with an optional trailing *", s)
		}
	}
	if has {
		switch r.kind() {
		case specCommand:
			if strings.Contains(strings.TrimSuffix(r.Specifier, ":*"), ":*") {
				return Rule{}, fmt.Errorf("rule %q: :* may only end a command prefix", s)
			}
		case specPath:
			if !doublestar.ValidatePattern(r.Specifier) {
				return Rule{}, fmt.Errorf("rule %q: invalid path pattern", s)
			}
		case specDomain:
			if !strings.HasPrefix(r.Specifier, "domain:") || strings.TrimPrefix(r.Specifier, "domain:") == "" {
				return Rule{}, fmt.Errorf("rule %q: write WebFetch(domain:example.com)", s)
			}
		default:
			if _, err := r.argMatch(); err != nil {
				return Rule{}, fmt.Errorf("rule %q: %w", s, err)
			}
		}
	}
	return r, nil
}

func (r Rule) String() string {
	if !r.HasSpecifier {
		return r.Tool
	}
	return r.Tool + "(" + r.Specifier + ")"
}

type specKind int

const (
	specNone specKind = iota
	specCommand
	specPath
	specDomain
	specArg
)

// kind says what the specifier is matched against, from the tool name.
func (r Rule) kind() specKind {
	switch strings.ToLower(r.Tool) {
	case "bash", "execute_code":
		return specCommand
	case "read", "write", "edit", "glob", "grep", "ls",
		"read_file", "write_file", "edit_file", "list_dir":
		return specPath
	case "webfetch", "fetch_url":
		return specDomain
	}
	return specArg
}

// argMatch parses a field:pattern specifier. The pattern is an exact value,
// a text prefix ending in *, or a path prefix ending in /**.
func (r Rule) argMatch() (types.ArgMatch, error) {
	field, pattern, ok := strings.Cut(r.Specifier, ":")
	field, pattern = strings.TrimSpace(field), strings.TrimSpace(pattern)
	if !ok || field == "" || pattern == "" {
		return types.ArgMatch{}, fmt.Errorf("the specifier names one argument: write %s(field:value)", r.Tool)
	}
	for _, c := range field {
		if c != '_' && c != '-' && c != '.' && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return types.ArgMatch{}, fmt.Errorf("argument name %q: use letters, digits, _, - and dots", field)
		}
	}
	m := types.ArgMatch{Field: field}
	if root, ok := strings.CutSuffix(pattern, "/**"); ok {
		m.PathPrefix = root
	} else if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		m.Prefix = prefix
	} else {
		m.Equals = pattern
	}
	if m.PathPrefix == "" && m.Prefix == "" && m.Equals == "" {
		return types.ArgMatch{}, fmt.Errorf("the pattern needs text before its wildcard")
	}
	return m, nil
}

// namesTool reports whether the rule's tool part covers tool.
func (r Rule) namesTool(tool string) bool {
	pattern := strings.ToLower(r.Tool)
	tool = strings.ToLower(tool)
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(tool, prefix)
	}
	if aliases, ok := ruleAliases[pattern]; ok {
		for _, a := range aliases {
			if a == tool {
				return true
			}
		}
		return false
	}
	return pattern == tool
}

// Matches reports whether the rule covers a call of tool with args. strict
// is set for allow rules: a shell command matches only when it is one
// simple command, so Bash(git status:*) never allows
// "git status && rm -rf ~". Ask and deny rules match when any command in a
// compound one matches.
func (r Rule) Matches(tool string, args map[string]any, strict bool) bool {
	if !r.namesTool(tool) {
		return false
	}
	if strings.EqualFold(tool, "execute_code") {
		// Only shell code is a command; a Bash rule never covers Python.
		lang, _ := args["language"].(string)
		if lang != "shell" && (r.HasSpecifier || strings.EqualFold(r.Tool, "bash")) {
			return false
		}
	}
	if !r.HasSpecifier {
		return true
	}
	switch r.kind() {
	case specCommand:
		cmd := commandArg(args)
		if strict {
			return simpleCommand(cmd) && commandMatches(r.Specifier, cmd)
		}
		for _, part := range splitCommands(cmd) {
			if commandMatches(r.Specifier, part) {
				return true
			}
		}
		return false
	case specPath:
		p, _ := args["path"].(string)
		if p == "" {
			p, _ = args["pattern"].(string)
		}
		if p == "" {
			return false
		}
		p = path.Clean(strings.TrimPrefix(p, "./"))
		if strings.HasPrefix(p, "../") || p == ".." {
			return false
		}
		ok, err := doublestar.Match(path.Clean(strings.TrimPrefix(r.Specifier, "./")), p)
		return err == nil && ok
	case specDomain:
		raw, _ := args["url"].(string)
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Hostname() == "" {
			return false
		}
		host := strings.ToLower(u.Hostname())
		want := strings.ToLower(strings.TrimPrefix(r.Specifier, "domain:"))
		return host == want || strings.HasSuffix(host, "."+want)
	case specArg:
		m, err := r.argMatch()
		return err == nil && m.Matches(args)
	}
	return false
}

func commandArg(args map[string]any) string {
	if s, ok := args["command"].(string); ok {
		return strings.TrimSpace(s)
	}
	s, _ := args["code"].(string)
	return strings.TrimSpace(s)
}

// commandMatches matches "prefix:*", "*" or an exact command.
func commandMatches(spec, cmd string) bool {
	cmd = strings.Join(strings.Fields(cmd), " ")
	if spec == "*" {
		return cmd != ""
	}
	if prefix, ok := strings.CutSuffix(spec, ":*"); ok {
		prefix = strings.Join(strings.Fields(prefix), " ")
		return cmd == prefix || strings.HasPrefix(cmd, prefix+" ")
	}
	return cmd == strings.Join(strings.Fields(spec), " ")
}

// shellOperators are the characters that join, redirect or substitute
// commands. A command with any of them is not simple.
const shellOperators = ";&|<>`\n$(){}"

func simpleCommand(cmd string) bool {
	return cmd != "" && !strings.ContainsAny(cmd, shellOperators)
}

// splitCommands splits a compound command at its operators, keeping the
// text of each command. It over-splits rather than under-splits: a
// conservative deny or ask rule is better matched too often.
func splitCommands(cmd string) []string {
	parts := strings.FieldsFunc(cmd, func(r rune) bool { return strings.ContainsRune(shellOperators, r) })
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
