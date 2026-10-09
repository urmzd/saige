package exec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/tools/internal/safepath"
)

// ApprovalKind is the marker kind on the bash tool.
const ApprovalKind = "human_approval"

// argCommand is the bash tool argument that holds the script.
const argCommand = "command"

// ErrNetworkUnenforced is returned when the policy denies the network but
// the sandbox cannot block it.
var ErrNetworkUnenforced = errors.New("exec: policy denies network access but the sandbox does not isolate the network")

// NewBashTool returns the bash tool. Commands run in root, which must be an
// existing directory, or in a subdirectory of it chosen per call.
//
// The tool is always wrapped in a human_approval marker; there is no option
// to remove it. Construction fails with ErrNetworkUnenforced when the policy
// denies the network (the default) and the sandbox does not isolate it, so
// a policy is never silently weaker than it reads. Pass NetworkAllow to run
// a plain Subprocess with network access.
func NewBashTool(sb Sandbox, root string, policy Policy) (types.Tool, error) {
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
	t := &bashTool{sandbox: sb, root: abs, policy: policy}
	return types.WithMarkers(t, types.Marker{
		Kind:    ApprovalKind,
		Message: "Shell command requires approval: bash",
		Meta:    map[string]any{"tool": "bash", "mutating": true, "network": string(policy.network())},
	}), nil
}

type bashTool struct {
	sandbox Sandbox
	root    string
	policy  Policy
}

func (t *bashTool) Definition() types.ToolDef {
	return types.ToolDef{
		Name:       "bash",
		Capability: types.ToolCapabilityDestructive,
		Description: "Run a shell command in the workspace and return its exit code, stdout, and stderr. " +
			"Each call starts a fresh shell: no state carries between calls.",
		Parameters: types.ParameterSchema{
			Type:     types.SchemaObject,
			Required: []string{argCommand},
			Properties: map[string]types.PropertyDef{
				argCommand:        {Type: types.SchemaString, Description: "The shell script to run."},
				"cwd":             {Type: types.SchemaString, Description: "Working directory relative to the workspace root (default: the root)."},
				"timeout_seconds": {Type: types.SchemaInteger, Description: "Time limit in seconds; capped by the configured maximum."},
			},
		},
	}
}

func (t *bashTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	script, _ := args[argCommand].(string)
	if strings.TrimSpace(script) == "" {
		return "", errors.New("bash: command is required")
	}
	if err := t.policy.Commands.Check(script); err != nil {
		return "", fmt.Errorf("bash: %w", err)
	}
	dir := t.root
	if cwd, _ := args["cwd"].(string); cwd != "" {
		d, err := safepath.Resolve(t.root, cwd, true)
		if err != nil {
			return "", fmt.Errorf("bash: %w", err)
		}
		dir = d
	}

	res, err := t.sandbox.Run(ctx, Command{
		Script:  script,
		Dir:     dir,
		Env:     t.policy.Env.environment(),
		Timeout: t.timeout(args["timeout_seconds"]),
	})
	if err != nil {
		return "", fmt.Errorf("bash: %w", err)
	}
	return formatResult(res), nil
}

// timeout picks the per-call limit: the requested value capped by
// MaxTimeout, or the policy default.
func (t *bashTool) timeout(v any) time.Duration {
	d := t.policy.Timeout
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
	if t.policy.MaxTimeout > 0 && (d <= 0 || d > t.policy.MaxTimeout) {
		d = t.policy.MaxTimeout
	}
	return d
}

func formatResult(r Result) string {
	var b strings.Builder
	switch {
	case r.TimedOut:
		b.WriteString("timed out\n")
	default:
		fmt.Fprintf(&b, "exit code: %d\n", r.ExitCode)
	}
	if r.Stdout != "" {
		b.WriteString("stdout:\n")
		b.WriteString(r.Stdout)
		if !strings.HasSuffix(r.Stdout, "\n") {
			b.WriteByte('\n')
		}
	}
	if r.Stderr != "" {
		b.WriteString("stderr:\n")
		b.WriteString(r.Stderr)
		if !strings.HasSuffix(r.Stderr, "\n") {
			b.WriteByte('\n')
		}
	}
	if r.Truncated {
		b.WriteString("(output truncated)\n")
	}
	return b.String()
}
