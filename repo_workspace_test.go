package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRepoWorkspaceManagerPrepareRunCreatesIsolatedCopies(t *testing.T) {
	remoteRoot := t.TempDir()
	remoteRepo := createBareRemoteRepo(t, remoteRoot, "git-cone")
	commitSHA := seedRemoteRepo(t, remoteRepo, map[string]string{
		".pipe/ci.yml": "steps:\n  - name: ci\n    run: echo ci\n",
		"README.md":    "hello\n",
	})

	manager := newRepoWorkspaceManager(t.TempDir())
	req := repoWorkspaceRequest{
		RepoName:  "git-cone",
		CloneURL:  remoteRepo,
		Branch:    "develop",
		CommitSHA: commitSHA,
		NullSHA:   nullSHA,
	}

	first, err := manager.prepareRun(nil, withRunID(req, "run-1"))
	if err != nil {
		t.Fatalf("prepareRun first: %v", err)
	}
	second, err := manager.prepareRun(nil, withRunID(req, "run-2"))
	if err != nil {
		t.Fatalf("prepareRun second: %v", err)
	}

	if first.RunDir == second.RunDir {
		t.Fatal("expected distinct run directories")
	}
	if first.CommitSHA == "" || second.CommitSHA == "" {
		t.Fatal("expected resolved commit SHA")
	}

	mutated := filepath.Join(first.RunDir, "README.md")
	if err := os.WriteFile(mutated, []byte("changed\n"), 0o644); err != nil {
		t.Fatalf("mutate first workspace: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(second.RunDir, "README.md"))
	if err != nil {
		t.Fatalf("read second workspace: %v", err)
	}
	if string(data) != "hello\n" {
		t.Fatalf("expected second workspace to stay isolated, got %q", string(data))
	}
}

func TestProcessJobMissingPipelineIsIgnored(t *testing.T) {
	remoteRoot := t.TempDir()
	remoteRepo := createBareRemoteRepo(t, remoteRoot, "git-cone")
	commitSHA := seedRemoteRepo(t, remoteRepo, map[string]string{
		".pipe/ci.yml": "steps:\n  - name: ci\n    run: echo ci\n",
	})

	workDir := t.TempDir()
	logDir := filepath.Join(workDir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}

	cfg := ServerConfig{
		CloneBaseURL:     remoteRoot,
		WorkDir:          workDir,
		PipelineFile:     ".pipe.yml",
		Executor:         "host",
		PullPolicy:       "missing",
		LogFormat:        "plain",
		workspaceManager: newRepoWorkspaceManager(workDir),
	}
	tracker := newRunTracker(8)
	payload := pushPayload{
		Repo: "git-cone",
		Ref:  "refs/heads/develop",
		New:  commitSHA,
	}
	runID := tracker.enqueue(payload, ".pipe/release.yml", "release.log")

	processJob(job{
		payload:      payload,
		logPath:      filepath.Join(logDir, "release.log"),
		pipelineFile: ".pipe/release.yml",
		runID:        runID,
	}, cfg, tracker)

	rec, ok := tracker.get(runID)
	if !ok {
		t.Fatal("expected run record")
	}
	if rec.Status != string(jobStatusIgnored) {
		t.Fatalf("expected ignored status, got %q (%s)", rec.Status, rec.Detail)
	}
	if !strings.Contains(rec.Detail, "not present at commit") {
		t.Fatalf("unexpected detail: %q", rec.Detail)
	}
}

func TestSyncRepoCacheRecoversWithRemotePrune(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "repos", "git-cone")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("mkdir cache dir: %v", err)
	}

	var calls [][]string
	restore := stubGitCommandRunner(func(_ io.Writer, args ...string) error {
		calls = append(calls, append([]string(nil), args...))
		if reflect.DeepEqual(args, []string{"-C", cacheDir, "fetch", "--all", "--prune"}) && len(calls) == 1 {
			return fmt.Errorf("cannot lock ref")
		}
		return nil
	})
	defer restore()

	err := syncRepoCache(nil, cacheDir, repoWorkspaceRequest{
		RepoName: "git-cone",
		Logf:     func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("syncRepoCache: %v", err)
	}

	want := [][]string{
		{"-C", cacheDir, "fetch", "--all", "--prune"},
		{"-C", cacheDir, "remote", "prune", "origin"},
		{"-C", cacheDir, "fetch", "--all", "--prune"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("unexpected git calls:\n got=%#v\nwant=%#v", calls, want)
	}
}

func TestSyncRepoCacheReclonesAfterRepeatedFetchFailure(t *testing.T) {
	cacheRoot := t.TempDir()
	cacheDir := filepath.Join(cacheRoot, "repos", "git-cone")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("mkdir cache dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "stale.txt"), []byte("stale"), 0o644); err != nil {
		t.Fatalf("write stale marker: %v", err)
	}

	var calls [][]string
	restore := stubGitCommandRunner(func(_ io.Writer, args ...string) error {
		calls = append(calls, append([]string(nil), args...))
		switch {
		case reflect.DeepEqual(args, []string{"-C", cacheDir, "fetch", "--all", "--prune"}):
			return fmt.Errorf("cannot lock ref")
		case reflect.DeepEqual(args, []string{"clone", "/tmp/remote/git-cone", cacheDir}):
			if err := os.MkdirAll(cacheDir, 0o755); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(cacheDir, "fresh.txt"), []byte("fresh"), 0o644)
		default:
			return nil
		}
	})
	defer restore()

	err := syncRepoCache(nil, cacheDir, repoWorkspaceRequest{
		RepoName: "git-cone",
		CloneURL: "/tmp/remote/git-cone",
		Logf:     func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("syncRepoCache: %v", err)
	}

	if _, err := os.Stat(filepath.Join(cacheDir, "stale.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected stale cache contents removed, stat err=%v", err)
	}
	data, err := os.ReadFile(filepath.Join(cacheDir, "fresh.txt"))
	if err != nil {
		t.Fatalf("expected recloned cache marker: %v", err)
	}
	if string(data) != "fresh" {
		t.Fatalf("unexpected recloned cache marker: %q", string(data))
	}

	if len(calls) != 4 {
		t.Fatalf("expected fetch, prune, fetch, clone; got %#v", calls)
	}
}

func withRunID(req repoWorkspaceRequest, runID string) repoWorkspaceRequest {
	req.RunID = runID
	return req
}

func stubGitCommandRunner(fn func(io.Writer, ...string) error) func() {
	prev := gitCommandRunner
	gitCommandRunner = fn
	return func() {
		gitCommandRunner = prev
	}
}

func createBareRemoteRepo(t *testing.T, root, name string) string {
	t.Helper()
	remoteRepo := filepath.Join(root, name)
	runGit(t, root, "init", "--bare", remoteRepo)
	return remoteRepo
}

func seedRemoteRepo(t *testing.T, remoteRepo string, files map[string]string) string {
	t.Helper()
	worktree := t.TempDir()
	runGit(t, worktree, "init")
	runGit(t, worktree, "config", "user.email", "pipe-tests@example.com")
	runGit(t, worktree, "config", "user.name", "pipe tests")
	runGit(t, worktree, "checkout", "-b", "develop")
	for name, body := range files {
		path := filepath.Join(worktree, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	runGit(t, worktree, "add", ".")
	runGit(t, worktree, "commit", "-m", "seed")
	runGit(t, worktree, "remote", "add", "origin", remoteRepo)
	runGit(t, worktree, "push", "-u", "origin", "develop")

	out := runGit(t, worktree, "rev-parse", "HEAD")
	return strings.TrimSpace(out)
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
	}
	return string(out)
}
