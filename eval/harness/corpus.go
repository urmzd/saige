package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var turnFileRE = regexp.MustCompile(`^turn-(\d+)\.md$`)

// defaultSystemFile is the system prompt file used when no config file is
// present.
const defaultSystemFile = "system.md"

// Config file names of a corpus entry, in lookup order. The second is the
// former name, still read so existing corpora load unchanged.
const (
	scriptConfigFile       = "script.json"
	legacyScriptConfigFile = "experiment.json"
)

// scriptConfig is the optional config file of a corpus entry.
type scriptConfig struct {
	Format  string            `json:"format"`
	Systems map[string]string `json:"systems"`
}

// LoadCorpus loads a corpus directory where each subdirectory is one
// script:
//
//	<dir>/<id>/
//	  script.json       optional: {"format": "...", "systems": {"base": "system.md", ...}}
//	  system.md         default Systems["base"] when script.json is absent
//	  turn-0.md ... turn-N.md
//
// experiment.json is read in place of script.json when only it exists. The
// config file is decoded strictly, so an unknown key fails the load, and two
// files for the same turn index (turn-1.md and turn-01.md) are rejected.
// Turns are sorted numerically and turn-0 must exist. Format defaults to
// "text/markdown". System values in the config file are file paths
// relative to the script directory. Scripts are returned sorted by ID.
func LoadCorpus(dir string) ([]Script, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var scripts []Script
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		script, err := loadScript(filepath.Join(dir, entry.Name()), entry.Name())
		if err != nil {
			return nil, err
		}
		scripts = append(scripts, script)
	}
	if len(scripts) == 0 {
		return nil, fmt.Errorf("no scripts found in %s", dir)
	}
	sort.Slice(scripts, func(i, j int) bool { return scripts[i].ID < scripts[j].ID })
	return scripts, nil
}

// readScriptConfig returns the content and name of the first config file
// present in dir, or os.ErrNotExist when there is none.
func readScriptConfig(dir string) ([]byte, string, error) {
	for _, name := range []string{scriptConfigFile, legacyScriptConfigFile} {
		data, err := os.ReadFile(filepath.Clean(filepath.Join(dir, name)))
		if err == nil {
			return data, name, nil
		}
		if !os.IsNotExist(err) {
			return nil, name, err
		}
	}
	return nil, "", os.ErrNotExist
}

func loadScript(dir, id string) (Script, error) {
	config := scriptConfig{Systems: map[string]string{baseFlowName: defaultSystemFile}}
	configData, configName, err := readScriptConfig(dir)
	switch {
	case err == nil:
		config = scriptConfig{}
		if err := decodeScriptConfig(configData, &config); err != nil {
			return Script{}, fmt.Errorf("%s: parse %s: %w", id, configName, err)
		}
		if len(config.Systems) == 0 {
			config.Systems = map[string]string{baseFlowName: defaultSystemFile}
		}
	case os.IsNotExist(err):
	default:
		return Script{}, err
	}
	if config.Format == "" {
		config.Format = formatMarkdown
	}

	systems := make(map[string]string, len(config.Systems))
	for name, rel := range config.Systems {
		data, err := os.ReadFile(filepath.Clean(filepath.Join(dir, rel)))
		if err != nil {
			return Script{}, fmt.Errorf("%s: read system %q: %w", id, name, err)
		}
		systems[name] = string(data)
	}

	turns, err := loadTurns(dir, id)
	if err != nil {
		return Script{}, err
	}

	return Script{
		ID:      id,
		Format:  config.Format,
		Dir:     dir,
		Systems: systems,
		Turns:   turns,
	}, nil
}

// decodeScriptConfig decodes a script config strictly: an unknown key is an
// error, so a misspelled setting is not ignored.
func decodeScriptConfig(data []byte, config *scriptConfig) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(config); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("unexpected data after the JSON object")
	}
	return nil
}

func loadTurns(dir, id string) ([]Turn, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var turns []Turn
	for _, entry := range entries {
		match := turnFileRE.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		index, err := strconv.Atoi(match[1])
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Clean(filepath.Join(dir, entry.Name())))
		if err != nil {
			return nil, fmt.Errorf("%s: read %s: %w", id, entry.Name(), err)
		}
		turns = append(turns, Turn{Index: index, Prompt: string(data)})
	}
	sort.Slice(turns, func(i, j int) bool { return turns[i].Index < turns[j].Index })
	for i := 1; i < len(turns); i++ {
		if turns[i].Index == turns[i-1].Index {
			return nil, fmt.Errorf("%s: two files define turn %d", id, turns[i].Index)
		}
	}
	if len(turns) == 0 || turns[0].Index != 0 {
		return nil, fmt.Errorf("%s: missing turn-0.md", id)
	}
	return turns, nil
}

// FilterScripts keeps scripts whose ID starts with idPrefix (all when
// empty) and truncates the result to count entries when count > 0.
func FilterScripts(scripts []Script, idPrefix string, count int) []Script {
	var filtered []Script
	for _, script := range scripts {
		if idPrefix == "" || strings.HasPrefix(script.ID, idPrefix) {
			filtered = append(filtered, script)
		}
	}
	if count > 0 && len(filtered) > count {
		filtered = filtered[:count]
	}
	return filtered
}

// FilterExperiments keeps scripts whose ID starts with idPrefix.
//
// Deprecated: Use [FilterScripts].
func FilterExperiments(scripts []Script, idPrefix string, count int) []Script {
	return FilterScripts(scripts, idPrefix, count)
}

// ValidateCorpus checks a corpus directory without running it and returns
// every problem found, not only the first: unreadable or unknown config
// keys (with their JSON pointer), missing system files, a missing turn-0,
// duplicate turn indices, and, as warnings, gaps in the turn numbering and
// an empty corpus filter. Issues carry the path of the file at fault.
func ValidateCorpus(dir string) []Issue {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []Issue{{File: dir, Severity: SeverityError, Message: err.Error()}}
	}
	var issues []Issue
	scripts := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		scripts++
		issues = append(issues, validateScript(filepath.Join(dir, entry.Name()))...)
	}
	if scripts == 0 {
		issues = append(issues, Issue{File: dir, Severity: SeverityError, Message: "no scripts: each subdirectory is one script with at least turn-0.md"})
	}
	return issues
}

func validateScript(dir string) []Issue {
	var issues []Issue
	config := scriptConfig{Systems: map[string]string{baseFlowName: defaultSystemFile}}
	configFile := ""
	data, name, err := readScriptConfig(dir)
	switch {
	case err == nil:
		file := filepath.Join(dir, name)
		configFile = file
		config = scriptConfig{}
		found, ok := decodeStrict(data, &config)
		issues = append(issues, withFile(file, found)...)
		if !ok {
			return issues
		}
		if name == legacyScriptConfigFile {
			issues = append(issues, Issue{File: file, Severity: SeverityWarning, Message: "experiment.json is the former name; rename it to script.json"})
		}
		if len(config.Systems) == 0 {
			config.Systems = map[string]string{baseFlowName: defaultSystemFile}
		}
		if _, ok := config.Systems[baseFlowName]; !ok {
			issues = append(issues, Issue{File: file, Pointer: "/systems", Severity: SeverityWarning, Message: `no "base" system prompt; the built-in flows send an empty system prompt`})
		}
	case os.IsNotExist(err):
	default:
		return append(issues, Issue{File: filepath.Join(dir, name), Severity: SeverityError, Message: err.Error()})
	}
	for sysName, rel := range config.Systems {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			issue := Issue{File: filepath.Join(dir, rel), Severity: SeverityError, Message: fmt.Sprintf("system %q: %v", sysName, err)}
			if configFile != "" {
				issue.File, issue.Pointer = configFile, "/systems/"+escapePointer(sysName)
			}
			issues = append(issues, issue)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return append(issues, Issue{File: dir, Severity: SeverityError, Message: err.Error()})
	}
	byIndex := map[int][]string{}
	for _, entry := range entries {
		match := turnFileRE.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		index, err := strconv.Atoi(match[1])
		if err != nil {
			continue
		}
		byIndex[index] = append(byIndex[index], entry.Name())
	}
	indices := make([]int, 0, len(byIndex))
	for index, files := range byIndex {
		indices = append(indices, index)
		if len(files) > 1 {
			sort.Strings(files)
			issues = append(issues, Issue{File: dir, Severity: SeverityError, Message: fmt.Sprintf("turn %d is defined by %s", index, strings.Join(files, " and "))})
		}
	}
	sort.Ints(indices)
	if len(indices) == 0 || indices[0] != 0 {
		issues = append(issues, Issue{File: dir, Severity: SeverityError, Message: "missing turn-0.md"})
	}
	for i := 1; i < len(indices); i++ {
		if indices[i] != indices[i-1]+1 {
			issues = append(issues, Issue{File: dir, Severity: SeverityWarning, Message: fmt.Sprintf("turns jump from %d to %d; turns run in numeric order, so check for a missing file", indices[i-1], indices[i])})
		}
	}
	return issues
}
