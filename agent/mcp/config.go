package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/urmzd/saige/agent/registry"
)

// ConfigOption customizes LoadConfig.
type ConfigOption func(*configOptions)

type configOptions struct {
	lookup     func(string) (string, bool)
	httpClient *http.Client
	setClient  bool
	registry   *registry.Registry[ServerSpec]
	regOpts    []registry.Option
}

// WithEnvLookup replaces os.LookupEnv for ${VAR} expansion.
func WithEnvLookup(fn func(string) (string, bool)) ConfigOption {
	return func(o *configOptions) { o.lookup = fn }
}

// WithConfigHTTPClient sets the HTTP client for every remote server in the
// file. The default is SafeHTTPClient(false), which refuses cloud metadata
// and link-local addresses. Pass nil to use the transport's default client
// with no address guard.
func WithConfigHTTPClient(c *http.Client) ConfigOption {
	return func(o *configOptions) { o.httpClient, o.setClient = c, true }
}

// WithConfigRegistry records each loaded spec as a new revision in r, under
// the server's name, so a configuration change can be pinned or rolled back.
func WithConfigRegistry(r *registry.Registry[ServerSpec], opts ...registry.Option) ConfigOption {
	return func(o *configOptions) { o.registry, o.regOpts = r, opts }
}

// configFile is the mcpServers document that Claude Code, Gemini CLI and
// other MCP hosts read.
type configFile struct {
	MCPServers map[string]configServer `json:"mcpServers"`
}

type configServer struct {
	Type    string            `json:"type"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	Cwd     string            `json:"cwd"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

// LoadConfig reads a standard MCP configuration file:
//
//	{"mcpServers": {"name": {"command": "...", "args": [...], "env": {...}}}}
//	{"mcpServers": {"name": {"type": "http", "url": "...", "headers": {...}}}}
//
// ${VAR} and ${VAR:-default} are expanded from the environment in command,
// args, env values, cwd, url and header values, so secrets stay out of the
// file. A reference to an unset variable with no default is an error. As in
// other MCP hosts, a local server's env adds to this process's environment
// rather than replacing it. Specs are returned sorted by name, and each one
// is validated.
func LoadConfig(path string, opts ...ConfigOption) ([]ServerSpec, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the path is the caller's own configuration file
	if err != nil {
		return nil, fmt.Errorf("mcp: read config: %w", err)
	}
	return ParseConfig(raw, opts...)
}

// ParseConfig is LoadConfig for a document already in memory.
func ParseConfig(raw []byte, opts ...ConfigOption) ([]ServerSpec, error) {
	o := configOptions{lookup: os.LookupEnv}
	for _, opt := range opts {
		opt(&o)
	}
	if !o.setClient {
		o.httpClient = SafeHTTPClient(false)
	}

	var file configFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("mcp: parse config: %w", err)
	}
	names := make([]string, 0, len(file.MCPServers))
	for name := range file.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)

	specs := make([]ServerSpec, 0, len(names))
	for _, name := range names {
		spec, err := o.spec(name, file.MCPServers[name])
		if err != nil {
			return nil, err
		}
		if err := spec.Validate(); err != nil {
			return nil, err
		}
		specs = append(specs, spec)
	}
	if o.registry != nil {
		for _, s := range specs {
			o.registry.Register(s.Name, s, o.regOpts...)
		}
	}
	return specs, nil
}

func (o *configOptions) spec(name string, s configServer) (ServerSpec, error) {
	expand := func(field, v string) (string, error) {
		out, err := expandEnv(v, o.lookup)
		if err != nil {
			return "", fmt.Errorf("mcp: server %q %s: %w", name, field, err)
		}
		return out, nil
	}
	var err error
	spec := ServerSpec{Name: name}
	switch strings.ToLower(s.Type) {
	case "", "stdio", "http", "streamable-http", "streamablehttp":
	case "sse":
		return spec, fmt.Errorf("mcp: server %q uses the legacy SSE transport, which is not supported; use streamable HTTP", name)
	default:
		return spec, fmt.Errorf("mcp: server %q has unknown type %q", name, s.Type)
	}

	if spec.Command, err = expand("command", s.Command); err != nil {
		return spec, err
	}
	for i, a := range s.Args {
		v, err := expand(fmt.Sprintf("args[%d]", i), a)
		if err != nil {
			return spec, err
		}
		spec.Args = append(spec.Args, v)
	}
	if spec.WorkDir, err = expand("cwd", s.Cwd); err != nil {
		return spec, err
	}
	if len(s.Env) > 0 {
		keys := make([]string, 0, len(s.Env))
		for k := range s.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		spec.Env = os.Environ()
		for _, k := range keys {
			v, err := expand("env."+k, s.Env[k])
			if err != nil {
				return spec, err
			}
			spec.Env = append(spec.Env, k+"="+v)
		}
	}

	if spec.URL, err = expand("url", s.URL); err != nil {
		return spec, err
	}
	if len(s.Headers) > 0 {
		spec.Headers = make(map[string]string, len(s.Headers))
		for k, v := range s.Headers {
			if spec.Headers[k], err = expand("headers."+k, v); err != nil {
				return spec, err
			}
		}
	}
	if spec.URL != "" {
		spec.HTTPClient = o.httpClient
	}
	return spec, nil
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// expandEnv replaces ${VAR} and ${VAR:-default}. Bare $VAR is left alone so
// a literal dollar sign in an argument survives.
func expandEnv(s string, lookup func(string) (string, bool)) (string, error) {
	var missing []string
	out := envRef.ReplaceAllStringFunc(s, func(m string) string {
		parts := envRef.FindStringSubmatch(m)
		v, ok := lookup(parts[1])
		if ok && (v != "" || parts[2] == "") {
			return v
		}
		if parts[2] != "" {
			return parts[3]
		}
		missing = append(missing, parts[1])
		return ""
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("environment variable %s is not set", strings.Join(missing, ", "))
	}
	return out, nil
}
