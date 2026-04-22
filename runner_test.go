package main

import (
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
	if got := resolveStepImage(step, p, "flag:img"); got != "flag:img" {
		t.Fatalf("expected flag image, got %q", got)
	}
	step.Image = "step:img"
	if got := resolveStepImage(step, p, "flag:img"); got != "step:img" {
		t.Fatalf("expected step image, got %q", got)
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
