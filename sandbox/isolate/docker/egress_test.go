package docker

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/sandbox/isolate"
	"github.com/urmzd/saige/sandbox/isolate/egress"
)

// upstreamServer answers every GET with the Authorization header it got and
// logs the request line.
const upstreamServer = `
import http.server
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = (self.headers.get("Authorization") or "").encode()
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
http.server.HTTPServer(("", 8080), H).serve_forever()
`

// fetch prints "<status> <body>" for one GET with a placeholder credential,
// or "error <reason>" when the request could not be made.
const fetch = `
import sys, urllib.request, urllib.error
req = urllib.request.Request(sys.argv[1], headers={"Authorization": "Bearer PLACEHOLDER"})
try:
    with urllib.request.urlopen(req, timeout=5) as r:
        print(r.status, r.read().decode())
except urllib.error.HTTPError as e:
    print(e.code, "")
except Exception as e:
    print("error", e)
`

// The whole path on a real daemon: the sandbox is on an internal network
// with only the gate; the gate also reaches a private upstream network. A
// request through the gate to the allowed host gets the real credential;
// one to another host is refused and never arrives; and nothing reaches
// either host without the gate.
func TestEgressThroughTheGate(t *testing.T) {
	d0, opts := dockerDriver(t, Config{})
	dir := opts.TempDir(t)
	arch, err := exec.Command("docker", "version", "--format", "{{.Server.Arch}}").Output()
	if err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", filepath.Join(dir, "gate"), "./testdata/gate")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+strings.TrimSpace(string(arch)))
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the gate: %v: %s", err, out)
	}
	suffix := isolate.NewID()
	sandboxNet, upstreamNet := "playscape-test-egress-"+suffix, "playscape-test-upstream-"+suffix
	gateName, upName, forbiddenName := "playscape-test-gate-"+suffix, "playscape-test-up-"+suffix, "playscape-test-forbidden-"+suffix
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", gateName, upName, forbiddenName).Run()
		_ = exec.Command("docker", "network", "rm", sandboxNet, upstreamNet).Run()
	})
	run("network", "create", "--internal", sandboxNet)
	run("network", "create", "--internal", upstreamNet)
	run("run", "-d", "--name", gateName, "--network", sandboxNet, "--network-alias", "gate",
		"--mount", "type=bind,source="+dir+",target=/g,readonly", "--entrypoint", "/g/gate", opts.Image)
	run("network", "connect", upstreamNet, gateName)
	for name, alias := range map[string]string{upName: "upstream", forbiddenName: "forbidden"} {
		run("run", "-d", "--name", name, "--network", upstreamNet, "--network-alias", alias, opts.Image, "python3", "-u", "-c", upstreamServer)
	}
	var token string
	for deadline := time.Now().Add(30 * time.Second); token == ""; time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the gate printed no token: %s", run("logs", gateName))
		}
		for _, line := range strings.Split(run("logs", gateName), "\n") {
			if tok, ok := strings.CutPrefix(line, "TOKEN "); ok {
				token = tok
			}
		}
	}

	// The sandbox reaches the gate by address: gVisor's netstack cannot
	// reach Docker's embedded DNS server on hosts whose NAT rules are in
	// nftables, so a name would resolve under runc only.
	gate := strings.TrimSpace(run("inspect", "--format", "{{(index .NetworkSettings.Networks \""+sandboxNet+"\").IPAddress}}", gateName)) + ":3128"

	// A proxy without an internal egress network is refused.
	if _, err := d0.Start(context.Background(), isolate.Spec{Image: opts.Image, Network: isolate.Network{Proxy: gate}}); err == nil {
		t.Fatal("a driver with no egress network started an instance with a proxy")
	}
	d, err := New(Config{
		WorkRoot: d0.workRoot, EgressNetwork: sandboxNet, Runtime: d0.runtime,
		InsecureDevelopmentRuntime: !sandboxRuntimes[d0.runtime], Log: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := d.Start(context.Background(), isolate.Spec{Image: opts.Image, Network: isolate.Network{Proxy: gate}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inst.Close(context.Background()) })

	get := func(target string, env map[string]string) string {
		t.Helper()
		var out strings.Builder
		e := map[string]string{"PATH": "/usr/local/bin:/usr/bin:/bin"}
		for k, v := range env {
			e[k] = v
		}
		st, err := inst.Exec(context.Background(), isolate.Command{Argv: []string{"python3", "-c", fetch, target}, Env: e, Timeout: time.Minute}, isolate.Stdio{Stdout: &out, Stderr: &out})
		if err != nil || st.Code != 0 {
			t.Fatalf("fetch %s: %+v, %v: %s", target, st, err, out.String())
		}
		return strings.TrimSpace(out.String())
	}
	proxy := egress.ProxyEnv(gate, token)

	if got := get("http://upstream:8080/", proxy); got != "200 Bearer real-gh" {
		t.Errorf("allowed host through the gate: %q", got)
	}
	if got := get("http://forbidden:8080/", proxy); got != "403" {
		t.Errorf("denied host through the gate: %q", got)
	}
	if got := get("http://upstream:8080/", egress.ProxyEnv(gate, "wrong")); got != "407" {
		t.Errorf("wrong token: %q", got)
	}
	for _, target := range []string{"http://upstream:8080/", "http://forbidden:8080/", "http://1.1.1.1/"} {
		if got := get(target, nil); !strings.HasPrefix(got, "error") {
			t.Errorf("%s without the gate: %q", target, got)
		}
	}
	if logs := run("logs", forbiddenName); strings.Contains(logs, "GET") {
		t.Errorf("the denied host received a request: %s", logs)
	}
	if logs := run("logs", upName); strings.Count(logs, "GET") != 1 {
		t.Errorf("the allowed host got other than one request: %s", logs)
	}
}
