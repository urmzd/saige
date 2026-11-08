package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/urmzd/saige/sandbox/task"
	"github.com/urmzd/saige/sandbox/tool"
)

// DefaultMaxLogBytes caps the logs a Shell tool returns when
// ShellConfig.MaxLogBytes is zero.
const DefaultMaxLogBytes = 1 << 20

// ShellConfig configures a Shell tool. Everything except the script is
// fixed by the operator; the agent cannot choose the image, environment,
// or limits.
type ShellConfig struct {
	// Shell is the interpreter argv the script is appended to. Empty
	// means ["/bin/sh", "-c"].
	Shell []string
	// Image, Env, Timeout, and Resources are copied into every Request.
	Image     string
	Env       map[string]string
	Timeout   time.Duration
	Resources Resources
	// MaxLogBytes caps the returned logs; zero means DefaultMaxLogBytes.
	MaxLogBytes int
}

// ShellInput is the JSON input a Shell tool accepts.
type ShellInput struct {
	Script string `json:"script"`
}

// ShellOutput is the JSON a Shell tool returns for every run that exited,
// whatever its exit code.
type ShellOutput struct {
	RunID         string `json:"run_id"`
	ExitCode      int    `json:"exit_code"`
	Logs          string `json:"logs"`
	LogsTruncated bool   `json:"logs_truncated,omitempty"`
}

// Shell returns a tool named name that runs its input script through ex,
// one run per call. With a runner/k8s executor that is one Kubernetes Job
// per call. Each call gets a fresh random run ID, returned in the output so
// logs and cluster objects can be correlated.
func Shell(name string, ex Executor, cfg ShellConfig) tool.Tool {
	if len(cfg.Shell) == 0 {
		cfg.Shell = []string{"/bin/sh", "-c"}
	}
	if cfg.MaxLogBytes <= 0 {
		cfg.MaxLogBytes = DefaultMaxLogBytes
	}
	return tool.Func(name, func(ctx context.Context, _ tool.Runtime, input []byte) ([]byte, error) {
		var in ShellInput
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, fmt.Errorf("%w: want {\"script\": ...}: %w", ErrInvalidRequest, err)
		}
		if in.Script == "" {
			return nil, fmt.Errorf("%w: empty script", ErrInvalidRequest)
		}
		req := Request{
			ID:        task.NewID(),
			Image:     cfg.Image,
			Command:   append(append([]string{}, cfg.Shell...), in.Script),
			Env:       cfg.Env,
			Timeout:   cfg.Timeout,
			Resources: cfg.Resources,
		}
		logs := &capped{max: cfg.MaxLogBytes}
		res, err := ex.Run(ctx, req, logs)
		if err != nil {
			return nil, fmt.Errorf("runner: shell %s: run %s: %w", name, req.ID, err)
		}
		return json.Marshal(ShellOutput{
			RunID:         req.ID,
			ExitCode:      res.ExitCode,
			Logs:          logs.String(),
			LogsTruncated: logs.truncated,
		})
	})
}

// capped keeps the first max bytes written and discards the rest. It never
// fails a write, so a chatty run is never killed by its log sink.
type capped struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (c *capped) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := c.max - c.buf.Len(); room < len(p) {
		c.truncated = true
		if room > 0 {
			c.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (c *capped) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// syncWriter serializes writes from concurrent stdout/stderr copiers.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
