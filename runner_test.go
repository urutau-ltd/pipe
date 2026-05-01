package main

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestBuildStepScript(t *testing.T) {
	t.Run("without actions url", func(t *testing.T) {
		script := buildStepScript("echo hi", map[string]string{})
		if strings.Contains(script, "pipe_action()") {
			t.Fatalf("did not expect pipe_action helper: %q", script)
		}
		if !strings.HasPrefix(script, "set -euo pipefail\n") {
			t.Fatalf("missing shell strict mode prefix: %q", script)
		}
	})

	t.Run("with actions url", func(t *testing.T) {
		script := buildStepScript("pipe_action go/test.sh", map[string]string{
			"PIPE_ACTIONS_URL": "https://example.com/actions",
		})
		if !strings.Contains(script, "pipe_action()") {
			t.Fatalf("expected pipe_action helper in script: %q", script)
		}
		if !strings.Contains(script, "curl -fsSL") {
			t.Fatalf("expected curl fetch in helper: %q", script)
		}
		if !strings.Contains(script, "pipe_action go/test.sh") {
			t.Fatalf("missing step body: %q", script)
		}
	})

	t.Run("with PATH+ prepend", func(t *testing.T) {
		script := buildStepScript("echo hi", map[string]string{
			"PATH+": "/opt/tool/bin:/usr/local/go/bin",
		})
		if !strings.Contains(script, `export PATH='/opt/tool/bin:/usr/local/go/bin'"${PATH:+:$PATH}"`) {
			t.Fatalf("expected PATH+ export in script: %q", script)
		}
	})
}

func TestNormalizeExecutorMode(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "", want: "auto"},
		{in: "auto", want: "auto"},
		{in: "container", want: "container"},
		{in: "host", want: "host"},
		{in: "weird", wantErr: true},
	}
	for _, tc := range tests {
		got, err := normalizeExecutorMode(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("expected error for %q", tc.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("unexpected mode for %q: got=%q want=%q", tc.in, got, tc.want)
		}
	}
}

func TestResolveStepImage(t *testing.T) {
	p := &Pipeline{Image: "pipeline:img"}
	step := Step{}
	if got := resolveStepImage(step, p, ""); got != "pipeline:img" {
		t.Fatalf("expected pipeline image, got %q", got)
	}
	if got := resolveStepImage(step, p, "flag:img"); got != "pipeline:img" {
		t.Fatalf("expected pipeline image to override fallback, got %q", got)
	}
	step.Image = "step:img"
	if got := resolveStepImage(step, p, "flag:img"); got != "step:img" {
		t.Fatalf("expected step image, got %q", got)
	}
	if got := resolveStepImage(Step{}, &Pipeline{}, "flag:img"); got != "flag:img" {
		t.Fatalf("expected fallback image when pipeline image is empty, got %q", got)
	}
}

func TestAppendGitSafeDirectoryEnv(t *testing.T) {
	t.Run("adds safe directory env when not configured", func(t *testing.T) {
		args := appendGitSafeDirectoryEnv([]string{"run"}, map[string]string{})
		want := []string{
			"run",
			"--env", "GIT_CONFIG_COUNT=1",
			"--env", "GIT_CONFIG_KEY_0=safe.directory",
			"--env", "GIT_CONFIG_VALUE_0=" + containerWorkspaceDir,
		}
		if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("unexpected args:\n got: %#v\nwant: %#v", args, want)
		}
	})

	t.Run("respects user git config env override", func(t *testing.T) {
		base := []string{"run"}
		args := appendGitSafeDirectoryEnv(base, map[string]string{
			"GIT_CONFIG_COUNT": "2",
		})
		if strings.Join(args, "\x00") != strings.Join(base, "\x00") {
			t.Fatalf("did not expect automatic git config env when user provides override: %#v", args)
		}
	})
}

func TestEnsurePathContains(t *testing.T) {
	t.Run("appends missing required dirs", func(t *testing.T) {
		got := ensurePathContains("/usr/bin/deno:/usr/bin:/usr/local/bin", []string{"/usr/bin", "/bin"})
		if got != "/usr/bin/deno:/usr/bin:/usr/local/bin:/bin" {
			t.Fatalf("unexpected path: %q", got)
		}
	})

	t.Run("keeps existing dirs without duplicates", func(t *testing.T) {
		got := ensurePathContains("/usr/local/bin:/usr/bin:/bin", []string{"/usr/bin", "/bin"})
		if got != "/usr/local/bin:/usr/bin:/bin" {
			t.Fatalf("unexpected path: %q", got)
		}
	})
}

func TestHardenPathEnv(t *testing.T) {
	env := map[string]string{
		"PATH": "/usr/bin/deno:/usr/bin:/usr/local/bin",
	}

	hardenPathEnv(env)

	path := env["PATH"]
	if !strings.Contains(path, "/bin") {
		t.Fatalf("expected /bin to be present in hardened PATH, got %q", path)
	}
	if !strings.Contains(path, "/usr/sbin") {
		t.Fatalf("expected /usr/sbin to be present in hardened PATH, got %q", path)
	}
}

func TestEnvPairsSkipsReservedKeys(t *testing.T) {
	pairs := envPairs(map[string]string{
		"FOO":   "bar",
		"PATH+": "/opt/tool/bin",
	})
	if len(pairs) != 1 || pairs[0] != "FOO=bar" {
		t.Fatalf("unexpected env pairs: %#v", pairs)
	}
}

func TestDetectColorEnabled(t *testing.T) {
	t.Run("explicit no-color flag wins", func(t *testing.T) {
		t.Setenv("PIPE_COLOR", "always")
		if detectColorEnabled(os.Stdout, true) {
			t.Fatal("expected colors disabled when noColor=true")
		}
	})

	t.Run("PIPE_COLOR overrides tty detection", func(t *testing.T) {
		t.Setenv("PIPE_COLOR", "always")
		t.Setenv("NO_COLOR", "")
		if !detectColorEnabled(nil, false) {
			t.Fatal("expected colors enabled with PIPE_COLOR=always")
		}
	})

	t.Run("NO_COLOR disables colors", func(t *testing.T) {
		t.Setenv("NO_COLOR", "1")
		t.Setenv("PIPE_COLOR", "")
		if detectColorEnabled(os.Stdout, false) {
			t.Fatal("expected colors disabled with NO_COLOR")
		}
	})
}

func TestResolveLogFormat(t *testing.T) {
	t.Run("requested format wins", func(t *testing.T) {
		if got := resolveLogFormat(nil, "plain"); got != logFormatPlain {
			t.Fatalf("expected plain format, got %q", got)
		}
		if got := resolveLogFormat(nil, "pretty"); got != logFormatPretty {
			t.Fatalf("expected pretty format, got %q", got)
		}
	})

	t.Run("env override", func(t *testing.T) {
		t.Setenv("PIPE_LOG_FORMAT", "plain")
		if got := resolveLogFormat(nil, ""); got != logFormatPlain {
			t.Fatalf("expected env plain format, got %q", got)
		}
	})

	t.Run("unknown falls back to auto detection", func(t *testing.T) {
		t.Setenv("PIPE_LOG_FORMAT", "")
		got := resolveLogFormat(nil, "weird")
		if got != logFormatPretty && got != logFormatPlain {
			t.Fatalf("expected known format, got %q", got)
		}
	})
}

func TestNormalizePullPolicy(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "", want: "missing"},
		{in: "auto", want: "missing"},
		{in: "missing", want: "missing"},
		{in: "always", want: "always"},
		{in: "never", want: "never"},
		{in: "weird", wantErr: true},
	}
	for _, tc := range tests {
		got, err := normalizePullPolicy(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("expected error for %q", tc.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("unexpected policy for %q: got=%q want=%q", tc.in, got, tc.want)
		}
	}
}

func TestBuildExecutionPlanLegacyParallel(t *testing.T) {
	steps := []Step{
		{Name: "lint", Parallel: true},
		{Name: "test", Parallel: true},
		{Name: "build"},
	}

	plan, err := buildExecutionPlan(steps, "")
	if err != nil {
		t.Fatalf("buildExecutionPlan returned error: %v", err)
	}
	if len(plan) != 3 {
		t.Fatalf("unexpected plan size: %d", len(plan))
	}

	if len(plan[0].deps) != 0 || len(plan[1].deps) != 0 {
		t.Fatalf("parallel group should not depend on previous steps: %#v", plan)
	}
	deps := strings.Join(plan[2].deps, ",")
	if deps != "lint,test" && deps != "test,lint" {
		t.Fatalf("build should depend on lint+test, got %q", deps)
	}
}

func TestBuildExecutionPlanExplicitDependsOn(t *testing.T) {
	steps := []Step{
		{Name: "build"},
		{Name: "deploy", DependsOn: []string{"build"}},
	}

	plan, err := buildExecutionPlan(steps, "")
	if err != nil {
		t.Fatalf("buildExecutionPlan returned error: %v", err)
	}
	if len(plan) != 2 {
		t.Fatalf("unexpected plan size: %d", len(plan))
	}
	if len(plan[1].deps) != 1 || plan[1].deps[0] != "build" {
		t.Fatalf("unexpected deps for deploy: %#v", plan[1].deps)
	}
}

func TestBuildExecutionPlanMixedExplicitAndLegacy(t *testing.T) {
	steps := []Step{
		{Name: "lint"},
		{Name: "build"},
		{Name: "package", DependsOn: []string{"lint"}},
	}

	plan, err := buildExecutionPlan(steps, "")
	if err != nil {
		t.Fatalf("buildExecutionPlan returned error: %v", err)
	}
	if len(plan) != 3 {
		t.Fatalf("unexpected plan size: %d", len(plan))
	}
	if len(plan[1].deps) != 1 || plan[1].deps[0] != "lint" {
		t.Fatalf("expected legacy dep build->lint, got %#v", plan[1].deps)
	}
	if len(plan[2].deps) != 1 || plan[2].deps[0] != "lint" {
		t.Fatalf("expected explicit dep package->lint, got %#v", plan[2].deps)
	}
}

func TestBuildExecutionPlanOnlyStepIgnoresDependencies(t *testing.T) {
	steps := []Step{
		{Name: "build"},
		{Name: "deploy", DependsOn: []string{"build"}},
	}

	plan, err := buildExecutionPlan(steps, "deploy")
	if err != nil {
		t.Fatalf("buildExecutionPlan returned error: %v", err)
	}
	if len(plan) != 1 {
		t.Fatalf("unexpected plan size: %d", len(plan))
	}
	if plan[0].step.Name != "deploy" {
		t.Fatalf("expected deploy step, got %q", plan[0].step.Name)
	}
	if len(plan[0].deps) != 0 {
		t.Fatalf("expected no deps for --step execution, got %#v", plan[0].deps)
	}
}

func TestExecutePlanFailureFlow(t *testing.T) {
	steps := []Step{
		{Name: "build"},
		{Name: "unit", DependsOn: []string{"build"}},
		{Name: "notify", DependsOn: []string{"build"}, RunsOn: []string{"failure"}},
		{Name: "cleanup", DependsOn: []string{"notify"}, RunsOn: []string{"always"}},
	}
	plan, err := buildExecutionPlan(steps, "")
	if err != nil {
		t.Fatalf("buildExecutionPlan returned error: %v", err)
	}

	run := func(step Step, _ io.Writer) error {
		if step.Name == "build" {
			return errors.New("boom")
		}
		return nil
	}

	results := executePlan(plan, "main", false, io.Discard, run, logStyle{format: logFormatPlain})
	if len(results) != 4 {
		t.Fatalf("unexpected results length: %d", len(results))
	}

	byName := make(map[string]StepResult, len(results))
	for _, r := range results {
		byName[r.Name] = r
	}

	if !byName["build"].Failed() {
		t.Fatal("expected build to be a hard failure")
	}
	if !byName["unit"].Skipped {
		t.Fatal("expected unit step to be skipped after pipeline failure")
	}
	if byName["notify"].Skipped || byName["notify"].Err != nil {
		t.Fatal("expected notify to run successfully on failure")
	}
	if byName["cleanup"].Skipped {
		t.Fatal("expected cleanup to run with runs_on=always")
	}
}
