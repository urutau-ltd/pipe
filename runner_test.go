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
