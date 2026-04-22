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
	Name     string            `yaml:"name"`
	Image    string            `yaml:"image"`
	Env      map[string]string `yaml:"env"`
	Labels   map[string]string `yaml:"labels"`
	Secrets  []string          `yaml:"secrets"`
	Services []Service         `yaml:"services"`
	Steps    []Step            `yaml:"steps"`
}

// Service is a sidecar container started before steps and kept running during
// the pipeline.
type Service struct {
	Name    string            `yaml:"name"`
	Image   string            `yaml:"image"`
	Env     map[string]string `yaml:"env"`
	Run     string            `yaml:"run"`
	Command []string          `yaml:"command"`
}

// Step is a single CI step.
type Step struct {
	Name       string   `yaml:"name"`
	Run        string   `yaml:"run"`
	Image      string   `yaml:"image"`
	Parallel   bool     `yaml:"parallel"`   // if true, runs concurrently with adjacent parallel steps
	Branches   []string `yaml:"branches"`   // if set, only runs on these branches
	Needs      []string `yaml:"needs"`      // explicit dependencies
	DependsOn  []string `yaml:"depends_on"` // alias for needs
	Failure    string   `yaml:"failure"`    // "ignore" to continue on step failure
	RunsOn     []string `yaml:"runs_on"`    // success, failure, always
	IgnoreStep bool     `yaml:"ignore"`     // optional explicit skip
}

// ShouldRun reports whether the step should run for the given branch.
// An empty branch argument disables filtering (step always runs).
func (s Step) ShouldRun(branch string) bool {
	if s.IgnoreStep {
		return false
	}
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

// DependencyNames returns explicit step dependencies, accepting both
// "needs" and "depends_on".
func (s Step) DependencyNames() []string {
	if len(s.Needs) == 0 {
		return append([]string{}, s.DependsOn...)
	}
	if len(s.DependsOn) == 0 {
		return append([]string{}, s.Needs...)
	}

	out := append([]string{}, s.Needs...)
	seen := make(map[string]struct{}, len(s.Needs))
	for _, d := range s.Needs {
		seen[d] = struct{}{}
	}
	for _, d := range s.DependsOn {
		if _, ok := seen[d]; ok {
			continue
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	return out
}

// FailureIgnored reports whether a step error should be ignored.
func (s Step) FailureIgnored() bool {
	return strings.EqualFold(strings.TrimSpace(s.Failure), "ignore")
}

// ShouldRunForPipelineStatus evaluates runs_on against current pipeline state.
// Default behavior matches classic CI flow: only run while pipeline is healthy.
func (s Step) ShouldRunForPipelineStatus(pipelineFailed bool) bool {
	if len(s.RunsOn) == 0 {
		return !pipelineFailed
	}

	var allowSuccess, allowFailure bool
	for _, raw := range s.RunsOn {
		mode := strings.ToLower(strings.TrimSpace(raw))
		switch mode {
		case "always":
			allowSuccess = true
			allowFailure = true
		case "success":
			allowSuccess = true
		case "failure", "failed":
			allowFailure = true
		}
	}
	if pipelineFailed {
		return allowFailure
	}
	return allowSuccess
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
