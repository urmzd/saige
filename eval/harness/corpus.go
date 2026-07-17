package harness

import (
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

// defaultSystemFile is the system prompt file used when experiment.json is absent.
const defaultSystemFile = "system.md"

// experimentConfig is the optional experiment.json file of a corpus entry.
type experimentConfig struct {
	Format  string            `json:"format"`
	Systems map[string]string `json:"systems"`
}

// LoadCorpus loads a corpus directory where each subdirectory is one
// experiment:
//
//	<dir>/<id>/
//	  experiment.json   optional: {"format": "...", "systems": {"base": "system.md", ...}}
//	  system.md         default Systems["base"] when experiment.json is absent
//	  turn-0.md ... turn-N.md
//
// Turns are sorted numerically and turn-0 must exist. Format defaults to
// "text/markdown". System values in experiment.json are file paths relative
// to the experiment directory. Experiments are returned sorted by ID.
func LoadCorpus(dir string) ([]Experiment, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var experiments []Experiment
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		exp, err := loadExperiment(filepath.Join(dir, entry.Name()), entry.Name())
		if err != nil {
			return nil, err
		}
		experiments = append(experiments, exp)
	}
	if len(experiments) == 0 {
		return nil, fmt.Errorf("no experiments found in %s", dir)
	}
	sort.Slice(experiments, func(i, j int) bool { return experiments[i].ID < experiments[j].ID })
	return experiments, nil
}

func loadExperiment(dir, id string) (Experiment, error) {
	config := experimentConfig{Systems: map[string]string{baseFlowName: defaultSystemFile}}
	configData, err := os.ReadFile(filepath.Clean(filepath.Join(dir, "experiment.json")))
	switch {
	case err == nil:
		config = experimentConfig{}
		if err := json.Unmarshal(configData, &config); err != nil {
			return Experiment{}, fmt.Errorf("%s: parse experiment.json: %w", id, err)
		}
		if len(config.Systems) == 0 {
			config.Systems = map[string]string{baseFlowName: defaultSystemFile}
		}
	case os.IsNotExist(err):
	default:
		return Experiment{}, err
	}
	if config.Format == "" {
		config.Format = formatMarkdown
	}

	systems := make(map[string]string, len(config.Systems))
	for name, rel := range config.Systems {
		data, err := os.ReadFile(filepath.Clean(filepath.Join(dir, rel)))
		if err != nil {
			return Experiment{}, fmt.Errorf("%s: read system %q: %w", id, name, err)
		}
		systems[name] = string(data)
	}

	turns, err := loadTurns(dir, id)
	if err != nil {
		return Experiment{}, err
	}

	return Experiment{
		ID:      id,
		Format:  config.Format,
		Dir:     dir,
		Systems: systems,
		Turns:   turns,
	}, nil
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
	if len(turns) == 0 || turns[0].Index != 0 {
		return nil, fmt.Errorf("%s: missing turn-0.md", id)
	}
	return turns, nil
}

// FilterExperiments keeps experiments whose ID starts with idPrefix (all
// when empty) and truncates the result to count entries when count > 0.
func FilterExperiments(experiments []Experiment, idPrefix string, count int) []Experiment {
	var filtered []Experiment
	for _, exp := range experiments {
		if idPrefix == "" || strings.HasPrefix(exp.ID, idPrefix) {
			filtered = append(filtered, exp)
		}
	}
	if count > 0 && len(filtered) > count {
		filtered = filtered[:count]
	}
	return filtered
}
