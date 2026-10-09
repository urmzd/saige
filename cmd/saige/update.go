package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// releaseAPI is the GitHub endpoint for the latest saige release.
const releaseAPI = "https://api.github.com/repos/urmzd/saige/releases/latest"

// checksumAsset is the release asset listing "<sha256>  <asset name>" lines.
const checksumAsset = "SHA256SUMS"

// goosWindows is the GOOS whose binaries carry an .exe suffix and cannot be
// replaced while running.
const goosWindows = "windows"

func newUpdateCmd() *cobra.Command {
	var check bool
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Self-update saige to the latest release",
		RunE: func(cmd *cobra.Command, args []string) error {
			exe, err := os.Executable()
			if err != nil {
				return fmt.Errorf("finding current executable: %w", err)
			}
			if resolved, err := filepath.EvalSymlinks(exe); err == nil {
				exe = resolved
			}
			u := &updater{
				client: &http.Client{Timeout: 2 * time.Minute},
				apiURL: releaseAPI,
				exe:    exe,
				goos:   runtime.GOOS,
				goarch: runtime.GOARCH,
				out:    cmd.ErrOrStderr(),
			}
			return u.run(cmd.Context(), version, check)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "Only report whether a newer release exists")
	return cmd
}

// assetName returns the release asset name for a binary on one platform.
// The release workflow and install.sh use the same scheme:
// <bin>-<goos>-<goarch>, with ".exe" appended on Windows.
func assetName(bin, goos, goarch string) string {
	name := fmt.Sprintf("%s-%s-%s", bin, goos, goarch)
	if goos == goosWindows {
		name += ".exe"
	}
	return name
}

// updater replaces the running binary with the latest release asset.
type updater struct {
	client *http.Client
	apiURL string
	exe    string // path of the binary to replace
	goos   string
	goarch string
	out    io.Writer
}

type releaseInfo struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func (r *releaseInfo) assetURL(name string) string {
	for _, a := range r.Assets {
		if a.Name == name {
			return a.BrowserDownloadURL
		}
	}
	return ""
}

func (u *updater) run(ctx context.Context, current string, checkOnly bool) error {
	if ctx == nil {
		ctx = context.Background()
	}

	var release releaseInfo
	if err := u.getJSON(ctx, u.apiURL, &release); err != nil {
		return fmt.Errorf("fetching release info: %w", err)
	}

	tag := release.TagName
	if tag == "v"+current || tag == current {
		fmt.Fprintln(u.out, "already up to date")
		return nil
	}
	if checkOnly {
		fmt.Fprintf(u.out, "update available: %s -> %s (run saige update)\n", current, tag)
		return nil
	}

	name := assetName(cliName, u.goos, u.goarch)
	binURL := release.assetURL(name)
	if binURL == "" {
		return fmt.Errorf("no asset named %q found in release %s", name, tag)
	}
	sumsURL := release.assetURL(checksumAsset)
	if sumsURL == "" {
		return fmt.Errorf("release %s has no %s; refusing to install an unverified binary", tag, checksumAsset)
	}

	want, err := u.expectedSum(ctx, sumsURL, name)
	if err != nil {
		return err
	}

	// The temp file lives next to the binary so the final rename stays on one
	// filesystem and is atomic.
	tmp, err := os.CreateTemp(filepath.Dir(u.exe), ".saige-update-*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op after a successful rename

	got, err := u.download(ctx, binURL, tmp)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("downloading update: %w", err)
	}
	if got != want {
		return fmt.Errorf("checksum mismatch for %s: got %s, want %s", name, got, want)
	}

	if err := os.Chmod(tmpName, 0o755); err != nil { //nolint:gosec // executable needs 0755
		return fmt.Errorf("chmod: %w", err)
	}
	if err := replaceExecutable(u.goos, tmpName, u.exe); err != nil {
		return fmt.Errorf("replacing executable: %w", err)
	}

	fmt.Fprintf(u.out, "updated: %s -> %s\n", current, tag)
	return nil
}

// replaceExecutable moves src over dst. Windows refuses to overwrite a
// running executable but allows renaming it, so there the old binary is
// first moved aside to dst+".old" and removed on a best-effort basis.
func replaceExecutable(goos, src, dst string) error {
	if goos != goosWindows {
		return os.Rename(src, dst)
	}
	old := dst + ".old"
	_ = os.Remove(old) // leftover from a previous update
	if err := os.Rename(dst, old); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err != nil {
		_ = os.Rename(old, dst)
		return err
	}
	_ = os.Remove(old) // fails while the old binary is still running
	return nil
}

func (u *updater) get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := u.client.Do(req) //nolint:gosec // URL comes from the release API, not user input
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	return resp, nil
}

func (u *updater) getJSON(ctx context.Context, url string, v any) error {
	resp, err := u.get(ctx, url)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return json.NewDecoder(resp.Body).Decode(v)
}

// download writes url's body to w and returns its hex SHA-256.
func (u *updater) download(ctx context.Context, url string, w io.Writer) (string, error) {
	resp, err := u.get(ctx, url)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(w, h), resp.Body); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// expectedSum fetches the checksum file and returns the digest for name.
func (u *updater) expectedSum(ctx context.Context, url, name string) (string, error) {
	resp, err := u.get(ctx, url)
	if err != nil {
		return "", fmt.Errorf("fetching %s: %w", checksumAsset, err)
	}
	defer func() { _ = resp.Body.Close() }()
	sum, err := findChecksum(resp.Body, name)
	if err != nil {
		return "", fmt.Errorf("%s: %w", checksumAsset, err)
	}
	return sum, nil
}

// findChecksum parses sha256sum output ("<hex>  <name>", or "<hex> *<name>"
// in binary mode) and returns the lowercase digest for name.
func findChecksum(r io.Reader, name string) (string, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		if strings.TrimPrefix(fields[1], "*") == name {
			sum := strings.ToLower(fields[0])
			if _, err := hex.DecodeString(sum); err != nil || len(sum) != sha256.Size*2 {
				return "", fmt.Errorf("malformed digest for %s", name)
			}
			return sum, nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", errors.New("no entry for " + name)
}
