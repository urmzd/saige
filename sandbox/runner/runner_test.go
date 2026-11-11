package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalRun(t *testing.T) {
	var logs bytes.Buffer
	res, err := Local{}.Run(context.Background(), Request{
		Command: []string{"/bin/sh", "-c", `echo out; echo err >&2; echo "$A"; exit 4`},
		Env:     map[string]string{"A": "only-this"},
	}, &logs)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 4 {
		t.Fatalf("exit = %d", res.ExitCode)
	}
	for _, want := range []string{"out", "err", "only-this"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("logs %q missing %q", logs.String(), want)
		}
	}
}

func TestLocalRunsInDir(t *testing.T) {
	dir := t.TempDir()
	var logs bytes.Buffer
	res, err := Local{Dir: dir}.Run(context.Background(), Request{
		Command: []string{"/bin/sh", "-c", "pwd; echo made > made.txt"},
	}, &logs)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("run: %+v, %v (%s)", res, err, logs.String())
	}
	got, err := os.ReadFile(filepath.Join(dir, "made.txt"))
	if err != nil || string(got) != "made\n" {
		t.Fatalf("file in Dir = %q, %v", got, err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("a caller's Dir must survive the run: %v", err)
	}
}

func TestLocalEnvIsNotInherited(t *testing.T) {
	t.Setenv("SANDBOX_SECRET", "leak")
	var logs bytes.Buffer
	if _, err := (Local{}).Run(context.Background(), Request{Command: []string{"/usr/bin/env"}}, &logs); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "SANDBOX_SECRET") {
		t.Fatalf("host env leaked: %q", logs.String())
	}
}

func TestLocalErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := (Local{}).Run(ctx, Request{}, io.Discard); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("empty command: %v", err)
	}
	if _, err := (Local{}).Run(ctx, Request{Command: []string{"/nonexistent/bin"}}, io.Discard); !errors.Is(err, ErrStart) {
		t.Fatalf("missing binary: %v", err)
	}
	_, err := Local{}.Run(ctx, Request{Command: []string{"/bin/sh", "-c", "sleep 5"}, Timeout: 100 * time.Millisecond}, io.Discard)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("timeout: %v", err)
	}
}

// recorder is a fake Executor that records the request it was given.
type recorder struct {
	req  Request
	logs string
	code int
	err  error
}

func (r *recorder) Run(_ context.Context, req Request, logs io.Writer) (Result, error) {
	r.req = req
	_, _ = io.WriteString(logs, r.logs)
	return Result{ExitCode: r.code}, r.err
}

func TestShellBuildsRequestAndCapsLogs(t *testing.T) {
	rec := &recorder{logs: "0123456789", code: 2}
	sh := Shell("sh", rec, ShellConfig{Image: "img", Env: map[string]string{"K": "v"}, Timeout: time.Minute, MaxLogBytes: 4})
	raw, err := sh.Call(context.Background(), nil, []byte(`{"script":"echo hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	var out ShellOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.ExitCode != 2 || out.Logs != "0123" || !out.LogsTruncated || out.RunID != rec.req.ID || out.RunID == "" {
		t.Fatalf("out = %+v", out)
	}
	if got := strings.Join(rec.req.Command, "|"); got != "/bin/sh|-c|echo hi" {
		t.Fatalf("command = %q", got)
	}
	if rec.req.Image != "img" || rec.req.Env["K"] != "v" || rec.req.Timeout != time.Minute {
		t.Fatalf("request = %+v", rec.req)
	}
}

func TestShellRejectsBadInput(t *testing.T) {
	sh := Shell("sh", &recorder{}, ShellConfig{})
	for _, in := range []string{``, `{}`, `{"script":""}`, `"echo"`} {
		if _, err := sh.Call(context.Background(), nil, []byte(in)); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("input %q: err = %v", in, err)
		}
	}
}

func TestShellPropagatesRunErrors(t *testing.T) {
	sh := Shell("sh", &recorder{err: ErrTimeout}, ShellConfig{})
	if _, err := sh.Call(context.Background(), nil, []byte(`{"script":"x"}`)); !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v", err)
	}
}

func TestShellOverLocal(t *testing.T) {
	sh := Shell("sh", Local{}, ShellConfig{})
	raw, err := sh.Call(context.Background(), nil, []byte(`{"script":"echo hello; exit 1"}`))
	if err != nil {
		t.Fatal(err)
	}
	var out ShellOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.ExitCode != 1 || out.Logs != "hello\n" {
		t.Fatalf("out = %+v", out)
	}
}
