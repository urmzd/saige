package exec

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ErrCommandDenied is returned when a script names a program the policy
// does not allow.
var ErrCommandDenied = errors.New("exec: command denied by policy")

// NetworkPolicy says whether commands may use the network.
type NetworkPolicy string

const (
	// NetworkDeny requires a sandbox that blocks network access. It is the
	// default.
	NetworkDeny NetworkPolicy = "deny"
	// NetworkAllow lets commands use the network.
	NetworkAllow NetworkPolicy = "allow"
)

// CommandPolicy restricts which programs a script may run. Entries are
// program names, compared against the base name of each command word.
//
// The check reads the script's command positions (the first word of each
// pipeline stage, list item, and substitution) without running a shell
// parser. It is a guardrail that catches plain mistakes; it is not an
// isolation boundary, because a shell can always build a program name at
// run time. The Sandbox and the approval marker are the boundary.
type CommandPolicy struct {
	// Allow, when non-empty, is the complete set of programs a script may
	// run.
	Allow []string
	// Deny lists programs a script may never run. It wins over Allow.
	Deny []string
}

// EnvPolicy builds the environment for each command from nothing: no
// variable is inherited unless it is named here, so credentials in the
// parent environment do not leak into commands.
type EnvPolicy struct {
	// Inherit names variables copied from the parent process when set.
	Inherit []string
	// Set adds or overrides variables.
	Set map[string]string
}

// Policy configures the bash tool.
type Policy struct {
	Commands CommandPolicy
	Env      EnvPolicy
	// Network is NetworkDeny when empty.
	Network NetworkPolicy
	// Timeout is the default per-command limit.
	Timeout time.Duration
	// MaxTimeout caps the timeout a call may ask for.
	MaxTimeout time.Duration
}

// DefaultDeny lists programs denied by DefaultPolicy: privilege escalation
// and host power or disk management.
var DefaultDeny = []string{"sudo", "su", "doas", "pkexec", "shutdown", "reboot", "halt", "poweroff", "mkfs", "fdisk", "diskutil"}

// DefaultInherit lists variables DefaultPolicy copies from the parent.
var DefaultInherit = []string{"PATH", "HOME", "USER", "LANG", "LC_ALL", "TERM", "TMPDIR", "TZ"}

// DefaultPolicy denies the network and DefaultDeny programs, inherits only
// DefaultInherit, and limits commands to two minutes by default and ten at
// most.
func DefaultPolicy() Policy {
	return Policy{
		Commands:   CommandPolicy{Deny: append([]string(nil), DefaultDeny...)},
		Env:        EnvPolicy{Inherit: append([]string(nil), DefaultInherit...)},
		Network:    NetworkDeny,
		Timeout:    2 * time.Minute,
		MaxTimeout: 10 * time.Minute,
	}
}

func (p Policy) network() NetworkPolicy {
	if p.Network == "" {
		return NetworkDeny
	}
	return p.Network
}

// callTimeout picks a call's limit: the requested seconds capped by
// MaxTimeout, or Timeout when the call asks for none.
func (p Policy) callTimeout(v any) time.Duration {
	d := p.Timeout
	var secs float64
	switch n := v.(type) {
	case float64:
		secs = n
	case int:
		secs = float64(n)
	case json.Number:
		secs, _ = n.Float64()
	}
	if secs > 0 {
		d = time.Duration(secs * float64(time.Second))
	}
	if p.MaxTimeout > 0 && (d <= 0 || d > p.MaxTimeout) {
		d = p.MaxTimeout
	}
	return d
}

// environment returns the KEY=VALUE list a command runs with.
func (p EnvPolicy) environment() []string {
	vars := map[string]string{}
	for _, kv := range environ(p.Inherit) {
		k, v, _ := strings.Cut(kv, "=")
		vars[k] = v
	}
	for k, v := range p.Set {
		vars[k] = v
	}
	out := make([]string, 0, len(vars))
	for k, v := range vars {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// Check reports whether script runs only programs the policy allows. The
// error wraps ErrCommandDenied and names the first program refused.
func (p CommandPolicy) Check(script string) error {
	deny := set(p.Deny)
	allow := set(p.Allow)
	for _, prog := range Programs(script) {
		if deny[prog] {
			return fmt.Errorf("%w: %s is denied", ErrCommandDenied, prog)
		}
		if len(allow) > 0 && !allow[prog] {
			return fmt.Errorf("%w: %s is not in the allow list", ErrCommandDenied, prog)
		}
	}
	return nil
}

func set(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// prefixWords run the next word as a program, so both are checked.
var prefixWords = map[string]bool{
	"env": true, "command": true, "exec": true, "nohup": true, "time": true,
	"nice": true, "xargs": true, "builtin": true, "timeout": true, "stdbuf": true,
}

// shellKeywords may start a list item without being a program.
var shellKeywords = map[string]bool{
	"if": true, "then": true, "else": true, "elif": true, "fi": true, "do": true,
	"done": true, "while": true, "until": true, "for": true, "in": true, "case": true,
	"esac": true, "function": true, "{": true, "}": true, "!": true, "select": true,
}

// Programs returns the base names of the programs in script's command
// positions, in order, without duplicates. Quoting is not interpreted, so
// text inside quotes that contains a separator is read as a command too;
// that errs toward refusing a script, never toward allowing one.
func Programs(script string) []string {
	r := strings.NewReplacer("$(", "\n", "`", "\n", "&&", "\n", "||", "\n",
		";", "\n", "|", "\n", "&", "\n", "(", "\n", ")", "\n")
	// Redirections such as 2>&1 and &>file are not list separators.
	script = strings.NewReplacer(">&", ">", "&>", ">").Replace(script)
	seen := map[string]bool{}
	var out []string
	add := func(w string) {
		w = filepath.Base(strings.Trim(w, `"'\`))
		if w == "" || w == "." || seen[w] {
			return
		}
		seen[w] = true
		out = append(out, w)
	}
	for _, segment := range strings.Split(r.Replace(script), "\n") {
		words := strings.Fields(segment)
		for len(words) > 0 {
			w := words[0]
			switch {
			case shellKeywords[w]:
				words = words[1:]
				continue
			case isAssignment(w), isDigits(w):
				// A variable prefix, or a count such as "timeout 5".
				words = words[1:]
				continue
			case strings.HasPrefix(w, "-") || strings.HasPrefix(w, "#"):
				// An option of a prefix word, or a comment.
				if strings.HasPrefix(w, "#") {
					words = nil
					continue
				}
				words = words[1:]
				continue
			}
			add(w)
			if !prefixWords[filepath.Base(w)] {
				break
			}
			words = words[1:]
		}
	}
	return out
}

// isAssignment reports whether w is a NAME=value prefix.
func isAssignment(w string) bool {
	name, _, ok := strings.Cut(w, "=")
	if !ok || name == "" {
		return false
	}
	for i, r := range name {
		if r != '_' && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (i == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// isDigits reports whether w is a non-empty run of ASCII digits.
func isDigits(w string) bool {
	for _, r := range w {
		if r < '0' || r > '9' {
			return false
		}
	}
	return w != ""
}
