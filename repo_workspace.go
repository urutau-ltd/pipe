package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const nullSHA = "0000000000000000000000000000000000000000"

var gitCommandRunner = gitRun

type repoWorkspaceRequest struct {
	RepoName  string
	CloneURL  string
	Branch    string
	CommitSHA string
	NullSHA   string
	RunID     string
	Logf      func(string, ...any)
}

type preparedWorkspace struct {
	RunDir    string
	CommitSHA string
}

type repoWorkspaceManager struct {
	baseDir string

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func newRepoWorkspaceManager(baseDir string) *repoWorkspaceManager {
	return &repoWorkspaceManager{
		baseDir: baseDir,
		locks:   map[string]*sync.Mutex{},
	}
}

func (m *repoWorkspaceManager) prepareRun(out io.Writer, req repoWorkspaceRequest) (preparedWorkspace, error) {
	unlock := m.lockRepo(req.RepoName)
	defer unlock()

	cacheDir := filepath.Join(m.baseDir, "repos", req.RepoName)
	runDir, err := m.createRunDir(req.RepoName, req.RunID)
	if err != nil {
		return preparedWorkspace{}, fmt.Errorf("create run workspace: %w", err)
	}

	if err := ensureRepoCache(out, cacheDir, req); err != nil {
		_ = os.RemoveAll(runDir)
		return preparedWorkspace{}, err
	}
	if err := copyTree(cacheDir, runDir); err != nil {
		_ = os.RemoveAll(runDir)
		return preparedWorkspace{}, fmt.Errorf("copy run workspace: %w", err)
	}

	return preparedWorkspace{
		RunDir:    runDir,
		CommitSHA: resolveCommit(runDir),
	}, nil
}

func (m *repoWorkspaceManager) createRunDir(repoName, runID string) (string, error) {
	parent := filepath.Join(m.baseDir, "runs", repoName)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", err
	}

	name := strings.TrimSpace(runID)
	if name == "" {
		return os.MkdirTemp(parent, "run-")
	}

	dir := filepath.Join(parent, name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

func (m *repoWorkspaceManager) lockRepo(repoName string) func() {
	m.mu.Lock()
	lock := m.locks[repoName]
	if lock == nil {
		lock = &sync.Mutex{}
		m.locks[repoName] = lock
	}
	m.mu.Unlock()

	lock.Lock()
	return lock.Unlock
}

func ensureRepoCache(out io.Writer, cacheDir string, req repoWorkspaceRequest) error {
	if req.Logf == nil {
		req.Logf = func(string, ...any) {}
	}

	if _, err := os.Stat(cacheDir); os.IsNotExist(err) {
		if err := cloneRepoCache(out, cacheDir, req); err != nil {
			return err
		}
	} else if err != nil {
		return fmt.Errorf("stat failed: %v", err)
	} else {
		req.Logf("fetching %s", cacheDir)
		if err := syncRepoCache(out, cacheDir, req); err != nil {
			return err
		}
	}

	target := targetRevision(req.Branch, req.CommitSHA, req.NullSHA)
	req.Logf("preparing cache=%s rev=%s", cacheDir, target)
	if err := gitCommandRunner(out, "-C", cacheDir, "checkout", "--detach", "--force", target); err != nil {
		return fmt.Errorf("checkout failed: %v", err)
	}
	if err := gitCommandRunner(out, "-C", cacheDir, "reset", "--hard", target); err != nil {
		return fmt.Errorf("reset failed: %v", err)
	}
	if err := gitCommandRunner(out, "-C", cacheDir, "clean", "-fdx"); err != nil {
		return fmt.Errorf("clean failed: %v", err)
	}
	return nil
}

func syncRepoCache(out io.Writer, cacheDir string, req repoWorkspaceRequest) error {
	if err := gitCommandRunner(out, "-C", cacheDir, "fetch", "--all", "--prune"); err == nil {
		return nil
	}

	req.Logf("fetch failed, pruning stale remote refs in %s", cacheDir)
	_ = gitCommandRunner(out, "-C", cacheDir, "remote", "prune", "origin")
	if err := gitCommandRunner(out, "-C", cacheDir, "fetch", "--all", "--prune"); err == nil {
		return nil
	}

	req.Logf("fetch recovery failed, recreating cache %s", cacheDir)
	if err := os.RemoveAll(cacheDir); err != nil {
		return fmt.Errorf("remove broken cache: %w", err)
	}
	return cloneRepoCache(out, cacheDir, req)
}

func cloneRepoCache(out io.Writer, cacheDir string, req repoWorkspaceRequest) error {
	if err := os.MkdirAll(filepath.Dir(cacheDir), 0o755); err != nil {
		return fmt.Errorf("create repo cache root: %w", err)
	}
	req.Logf("cloning %s", req.CloneURL)
	if err := gitCommandRunner(out, "clone", req.CloneURL, cacheDir); err != nil {
		return fmt.Errorf("clone failed: %v", err)
	}
	return nil
}

func targetRevision(branch, commitSHA, nullSHAValue string) string {
	if strings.TrimSpace(commitSHA) != "" && commitSHA != nullSHAValue {
		return commitSHA
	}
	return "origin/" + branch
}
