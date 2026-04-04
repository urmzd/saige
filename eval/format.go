package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// On-disk experiment format:
//
//	<dir>/
//	  result.json          -- full ExperimentResult
//	  inputs/
//	    000.json           -- Observation inputs
//	  outputs/
//	    base/
//	      000.json         -- ObservationResult from base subject
//	    exp/
//	      000.json         -- ObservationResult from experimental subject

// WriteExperiment writes an [ExperimentResult] to disk.
func WriteExperiment(dir string, result *ExperimentResult) error {
	dirs := []string{
		filepath.Join(dir, "inputs"),
		filepath.Join(dir, "outputs", "base"),
		filepath.Join(dir, "outputs", "exp"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	// Write full result.
	if err := writeJSON(filepath.Join(dir, "result.json"), result); err != nil {
		return err
	}

	// Write individual inputs and outputs.
	for i, r := range result.BaseResults {
		name := fmt.Sprintf("%03d.json", i)
		if err := writeJSON(filepath.Join(dir, "inputs", name), r.Observation); err != nil {
			return err
		}
		if err := writeJSON(filepath.Join(dir, "outputs", "base", name), r); err != nil {
			return err
		}
	}
	for i, r := range result.ExpResults {
		name := fmt.Sprintf("%03d.json", i)
		if err := writeJSON(filepath.Join(dir, "outputs", "exp", name), r); err != nil {
			return err
		}
	}

	return nil
}

// ReadExperiment reads an [ExperimentResult] from disk.
func ReadExperiment(dir string) (*ExperimentResult, error) {
	var result ExperimentResult
	if err := readJSON(filepath.Join(dir, "result.json"), &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// WriteSuiteResult writes a [SuiteResult] to a single JSON file.
func WriteSuiteResult(path string, result *SuiteResult) error {
	return writeJSON(path, result)
}

// ReadSuiteResult reads a [SuiteResult] from a JSON file.
func ReadSuiteResult(path string) (*SuiteResult, error) {
	var result SuiteResult
	if err := readJSON(path, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("unmarshal %s: %w", path, err)
	}
	return nil
}
