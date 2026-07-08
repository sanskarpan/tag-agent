package ciauto

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Case is a single eval case (only fields needed for the offline dry-run plan).
type Case struct {
	ID     string `yaml:"id"`
	Prompt string `yaml:"prompt"`
	Input  string `yaml:"input"`
}

// Suite is a parsed eval suite YAML.
type Suite struct {
	Name  string `yaml:"name"`
	Cases []Case `yaml:"cases"`
}

// LoadSuite reads and validates an eval suite YAML file, mirroring
// eval_framework.load_suite's structural checks (mapping with a non-empty
// 'cases' list). It performs NO model calls.
func LoadSuite(path string) (*Suite, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("suite not found: %s", path)
		}
		return nil, err
	}
	var s Suite
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("suite must be a YAML mapping: %w", err)
	}
	if len(s.Cases) == 0 {
		return nil, fmt.Errorf("suite must have at least one case")
	}
	return &s, nil
}
