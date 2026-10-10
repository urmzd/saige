package exec

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/tools/internal/safepath"
)

// Language is a language execute_code runs.
type Language string

// Languages execute_code knows how to run.
const (
	// LanguageShell runs the code as a shell script.
	LanguageShell Language = "shell"
	// LanguagePython runs the code with python3, or python when python3 is
	// missing.
	LanguagePython Language = "python"
	// LanguageGo builds the code as a main package and runs the binary. The
	// code may import only the standard library.
	LanguageGo Language = "go"
)

// CodeToolName is the name of the tool NewCodeTool returns.
const CodeToolName = "execute_code"

// ErrNoLanguages is returned when none of the requested languages is
// available in the sandbox.
var ErrNoLanguages = errors.New("exec: no requested language is available in the sandbox")

// detectTimeout bounds the probe that finds which languages a sandbox has.
const detectTimeout = 2 * time.Minute

// DetectLanguages reports which languages the sandbox can run, by running
// one probe command in dir. The shell is always available.
func DetectLanguages(ctx context.Context, sb Sandbox, dir string, env []string) ([]Language, error) {
	res, err := sb.Run(ctx, Command{
		Script:  `for p in python3 python go; do command -v "$p" >/dev/null 2>&1 && echo "$p"; done; true`,
		Dir:     dir,
		Env:     env,
		Timeout: detectTimeout,
	})
	if err != nil {
		return nil, err
	}
	// The probe ends in true, so a failure means the sandbox itself cannot
	// run commands, for example a wrapper the kernel refuses.
	if res.ExitCode != 0 || res.TimedOut {
		return nil, fmt.Errorf("sandbox cannot run commands: exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	found := map[string]bool{}
	for _, line := range strings.Fields(res.Stdout) {
		found[line] = true
	}
	langs := []Language{LanguageShell}
	if found["python3"] || found["python"] {
		langs = append(langs, LanguagePython)
	}
	if found["go"] {
		langs = append(langs, LanguageGo)
	}
	return langs, nil
}

// CodeOptions configures NewCodeTool.
type CodeOptions struct {
	// Languages limits the languages offered. Empty offers every language
	// DetectLanguages finds; a language listed but missing is left out.
	Languages []Language
}

// NewCodeTool returns execute_code, which runs a snippet of shell, Python,
// or Go behind sb, in root or a subdirectory of it, under policy.
//
// Like the bash tool it is always wrapped in a human_approval marker and
// refuses a policy that denies the network on a sandbox that cannot block
// it. Its capability class follows the sandbox: write when the sandbox
// confines file changes to the workspace (Isolation.Filesystem) and the
// network is denied, so an approval grant can cover it; destructive
// otherwise, so every call is asked about.
//
// The policy's command check applies to shell code and to the interpreter
// or compiler each language runs.
func NewCodeTool(ctx context.Context, sb Sandbox, root string, policy Policy, opts CodeOptions) (types.Tool, error) {
	if sb == nil {
		return nil, errors.New("exec: sandbox is required")
	}
	if root == "" {
		return nil, errors.New("exec: a workspace root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("exec: resolve root: %w", err)
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("exec: root %s is not a directory", abs)
	}
	if policy.network() == NetworkDeny && !sb.Isolation().Network {
		return nil, ErrNetworkUnenforced
	}
	available, err := DetectLanguages(ctx, sb, abs, policy.Env.environment())
	if err != nil {
		return nil, fmt.Errorf("exec: detect languages: %w", err)
	}
	langs := available
	if len(opts.Languages) > 0 {
		langs = nil
		for _, l := range opts.Languages {
			if slices.Contains(available, l) && !slices.Contains(langs, l) {
				langs = append(langs, l)
			}
		}
	}
	if len(langs) == 0 {
		return nil, ErrNoLanguages
	}

	capability := types.ToolCapabilityDestructive
	if sb.Isolation().Filesystem && policy.network() == NetworkDeny {
		capability = types.ToolCapabilityWrite
	}
	t := &codeTool{sandbox: sb, root: abs, policy: policy, langs: langs, capability: capability}
	return types.WithMarkers(t, types.Marker{
		Kind:    ApprovalKind,
		Message: "Running code requires approval: " + CodeToolName,
		Meta: map[string]any{
			"tool": CodeToolName, "mutating": true,
			"network": string(policy.network()), "confined": sb.Isolation().Filesystem,
		},
	}), nil
}

type codeTool struct {
	sandbox    Sandbox
	root       string
	policy     Policy
	langs      []Language
	capability types.ToolCapability
}

func (t *codeTool) Definition() types.ToolDef {
	names := make([]string, len(t.langs))
	for i, l := range t.langs {
		names[i] = string(l)
	}
	return types.ToolDef{
		Name:       CodeToolName,
		Capability: t.capability,
		Description: "Run a short program in the workspace and return its exit code, stdout, and stderr. " +
			"Languages: " + strings.Join(names, ", ") + ". Python runs with python3; Go code must be a main package " +
			"using only the standard library. Each call starts fresh: no state carries between calls. " +
			"The working directory is the workspace root unless cwd is given.",
		Parameters: types.ParameterSchema{
			Type:     types.SchemaObject,
			Required: []string{"language", "code"},
			Properties: map[string]types.PropertyDef{
				"language":        {Type: types.SchemaString, Enum: names, Description: "Language of the code."},
				"code":            {Type: types.SchemaString, Description: "The complete program or script."},
				"cwd":             {Type: types.SchemaString, Description: "Working directory relative to the workspace root (default: the root)."},
				"timeout_seconds": {Type: types.SchemaInteger, Description: "Time limit in seconds; capped by the configured maximum."},
			},
		},
	}
}

func (t *codeTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	lang := Language(stringValue(args["language"]))
	code := stringValue(args["code"])
	if strings.TrimSpace(code) == "" {
		return "", fmt.Errorf("%s: code is required", CodeToolName)
	}
	if !slices.Contains(t.langs, lang) {
		return "", fmt.Errorf("%s: language %q is not available (want one of %v)", CodeToolName, lang, t.langs)
	}
	script, err := codeScript(lang, code)
	if err != nil {
		return "", fmt.Errorf("%s: %w", CodeToolName, err)
	}
	checked := code
	if lang != LanguageShell {
		checked = script.runner
	}
	if err := t.policy.Commands.Check(checked); err != nil {
		return "", fmt.Errorf("%s: %w", CodeToolName, err)
	}
	dir := t.root
	if cwd := stringValue(args["cwd"]); cwd != "" {
		d, err := safepath.Resolve(t.root, cwd, true)
		if err != nil {
			return "", fmt.Errorf("%s: %w", CodeToolName, err)
		}
		dir = d
	}
	res, err := t.sandbox.Run(ctx, Command{
		Script:  script.text,
		Dir:     dir,
		Env:     t.policy.Env.environment(),
		Timeout: t.policy.callTimeout(args["timeout_seconds"]),
	})
	if err != nil {
		return "", fmt.Errorf("%s: %w", CodeToolName, err)
	}
	return formatResult(res), nil
}

// script is the shell text that runs a snippet, and the programs it starts.
type script struct {
	text   string
	runner string
}

// codeScript wraps code in a shell script for lang. Code is passed through
// a quoted heredoc, so the shell expands nothing in it.
func codeScript(lang Language, code string) (script, error) {
	if lang == LanguageShell {
		return script{text: code}, nil
	}
	delim, err := heredocDelimiter(code)
	if err != nil {
		return script{}, err
	}
	body := code
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	switch lang {
	case LanguagePython:
		return script{
			runner: "python3; python",
			text: "py=python3; command -v python3 >/dev/null 2>&1 || py=python\n" +
				`"$py" - <<'` + delim + "'\n" + body + delim + "\n",
		}, nil
	case LanguageGo:
		// The program is built in a private directory with its own module,
		// so the workspace's go.mod does not apply, and runs in the
		// requested working directory. GOTOOLCHAIN=local keeps the go
		// command from downloading a toolchain.
		return script{
			runner: "go",
			text: "d=$(mktemp -d) || exit 1\ntrap 'rm -rf \"$d\"' EXIT\n" +
				`cat > "$d/main.go" <<'` + delim + "'\n" + body + delim + "\n" +
				`printf 'module main\n\ngo 1.21\n' > "$d/go.mod"` + "\n" +
				`(cd "$d" && GOTOOLCHAIN=local go build -o prog .) || exit $?` + "\n" +
				`"$d/prog"` + "\n",
		}, nil
	}
	return script{}, fmt.Errorf("unknown language %q", lang)
}

// heredocDelimiter returns a delimiter that does not occur in code.
func heredocDelimiter(code string) (string, error) {
	for range 8 {
		var b [6]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		d := "SAIGE_CODE_" + hex.EncodeToString(b[:])
		if !strings.Contains(code, d) {
			return d, nil
		}
	}
	return "", errors.New("could not pick a heredoc delimiter")
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}
