package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// On-disk comparison format:
//
//	<dir>/
//	  result.json          -- full Comparison
//	  inputs/
//	    000.json           -- Observation dataset fields: id, turn, sample,
//	                          labels, input, ground_truth (no output,
//	                          annotations, or timing), loadable as a
//	                          dataset row
//	  outputs/
//	    base/
//	      000.json         -- ObservationResult from base subject
//	    exp/
//	      000.json         -- ObservationResult from experimental subject
//
// Files with the same number in inputs/, outputs/base/, and outputs/exp/
// hold the same case, matched by its key (see [CaseDiff]) and occurrence. Cases
// are numbered in base order, then exp-only cases in exp order. A case that
// only one arm finished, as after a cancelled run, still gets an inputs/
// file, and the other arm has no file under that number.

// WriteComparison writes a [Comparison] to disk.
func WriteComparison(dir string, result *Comparison) error {
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

	// Write individual inputs and outputs, numbered by case so both arms
	// pair up even when one of them is missing cases.
	cases := newCaseIndex(caseKeyer(result.BaseResults, result.ExpResults))
	baseNums := cases.number(result.BaseResults)
	expNums := cases.number(result.ExpResults)
	inputWritten := make(map[int]bool, len(cases.nums))
	writeArm := func(arm string, results []ObservationResult, nums []int) error {
		for i, r := range results {
			name := fmt.Sprintf("%03d.json", nums[i])
			if !inputWritten[nums[i]] {
				if err := writeJSON(filepath.Join(dir, "inputs", name), inputOnly(r.Observation)); err != nil {
					return err
				}
				inputWritten[nums[i]] = true
			}
			if err := writeJSON(filepath.Join(dir, "outputs", arm, name), r); err != nil {
				return err
			}
		}
		return nil
	}
	if err := writeArm("base", result.BaseResults, baseNums); err != nil {
		return err
	}
	return writeArm("exp", result.ExpResults, expNums)
}

// caseKey identifies one case across arms: the pairing key plus which
// occurrence of it this is, so a dataset that repeats a key still pairs
// the nth occurrence in each arm.
type caseKey struct {
	pairKey
	nth int
}

// caseIndex assigns each case a file number on first sight.
type caseIndex struct {
	key  keyer
	nums map[caseKey]int
}

func newCaseIndex(key keyer) *caseIndex {
	return &caseIndex{key: key, nums: map[caseKey]int{}}
}

// number returns the file number of each result in one arm, assigning new
// numbers to cases not seen before.
func (c *caseIndex) number(results []ObservationResult) []int {
	seen := make(map[pairKey]int, len(results))
	nums := make([]int, len(results))
	for i, r := range results {
		pk := c.key(r.Observation)
		key := caseKey{pairKey: pk, nth: seen[pk]}
		seen[pk]++
		num, ok := c.nums[key]
		if !ok {
			num = len(c.nums)
			c.nums[key] = num
		}
		nums[i] = num
	}
	return nums
}

// inputRow is the on-disk form of an inputs/ file: the dataset fields of an
// [Observation] under the same JSON keys, so it loads back as one, without
// the base arm's output, annotations, or timing.
type inputRow struct {
	ID          string          `json:"id"`
	Turn        int             `json:"turn"`
	Sample      int             `json:"sample,omitempty"`
	Labels      Labels          `json:"labels,omitempty"`
	Input       json.RawMessage `json:"input"`
	GroundTruth json.RawMessage `json:"ground_truth,omitempty"`
}

func inputOnly(obs Observation) inputRow {
	return inputRow{
		ID:          obs.ID,
		Turn:        obs.Turn,
		Sample:      obs.Sample,
		Labels:      obs.Labels,
		Input:       obs.Input,
		GroundTruth: obs.GroundTruth,
	}
}

// ReadComparison reads a [Comparison] from disk.
func ReadComparison(dir string) (*Comparison, error) {
	var result Comparison
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

// writeJSON writes v as indented JSON through a temporary file in the same
// directory, synced and renamed over path, so a crash leaves the previous
// file or the complete new one, never a partial write.
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmpPath, 0o600)
	}
	if err == nil {
		err = os.Rename(tmpPath, path)
	}
	if err != nil {
		_ = os.Remove(tmpPath)
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
