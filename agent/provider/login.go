package provider

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// AnthropicLogin reports whether the Anthropic SDK can authenticate without
// an API key: ANTHROPIC_AUTH_TOKEN is set, workload identity federation is
// configured, or a profile written by `ant auth login` exists. The adapter
// then leaves the key out and the SDK resolves the credential itself, in
// that order.
//
// The profile is looked up the way the SDK does it: ANTHROPIC_CONFIG_DIR,
// then XDG_CONFIG_HOME, then the home directory, and the profile named by
// ANTHROPIC_PROFILE, the active_config file, or "default". Every variable is
// read through getenv, so an injected environment without a home directory
// never finds the profile of the machine it runs on. Nil uses os.Getenv.
func AnthropicLogin(getenv func(string) string) bool {
	if getenv == nil {
		getenv = os.Getenv
	}
	if getenv("ANTHROPIC_AUTH_TOKEN") != "" {
		return true
	}
	if getenv("ANTHROPIC_FEDERATION_RULE_ID") != "" && getenv("ANTHROPIC_ORGANIZATION_ID") != "" &&
		(getenv("ANTHROPIC_IDENTITY_TOKEN_FILE") != "" || getenv("ANTHROPIC_IDENTITY_TOKEN") != "") {
		return true
	}
	dir := anthropicConfigDir(getenv)
	if dir == "" {
		return false
	}
	profile := getenv("ANTHROPIC_PROFILE")
	if profile == "" {
		if data, err := os.ReadFile(filepath.Join(dir, "active_config")); err == nil { //nolint:gosec // the SDK's own config directory
			profile = strings.TrimSpace(string(data))
		}
	}
	if profile == "" {
		profile = "default"
	}
	if profile != filepath.Base(profile) {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, "configs", profile+".json")) //nolint:gosec // profile is a bare file name, checked above
	return err == nil
}

// anthropicConfigDir is the directory the Anthropic SDK keeps profiles in,
// empty when the environment names no home.
func anthropicConfigDir(getenv func(string) string) string {
	if dir := getenv("ANTHROPIC_CONFIG_DIR"); dir != "" {
		return dir
	}
	if runtime.GOOS == "windows" {
		if appdata := getenv("APPDATA"); appdata != "" {
			return filepath.Join(appdata, "Anthropic")
		}
		return ""
	}
	if xdg := getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "anthropic")
	}
	if home := getenv("HOME"); home != "" {
		return filepath.Join(home, ".config", "anthropic")
	}
	return ""
}
