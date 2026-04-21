package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type containerRuntime struct {
	Name         string
	Binary       string
	SocketPath   string
	HostEnvKey   string
	HostEnvValue string
}

func detectContainerRuntime(engineHint, socketHint string) (*containerRuntime, error) {
	engine := strings.ToLower(strings.TrimSpace(engineHint))
	if engine == "" {
		engine = "auto"
	}
	if engine != "auto" && engine != "docker" && engine != "podman" {
		return nil, fmt.Errorf("invalid engine %q (allowed: auto, docker, podman)", engineHint)
	}

	socketPath, err := normalizeSocketHint(socketHint)
	if err != nil {
		return nil, err
	}

	switch engine {
	case "docker":
		rt, err := detectDockerRuntime(socketPath)
		if err == nil {
			return rt, nil
		}
		if socketPath == "" {
			return detectDockerRuntimeWithoutSocket()
		}
		return nil, err
	case "podman":
		rt, err := detectPodmanRuntime(socketPath)
		if err == nil {
			return rt, nil
		}
		if socketPath == "" {
			return detectPodmanRuntimeWithoutSocket()
		}
		return nil, err
	default:
		return detectAutoRuntime(socketPath)
	}
}

func detectAutoRuntime(socketPath string) (*containerRuntime, error) {
	if socketPath != "" {
		// Heuristic when engine=auto and socket is explicit.
		if strings.Contains(socketPath, "podman") {
			if rt, err := detectPodmanRuntime(socketPath); err == nil {
				return rt, nil
			}
			if rt, err := detectDockerRuntime(socketPath); err == nil {
				return rt, nil
			}
		} else {
			if rt, err := detectDockerRuntime(socketPath); err == nil {
				return rt, nil
			}
			if rt, err := detectPodmanRuntime(socketPath); err == nil {
				return rt, nil
			}
		}
		return nil, fmt.Errorf("no container runtime could use socket %q", socketPath)
	}

	if rt, err := detectDockerRuntime(""); err == nil && rt.SocketPath != "" {
		return rt, nil
	}
	if rt, err := detectPodmanRuntime(""); err == nil && rt.SocketPath != "" {
		return rt, nil
	}

	// Fallback to default CLI context if no socket was found.
	if rt, err := detectDockerRuntimeWithoutSocket(); err == nil {
		return rt, nil
	}
	if rt, err := detectPodmanRuntimeWithoutSocket(); err == nil {
		return rt, nil
	}
	return nil, fmt.Errorf("no docker/podman runtime detected")
}

func detectDockerRuntime(explicitSocket string) (*containerRuntime, error) {
	bin, err := exec.LookPath("docker")
	if err != nil {
		return nil, fmt.Errorf("docker not found in PATH")
	}

	if explicitSocket != "" {
		if err := ensureUnixSocket(explicitSocket); err != nil {
			return nil, err
		}
		return &containerRuntime{
			Name:         "docker",
			Binary:       bin,
			SocketPath:   explicitSocket,
			HostEnvKey:   "DOCKER_HOST",
			HostEnvValue: "unix://" + explicitSocket,
		}, nil
	}

	if envHost := strings.TrimSpace(os.Getenv("DOCKER_HOST")); envHost != "" {
		if p, ok := unixSocketPathFromHost(envHost); ok && isUnixSocket(p) {
			return &containerRuntime{
				Name:         "docker",
				Binary:       bin,
				SocketPath:   p,
				HostEnvKey:   "DOCKER_HOST",
				HostEnvValue: envHost,
			}, nil
		}
	}

	for _, p := range dockerSocketCandidates() {
		if isUnixSocket(p) {
			return &containerRuntime{
				Name:         "docker",
				Binary:       bin,
				SocketPath:   p,
				HostEnvKey:   "DOCKER_HOST",
				HostEnvValue: "unix://" + p,
			}, nil
		}
	}

	return nil, fmt.Errorf("docker socket not found")
}

func detectDockerRuntimeWithoutSocket() (*containerRuntime, error) {
	bin, err := exec.LookPath("docker")
	if err != nil {
		return nil, fmt.Errorf("docker not found in PATH")
	}
	return &containerRuntime{Name: "docker", Binary: bin}, nil
}

func detectPodmanRuntime(explicitSocket string) (*containerRuntime, error) {
	bin, err := exec.LookPath("podman")
	if err != nil {
		return nil, fmt.Errorf("podman not found in PATH")
	}

	if explicitSocket != "" {
		if err := ensureUnixSocket(explicitSocket); err != nil {
			return nil, err
		}
		return &containerRuntime{
			Name:         "podman",
			Binary:       bin,
			SocketPath:   explicitSocket,
			HostEnvKey:   "CONTAINER_HOST",
			HostEnvValue: "unix://" + explicitSocket,
		}, nil
	}

	if envHost := strings.TrimSpace(os.Getenv("CONTAINER_HOST")); envHost != "" {
		if p, ok := unixSocketPathFromHost(envHost); ok && isUnixSocket(p) {
			return &containerRuntime{
				Name:         "podman",
				Binary:       bin,
				SocketPath:   p,
				HostEnvKey:   "CONTAINER_HOST",
				HostEnvValue: envHost,
			}, nil
		}
	}

	for _, p := range podmanSocketCandidates() {
		if isUnixSocket(p) {
			return &containerRuntime{
				Name:       "podman",
				Binary:     bin,
				SocketPath: p,
			}, nil
		}
	}

	return nil, fmt.Errorf("podman socket not found")
}

func detectPodmanRuntimeWithoutSocket() (*containerRuntime, error) {
	bin, err := exec.LookPath("podman")
	if err != nil {
		return nil, fmt.Errorf("podman not found in PATH")
	}
	return &containerRuntime{Name: "podman", Binary: bin}, nil
}

func dockerSocketCandidates() []string {
	return []string{
		"/var/run/docker.sock",
		"/run/docker.sock",
	}
}

func podmanSocketCandidates() []string {
	uniq := make(map[string]struct{})
	var out []string
	add := func(p string) {
		if p == "" {
			return
		}
		if _, ok := uniq[p]; ok {
			return
		}
		uniq[p] = struct{}{}
		out = append(out, p)
	}

	xdg := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR"))
	if xdg != "" {
		add(filepath.Join(xdg, "podman", "podman.sock"))
	}
	if uid := os.Getuid(); uid >= 0 {
		add(fmt.Sprintf("/run/user/%d/podman/podman.sock", uid))
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		add(filepath.Join(home, ".local", "share", "containers", "podman", "podman.sock"))
		add(filepath.Join(home, ".local", "share", "containers", "podman", "machine", "podman.sock"))
	}

	add("/run/podman/podman.sock")
	add("/var/run/podman/podman.sock")
	return out
}

func normalizeSocketHint(socketHint string) (string, error) {
	hint := strings.TrimSpace(socketHint)
	if hint == "" {
		return "", nil
	}
	if strings.HasPrefix(hint, "unix://") {
		hint = strings.TrimPrefix(hint, "unix://")
	}
	if !filepath.IsAbs(hint) {
		return "", fmt.Errorf("socket path must be absolute: %q", socketHint)
	}
	if strings.Contains(hint, "\x00") {
		return "", fmt.Errorf("invalid socket path")
	}
	return hint, nil
}

func ensureUnixSocket(path string) error {
	if !isUnixSocket(path) {
		return fmt.Errorf("socket not found: %s", path)
	}
	return nil
}

func isUnixSocket(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeSocket != 0
}

func unixSocketPathFromHost(host string) (string, bool) {
	h := strings.TrimSpace(host)
	if !strings.HasPrefix(h, "unix://") {
		return "", false
	}
	p := strings.TrimPrefix(h, "unix://")
	if p == "" || !filepath.IsAbs(p) {
		return "", false
	}
	return p, true
}
