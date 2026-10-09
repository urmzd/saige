package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAssetName(t *testing.T) {
	tests := []struct {
		bin, goos, goarch, want string
	}{
		{"saige", "linux", "amd64", "saige-linux-amd64"},
		{"saige", "linux", "arm64", "saige-linux-arm64"},
		{"saige", "darwin", "amd64", "saige-darwin-amd64"},
		{"saige", "darwin", "arm64", "saige-darwin-arm64"},
		{"saige", "windows", "amd64", "saige-windows-amd64.exe"},
		{"saige-mcp", "linux", "arm64", "saige-mcp-linux-arm64"},
	}
	for _, tt := range tests {
		if got := assetName(tt.bin, tt.goos, tt.goarch); got != tt.want {
			t.Errorf("assetName(%q, %q, %q) = %q, want %q", tt.bin, tt.goos, tt.goarch, got, tt.want)
		}
	}
}

// TestReleaseWorkflowPublishesUpdateAssets keeps the release workflow and
// saige update in agreement: every platform update looks up must be uploaded.
func TestReleaseWorkflowPublishesUpdateAssets(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("reading release workflow: %v", err)
	}
	wf := string(data)
	platforms := [][2]string{
		{runtime.GOOS, runtime.GOARCH},
		{"linux", "amd64"}, {"linux", "arm64"},
		{"darwin", "amd64"}, {"darwin", "arm64"},
		{"windows", "amd64"},
	}
	for _, bin := range []string{"saige", "saige-mcp"} {
		for _, p := range platforms {
			name := assetName(bin, p[0], p[1])
			if !strings.Contains(wf, "build/bin/"+name+"\n") {
				t.Errorf("release workflow does not upload %s", name)
			}
		}
	}
	if !strings.Contains(wf, "build/bin/"+checksumAsset) {
		t.Errorf("release workflow does not upload %s", checksumAsset)
	}
}

// releaseFixture serves a fake latest-release API, binary asset and checksum
// file. sums overrides the checksum file body when non-empty.
func releaseFixture(t *testing.T, tag string, asset []byte, assetName, sums string, withSums bool) *httptest.Server {
	t.Helper()
	sum := sha256.Sum256(asset)
	if sums == "" {
		sums = fmt.Sprintf("%s  %s\n%s  other-file\n", hex.EncodeToString(sum[:]), assetName, strings.Repeat("0", 64))
	}
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/latest", func(w http.ResponseWriter, r *http.Request) {
		assets := []map[string]string{
			{"name": assetName, "browser_download_url": srv.URL + "/bin"},
		}
		if withSums {
			assets = append(assets, map[string]string{"name": checksumAsset, "browser_download_url": srv.URL + "/sums"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": tag, "assets": assets})
	})
	mux.HandleFunc("/bin", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(asset) })
	mux.HandleFunc("/sums", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(sums)) })
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestUpdaterRun(t *testing.T) {
	newBinary := []byte("#!/bin/sh\necho new\n")
	name := assetName("saige", "linux", "arm64")

	tests := []struct {
		name      string
		current   string
		check     bool
		sums      string
		withSums  bool
		assetName string
		wantErr   string
		replaced  bool
		wantOut   string
	}{
		{name: "installs newer release", current: "1.0.0", withSums: true, assetName: name, replaced: true, wantOut: "updated"},
		{name: "already current", current: "2.0.0", withSums: true, assetName: name, wantOut: "already up to date"},
		{name: "check only", current: "1.0.0", check: true, withSums: true, assetName: name, wantOut: "update available"},
		{name: "missing platform asset", current: "1.0.0", withSums: true, assetName: "saige-linux", wantErr: "no asset named"},
		{name: "missing checksums", current: "1.0.0", assetName: name, wantErr: "unverified"},
		{name: "checksum mismatch", current: "1.0.0", withSums: true, assetName: name, sums: strings.Repeat("a", 64) + "  " + name + "\n", wantErr: "checksum mismatch"},
		{name: "asset absent from checksums", current: "1.0.0", withSums: true, assetName: name, sums: strings.Repeat("a", 64) + "  other\n", wantErr: "no entry"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := releaseFixture(t, "v2.0.0", newBinary, tt.assetName, tt.sums, tt.withSums)
			exe := filepath.Join(t.TempDir(), "saige")
			if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil { //nolint:gosec // test binary
				t.Fatal(err)
			}
			var out bytes.Buffer
			u := &updater{client: srv.Client(), apiURL: srv.URL + "/latest", exe: exe, goos: "linux", goarch: "arm64", out: &out}

			err := u.run(context.Background(), tt.current, tt.check)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("run: %v", err)
			}
			if tt.wantOut != "" && !strings.Contains(out.String(), tt.wantOut) {
				t.Fatalf("output %q does not contain %q", out.String(), tt.wantOut)
			}

			got, _ := os.ReadFile(exe) //nolint:gosec // test path
			if replaced := bytes.Equal(got, newBinary); replaced != tt.replaced {
				t.Fatalf("binary replaced = %v, want %v", replaced, tt.replaced)
			}
			entries, _ := os.ReadDir(filepath.Dir(exe))
			if len(entries) != 1 {
				t.Fatalf("temp files left behind: %v", entries)
			}
		})
	}
}

func TestReplaceExecutableWindowsMovesOldAside(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "saige.exe")
	src := filepath.Join(dir, "new")
	_ = os.WriteFile(dst, []byte("old"), 0o600)
	_ = os.WriteFile(src, []byte("new"), 0o600)
	if err := replaceExecutable("windows", src, dst); err != nil {
		t.Fatalf("replaceExecutable: %v", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "new" { //nolint:gosec // test path
		t.Fatalf("dst = %q, want new", got)
	}
}

func TestFindChecksum(t *testing.T) {
	digest := strings.Repeat("ab", 32)
	tests := []struct {
		name, body, want, wantErr string
	}{
		{name: "text mode", body: digest + "  saige-linux-amd64\n", want: digest},
		{name: "binary mode", body: digest + " *saige-linux-amd64\n", want: digest},
		{name: "uppercase digest", body: strings.ToUpper(digest) + "  saige-linux-amd64\n", want: digest},
		{name: "prefix name does not match", body: digest + "  saige-linux-amd64.exe\n", wantErr: "no entry"},
		{name: "malformed digest", body: "xyz  saige-linux-amd64\n", wantErr: "malformed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := findChecksum(strings.NewReader(tt.body), "saige-linux-amd64")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("got (%q, %v), want %q", got, err, tt.want)
			}
		})
	}
}

// TestInstallScriptSelectsArch runs install.sh in dry-run mode with uname
// stubbed, checking that it picks the asset matching the machine.
func TestInstallScriptSelectsArch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("install.sh targets macOS and Linux")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		os, arch, bin, want string
		wantErr             bool
	}{
		{os: "Linux", arch: "aarch64", want: "saige-linux-arm64"},
		{os: "Linux", arch: "x86_64", want: "saige-linux-amd64"},
		{os: "Darwin", arch: "x86_64", want: "saige-darwin-amd64"},
		{os: "Darwin", arch: "arm64", want: "saige-darwin-arm64"},
		{os: "Linux", arch: "arm64", bin: "saige-mcp", want: "saige-mcp-linux-arm64"},
		{os: "Linux", arch: "armv7l", wantErr: true},
		{os: "Linux", arch: "x86_64", bin: "other", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.os+"-"+tt.arch+"-"+tt.bin, func(t *testing.T) {
			stubDir := t.TempDir()
			stub := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in -m) echo %s ;; *) echo %s ;; esac\n", tt.arch, tt.os)
			if err := os.WriteFile(filepath.Join(stubDir, "uname"), []byte(stub), 0o755); err != nil { //nolint:gosec // test stub must be executable
				t.Fatal(err)
			}
			cmd := exec.Command(bash, script) //nolint:gosec // fixed test script
			cmd.Env = []string{"PATH=" + stubDir + string(os.PathListSeparator) + os.Getenv("PATH"), "HOME=" + t.TempDir(), "DRY_RUN=1"}
			if tt.bin != "" {
				cmd.Env = append(cmd.Env, "BIN="+tt.bin)
			}
			out, err := cmd.CombinedOutput()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected failure, got output %q", out)
				}
				return
			}
			if err != nil {
				t.Fatalf("install.sh: %v\n%s", err, out)
			}
			if got := strings.TrimSpace(string(out)); got != tt.want {
				t.Fatalf("asset = %q, want %q", got, tt.want)
			}
		})
	}
}
