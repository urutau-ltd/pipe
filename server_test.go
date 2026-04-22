package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSanitizeRepo(t *testing.T) {
	tests := []struct {
		name    string
		repo    string
		wantErr bool
	}{
		{name: "valid", repo: "my-app", wantErr: false},
		{name: "path traversal", repo: "../my-app", wantErr: true},
		{name: "slash", repo: "org/my-app", wantErr: true},
		{name: "empty", repo: "", wantErr: true},
		{name: "spaces", repo: " my-app ", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := sanitizeRepo(tc.repo)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for %q", tc.repo)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.repo, err)
			}
		})
	}
}

func TestValidateRef(t *testing.T) {
	tests := []struct {
		name    string
		ref     string
		wantErr bool
	}{
		{name: "valid main", ref: "refs/heads/main", wantErr: false},
		{name: "valid feature", ref: "refs/heads/feature/x", wantErr: false},
		{name: "tag not allowed", ref: "refs/tags/v1.0.0", wantErr: true},
		{name: "double dot", ref: "refs/heads/feat..x", wantErr: true},
		{name: "space", ref: "refs/heads/feat x", wantErr: true},
		{name: "empty branch", ref: "refs/heads/", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRef(tc.ref)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for %q", tc.ref)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.ref, err)
			}
		})
	}
}

func TestPipelineFileFromSelector(t *testing.T) {
	tests := []struct {
		name     string
		selector string
		want     string
		wantErr  bool
	}{
		{name: "ci short", selector: "ci", want: ".pipe/ci.yml"},
		{name: "release with extension", selector: "release.yml", want: ".pipe/release.yml"},
		{name: "invalid slash", selector: "ops/ci", wantErr: true},
		{name: "invalid ext", selector: "ci.json", wantErr: true},
		{name: "invalid chars", selector: "ci!", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pipelineFileFromSelector(tc.selector)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tc.selector)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.selector, err)
			}
			if got != tc.want {
				t.Fatalf("unexpected pipeline file: got=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestResolveRequestedPipeline(t *testing.T) {
	got, err := resolveRequestedPipeline(".pipe.yml", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != ".pipe.yml" {
		t.Fatalf("unexpected default pipeline: %q", got)
	}

	got, err = resolveRequestedPipeline(".pipe.yml", "nightly")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != ".pipe/nightly.yml" {
		t.Fatalf("unexpected resolved pipeline: %q", got)
	}
}

func TestResolveRequestedPipelines(t *testing.T) {
	t.Run("default pipeline", func(t *testing.T) {
		got, err := resolveRequestedPipelines(".pipe.yml", "", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 || got[0] != ".pipe.yml" {
			t.Fatalf("unexpected pipelines: %#v", got)
		}
	})

	t.Run("single selector", func(t *testing.T) {
		got, err := resolveRequestedPipelines(".pipe.yml", "ci", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 || got[0] != ".pipe/ci.yml" {
			t.Fatalf("unexpected pipelines: %#v", got)
		}
	})

	t.Run("multiple selectors dedupe preserve order", func(t *testing.T) {
		got, err := resolveRequestedPipelines(".pipe.yml", "", []string{"ci", "release.yml", "ci", "nightly"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []string{".pipe/ci.yml", ".pipe/release.yml", ".pipe/nightly.yml"}
		if len(got) != len(want) {
			t.Fatalf("unexpected length: got=%d want=%d (%#v)", len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("unexpected pipelines[%d]: got=%q want=%q", i, got[i], want[i])
			}
		}
	})

	t.Run("reject mixed pipeline and pipelines", func(t *testing.T) {
		_, err := resolveRequestedPipelines(".pipe.yml", "ci", []string{"release"})
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "either pipeline or pipelines") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("reject invalid selector in list", func(t *testing.T) {
		_, err := resolveRequestedPipelines(".pipe.yml", "", []string{"ci", "../oops"})
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("reject too many", func(t *testing.T) {
		selectors := make([]string, maxPipelinesPerRequest+1)
		for i := range selectors {
			selectors[i] = "ci"
		}
		_, err := resolveRequestedPipelines(".pipe.yml", "", selectors)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "too many pipelines") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestShouldNotifyGotify(t *testing.T) {
	cfg := ServerConfig{GotifyEndpoint: "https://gotify.local/message"}

	if !shouldNotifyGotify(cfg, jobStatusOK) {
		t.Fatal("expected ok status to notify in default mode")
	}
	if !shouldNotifyGotify(cfg, jobStatusFail) {
		t.Fatal("expected fail status to notify in default mode")
	}

	cfg.GotifyOn = "fail"

	if !shouldNotifyGotify(cfg, jobStatusFail) {
		t.Fatal("expected fail status to notify in fail mode")
	}
	if shouldNotifyGotify(cfg, jobStatusOK) {
		t.Fatal("did not expect ok status to notify in fail mode")
	}

	cfg.GotifyOn = "all"
	if !shouldNotifyGotify(cfg, jobStatusOK) {
		t.Fatal("expected ok status to notify in all mode")
	}

	cfg.GotifyEndpoint = ""
	if shouldNotifyGotify(cfg, jobStatusFail) {
		t.Fatal("did not expect notify when endpoint is empty")
	}
}

func TestParseRunsListLimit(t *testing.T) {
	if got := parseRunsListLimit(""); got != defaultRunsListLimit {
		t.Fatalf("unexpected default limit: %d", got)
	}
	if got := parseRunsListLimit("abc"); got != defaultRunsListLimit {
		t.Fatalf("unexpected invalid limit fallback: %d", got)
	}
	if got := parseRunsListLimit("-1"); got != defaultRunsListLimit {
		t.Fatalf("unexpected negative limit fallback: %d", got)
	}
	if got := parseRunsListLimit("15"); got != 15 {
		t.Fatalf("unexpected explicit limit: %d", got)
	}
	if got := parseRunsListLimit("9999"); got != maxRunsListLimit {
		t.Fatalf("unexpected capped limit: %d", got)
	}
}

func TestResolveRunLogPath(t *testing.T) {
	logDir := "/tmp/pipe-logs"

	got, err := resolveRunLogPath(logDir, "repo-ci-1.log")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != filepath.Join(logDir, "repo-ci-1.log") {
		t.Fatalf("unexpected log path: %q", got)
	}

	if _, err := resolveRunLogPath(logDir, "../secret.log"); err == nil {
		t.Fatal("expected traversal log name to be rejected")
	}
}

func TestValidateServerRuntime(t *testing.T) {
	if err := validateServerRuntime(ServerConfig{Executor: "host"}); err != nil {
		t.Fatalf("host preflight should pass: %v", err)
	}

	if err := validateServerRuntime(ServerConfig{Executor: "wat"}); err == nil {
		t.Fatal("expected invalid executor mode to fail")
	}
}

func TestParseLabelMap(t *testing.T) {
	got, err := parseLabelMap([]string{"region=mx", "docker=true"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["region"] != "mx" || got["docker"] != "true" {
		t.Fatalf("unexpected labels: %#v", got)
	}
	if _, err := parseLabelMap([]string{"broken"}); err == nil {
		t.Fatal("expected error for invalid label format")
	}
}

func TestPipelineMatchesLabels(t *testing.T) {
	if !pipelineMatchesLabels(map[string]string{"region": "mx"}, map[string]string{"region": "mx"}) {
		t.Fatal("expected exact label match")
	}
	if pipelineMatchesLabels(map[string]string{"region": "mx"}, map[string]string{"region": "us"}) {
		t.Fatal("did not expect label mismatch to pass")
	}
	if !pipelineMatchesLabels(map[string]string{"region": "*"}, map[string]string{"region": "us"}) {
		t.Fatal("expected wildcard requirement to pass")
	}
}

func TestPruneServerLogs(t *testing.T) {
	dir := t.TempDir()
	oldLog := filepath.Join(dir, "old.log")
	newLog := filepath.Join(dir, "new.log")
	if err := os.WriteFile(oldLog, []byte("old"), 0o644); err != nil {
		t.Fatalf("write old log: %v", err)
	}
	if err := os.WriteFile(newLog, []byte("new"), 0o644); err != nil {
		t.Fatalf("write new log: %v", err)
	}
	oldTime := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(oldLog, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes old log: %v", err)
	}

	removed, err := pruneServerLogs(dir, 1, 0)
	if err != nil {
		t.Fatalf("pruneServerLogs returned error: %v", err)
	}
	if removed == 0 {
		t.Fatal("expected at least one log file to be pruned by age")
	}
	if _, err := os.Stat(oldLog); !os.IsNotExist(err) {
		t.Fatalf("expected old log to be removed, stat err=%v", err)
	}

	removed, err = pruneServerLogs(dir, 0, 1)
	if err != nil {
		t.Fatalf("pruneServerLogs count-mode error: %v", err)
	}
	if removed != 0 {
		t.Fatalf("did not expect extra removals, got %d", removed)
	}
}

func TestNotifyGotify(t *testing.T) {
	type gotifyRequest struct {
		Title    string `json:"title"`
		Message  string `json:"message"`
		Priority int    `json:"priority"`
	}

	var reqBody gotifyRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Fatalf("unexpected content type: %q", ct)
		}
		if key := r.Header.Get("X-Gotify-Key"); key != "" {
			t.Fatalf("did not expect auth header, got: %q", key)
		}
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := notifyGotify(ServerConfig{
		GotifyEndpoint: srv.URL,
		GotifyPriority: 7,
		GotifyOn:       "all",
	}, pushPayload{
		Repo: "pipe",
		Ref:  "refs/heads/main",
	}, ".pipe/ci.yml", "main", "abc123", jobStatusOK, "pipeline passed", "pipe-123.log", "run-123")
	if err != nil {
		t.Fatalf("notifyGotify returned error: %v", err)
	}

	if reqBody.Priority != 7 {
		t.Fatalf("unexpected priority: %d", reqBody.Priority)
	}
	if !strings.Contains(reqBody.Title, "pipe OK") {
		t.Fatalf("unexpected title: %q", reqBody.Title)
	}
	if !strings.Contains(reqBody.Message, "repo=pipe") {
		t.Fatalf("unexpected message: %q", reqBody.Message)
	}
}

func TestNotifyGotifyWithToken(t *testing.T) {
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Gotify-Key")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := notifyGotify(ServerConfig{
		GotifyEndpoint: srv.URL,
		GotifyToken:    "secret-token",
		GotifyPriority: 5,
		GotifyOn:       "all",
	}, pushPayload{Repo: "pipe", Ref: "refs/heads/main"}, ".pipe/release.yml", "main", "abc123", jobStatusOK, "pipeline passed", "pipe-123.log", "run-456")
	if err != nil {
		t.Fatalf("notifyGotify returned error: %v", err)
	}
	if gotKey != "secret-token" {
		t.Fatalf("unexpected gotify key header: %q", gotKey)
	}
}

func TestNotifyGotifyErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := notifyGotify(ServerConfig{
		GotifyEndpoint: srv.URL,
		GotifyPriority: 5,
		GotifyOn:       "all",
	}, pushPayload{Repo: "pipe", Ref: "refs/heads/main"}, ".pipe/ci.yml", "main", "abc123", jobStatusFail, "pipeline failed", "pipe-123.log", "run-789")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "gotify returned") {
		t.Fatalf("unexpected error: %v", err)
	}
}
