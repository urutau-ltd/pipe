package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

const redactedValue = "[REDACTED]"

type redactWriter struct {
	dst      io.Writer
	replacer *strings.Replacer
}

func (w *redactWriter) Write(p []byte) (int, error) {
	masked := w.replacer.Replace(string(p))
	_, err := io.WriteString(w.dst, masked)
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

func injectSecretEnv(env map[string]string, names []string) error {
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			return fmt.Errorf("secret env name is empty")
		}
		if !isValidEnvName(name) {
			return fmt.Errorf("invalid secret env name %q", raw)
		}
		val, ok := os.LookupEnv(name)
		if !ok {
			return fmt.Errorf("missing secret env %q", name)
		}
		env[name] = val
	}
	return nil
}

func wrapWithSecretRedactor(out io.Writer, env map[string]string, secretNames, extraMasks []string) io.Writer {
	values := collectRedactionValues(env, secretNames, extraMasks)
	if len(values) == 0 {
		return out
	}

	pairs := make([]string, 0, len(values)*2)
	for _, v := range values {
		pairs = append(pairs, v, redactedValue)
	}

	return &redactWriter{
		dst:      out,
		replacer: strings.NewReplacer(pairs...),
	}
}

func collectRedactionValues(env map[string]string, secretNames, extraMasks []string) []string {
	seen := make(map[string]struct{})
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" || len(v) < 4 {
			return
		}
		seen[v] = struct{}{}
	}

	for _, v := range extraMasks {
		add(v)
	}

	for _, name := range secretNames {
		if val, ok := env[strings.TrimSpace(name)]; ok {
			add(val)
		}
	}

	for k, v := range env {
		if isSensitiveEnvKey(k) {
			add(v)
		}
	}

	for _, kv := range os.Environ() {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		k := kv[:i]
		v := kv[i+1:]
		if isSensitiveEnvKey(k) {
			add(v)
		}
	}

	values := make([]string, 0, len(seen))
	for v := range seen {
		values = append(values, v)
	}

	sort.Slice(values, func(i, j int) bool {
		if len(values[i]) == len(values[j]) {
			return values[i] < values[j]
		}
		return len(values[i]) > len(values[j])
	})
	return values
}

func isSensitiveEnvKey(key string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	if k == "" {
		return false
	}
	needles := []string{
		"token",
		"secret",
		"password",
		"passwd",
		"api_key",
		"apikey",
		"auth",
		"credential",
		"private_key",
		"ssh_key",
		"cookie",
		"jwt",
	}
	for _, n := range needles {
		if strings.Contains(k, n) {
			return true
		}
	}
	return false
}

func isValidEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if i == 0 {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_') {
				return false
			}
			continue
		}
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_') {
			return false
		}
	}
	return true
}
