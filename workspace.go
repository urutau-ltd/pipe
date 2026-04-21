package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func createIsolatedWorkspace(src string) (string, error) {
	absSrc, err := filepath.Abs(src)
	if err != nil {
		return "", fmt.Errorf("resolve source dir: %w", err)
	}

	tmp, err := os.MkdirTemp("", "pipe-workspace-*")
	if err != nil {
		return "", fmt.Errorf("create temp workspace: %w", err)
	}
	if err := copyTree(absSrc, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return "", fmt.Errorf("copy repository to temp workspace: %w", err)
	}
	return tmp, nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}

		if d.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}

		if d.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}

		return copyFile(path, target, info.Mode().Perm())
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

func exportArtifacts(workspaceDir, targetDir string, artifacts []string) error {
	for _, raw := range artifacts {
		pattern, err := normalizeArtifactPattern(raw)
		if err != nil {
			return err
		}

		matches, err := filepath.Glob(filepath.Join(workspaceDir, pattern))
		if err != nil {
			return fmt.Errorf("invalid artifact pattern %q: %w", raw, err)
		}
		if len(matches) == 0 {
			return fmt.Errorf("artifact pattern %q matched no files", raw)
		}

		for _, m := range matches {
			if err := copyArtifactPath(workspaceDir, targetDir, m); err != nil {
				return err
			}
		}
	}
	return nil
}

func normalizeArtifactPattern(pattern string) (string, error) {
	p := strings.TrimSpace(pattern)
	if p == "" {
		return "", fmt.Errorf("artifact pattern is empty")
	}
	if strings.Contains(p, "\x00") {
		return "", fmt.Errorf("invalid artifact pattern")
	}
	if filepath.IsAbs(p) {
		return "", fmt.Errorf("artifact path must be relative: %q", pattern)
	}
	clean := filepath.Clean(p)
	if clean == "." || strings.HasPrefix(clean, "..") {
		return "", fmt.Errorf("artifact path must stay inside repository: %q", pattern)
	}
	return clean, nil
}

func copyArtifactPath(workspaceDir, targetDir, srcPath string) error {
	rel, err := filepath.Rel(workspaceDir, srcPath)
	if err != nil {
		return err
	}
	if rel == "." || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("artifact path escaped workspace: %q", srcPath)
	}

	dst := filepath.Join(targetDir, rel)
	info, err := os.Lstat(srcPath)
	if err != nil {
		return err
	}

	if info.Mode()&os.ModeSymlink != 0 {
		link, err := os.Readlink(srcPath)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		_ = os.RemoveAll(dst)
		return os.Symlink(link, dst)
	}

	if info.IsDir() {
		return copyTree(srcPath, dst)
	}
	return copyFile(srcPath, dst, info.Mode().Perm())
}
