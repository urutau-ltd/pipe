package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestInjectSecretEnv(t *testing.T) {
	t.Setenv("PIPE_TEST_TOKEN", "super-secret-token")

	env := map[string]string{"PIPE_REPO": "pipe"}
	if err := injectSecretEnv(env, []string{"PIPE_TEST_TOKEN"}); err != nil {
		t.Fatalf("injectSecretEnv returned error: %v", err)
	}
	if env["PIPE_TEST_TOKEN"] != "super-secret-token" {
		t.Fatalf("secret env not injected: %#v", env)
	}
}

func TestInjectSecretEnvMissing(t *testing.T) {
	env := map[string]string{}
	err := injectSecretEnv(env, []string{"PIPE_DOES_NOT_EXIST"})
	if err == nil {
		t.Fatal("expected error for missing secret env")
	}
}

func TestInjectSecretEnvOptionalMissing(t *testing.T) {
	env := map[string]string{}
	if err := injectSecretEnv(env, []string{"PIPE_DOES_NOT_EXIST?"}); err != nil {
		t.Fatalf("did not expect error for optional missing secret env: %v", err)
	}
}

func TestParseSecretEnvRef(t *testing.T) {
	name, optional, err := parseSecretEnvRef("GITHUB_TOKEN?")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "GITHUB_TOKEN" || !optional {
		t.Fatalf("unexpected parse result: name=%q optional=%v", name, optional)
	}
}

func TestWrapWithSecretRedactor(t *testing.T) {
	t.Setenv("PIPE_TEST_PASSWORD", "host-pass-12345")

	env := map[string]string{
		"PIPE_API_TOKEN": "token-abc-123",
		"NORMAL_ENV":     "hello",
	}

	var out bytes.Buffer
	w := wrapWithSecretRedactor(&out, env, []string{"PIPE_API_TOKEN"}, []string{"manual-mask-value"})

	_, err := w.Write([]byte("token=token-abc-123 password=host-pass-12345 other=manual-mask-value ok=hello"))
	if err != nil {
		t.Fatalf("write failed: %v", err)
	}

	got := out.String()
	if strings.Contains(got, "token-abc-123") {
		t.Fatalf("token was not redacted: %q", got)
	}
	if strings.Contains(got, "host-pass-12345") {
		t.Fatalf("host secret was not redacted: %q", got)
	}
	if strings.Contains(got, "manual-mask-value") {
		t.Fatalf("manual mask value was not redacted: %q", got)
	}
	if !strings.Contains(got, redactedValue) {
		t.Fatalf("expected redacted marker in output: %q", got)
	}
}
