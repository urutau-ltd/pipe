package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v2"
)

// Pipeline is the parsed representation of a .pipe.yml file.
type Pipeline struct {
	Name  string            `yaml:"name"`
	Env   map[string]string `yaml:"env"`
	Steps []Step            `yaml:"steps"`
}

// Step is a single CI step.
type Step struct {
	Name     string   `yaml:"name"`
	Run      string   `yaml:"run"`
	Parallel bool     `yaml:"parallel"` // if true, runs concurrently with adjacent parallel steps
	Branches []string `yaml:"branches"` // if set, only runs on these branches
}

// ShouldRun reports whether the step should run for the given branch.
// An empty branch argument disables filtering (step always runs).
func (s *Step) ShouldRun(branch string) bool {
	if len(s.Branches) == 0 || branch == "" {
		return true
	}
	for _, b := range s.Branches {
		if b == branch {
			return true
		}
	}
	return false
}

// LoadPipeline reads and parses a pipeline file from dir/filename.
func LoadPipeline(dir, filename string) (*Pipeline, error) {
	path := filepath.Join(dir, filename)

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	var p Pipeline
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	if len(p.Steps) == 0 {
		return nil, fmt.Errorf("%s: no steps defined", path)
	}

	if p.Name == "" {
		p.Name = filepath.Base(dir)
	}

	return &p, nil
}

// stepGroup is a batch of steps that share the same parallel flag.
// Sequential steps form single-element groups.
type stepGroup struct {
	steps    []Step
	parallel bool
}

// groupSteps batches consecutive parallel steps together.
// Sequential steps each get their own single-element group.
// If onlyStep is set, all other steps are dropped before grouping.
func groupSteps(steps []Step, onlyStep string) []stepGroup {
	filtered := steps
	if onlyStep != "" {
		filtered = nil
		for _, s := range steps {
			if s.Name == onlyStep {
				filtered = append(filtered, s)
			}
		}
	}

	var groups []stepGroup
	for _, s := range filtered {
		if len(groups) == 0 {
			groups = append(groups, stepGroup{steps: []Step{s}, parallel: s.Parallel})
			continue
		}
		last := &groups[len(groups)-1]
		if s.Parallel && last.parallel {
			last.steps = append(last.steps, s)
		} else {
			groups = append(groups, stepGroup{steps: []Step{s}, parallel: s.Parallel})
		}
	}
	return groups
}

// resolveCommit returns the short SHA of HEAD in dir, or "dev" if unavailable.
func resolveCommit(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "dev"
	}
	return strings.TrimSpace(string(out))
}
