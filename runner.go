package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// maybeStep pairs a Step with its skip decision, used when building
// execution groups before running.
type maybeStep struct {
	step    Step
	skipped bool
}

// RunOptions controls pipeline execution.
type RunOptions struct {
	Dir             string            // working directory for steps
	Branch          string            // branch name for step filtering
	OnlyStep        string            // if set, only run this step by name
	Env             map[string]string // extra env vars merged over pipeline-level env
	Output          io.Writer         // destination for all output (default: os.Stdout)
	NoColor         bool              // disable ANSI colors in logs
	LogFormat       string            // auto, pretty, plain
	PullPolicy      string            // missing, always, never
	SecretEnv       []string          // host env names injected into the run env (and masked)
	SecretMask      []string          // extra literal values to redact from logs
	NoMaskSecrets   bool              // disable log secret masking
	Executor        string            // auto, container, host
	ContainerEngine string            // auto, docker, podman
	ContainerSocket string            // optional unix socket path
	ContainerImage  string            // default image used when step/pipeline does not set one
}

// StepResult holds the outcome of a single step.
type StepResult struct {
	Name           string
	Skipped        bool
	Parallel       bool
	Duration       time.Duration
	Err            error
	FailureIgnored bool
}

// Failed reports whether the step is considered a hard failure.
func (r StepResult) Failed() bool {
	return r.Err != nil && !r.FailureIgnored
}

// HasFailure reports whether any step in results failed.
func HasFailure(results []StepResult) bool {
	for _, r := range results {
		if r.Failed() {
			return true
		}
	}
	return false
}

type plannedStep struct {
	step  Step
	deps  []string
	order int
}

type stepRunnerFunc func(step Step, out io.Writer) error

// RunPipeline executes all steps in p according to opts.
func RunPipeline(p *Pipeline, opts RunOptions) []StepResult {
	if opts.Output == nil {
		opts.Output = os.Stdout
	}
	style := logStyle{
		palette: newLogPalette(detectColorEnabled(opts.Output, opts.NoColor)),
		format:  resolveLogFormat(opts.Output, opts.LogFormat),
	}

	// Merge env: pipeline base, then caller overrides.
	env := make(map[string]string, len(p.Env)+len(opts.Env))
	for k, v := range p.Env {
		env[k] = v
	}
	for k, v := range opts.Env {
		env[k] = v
	}
	// Keep core system paths reachable even when pipeline PATH is overridden.
	hardenPathEnv(env)
	secretEnvNames := append([]string{}, p.Secrets...)
	secretEnvNames = append(secretEnvNames, opts.SecretEnv...)
	if err := injectSecretEnv(env, secretEnvNames); err != nil {
		result := StepResult{Name: "__setup__", Err: err}
		printStepFail(opts.Output, style, "__setup__", 0, err)
		printSummary(opts.Output, style, []StepResult{result})
		return []StepResult{result}
	}
	output := opts.Output
	if !opts.NoMaskSecrets {
		output = wrapWithSecretRedactor(output, env, secretEnvNames, opts.SecretMask)
	}

	canParallel := runtime.NumCPU() > 1
	printHeader(output, style, p.Name, canParallel)

	plan, err := buildExecutionPlan(p.Steps, opts.OnlyStep)
	if err != nil {
		result := StepResult{Name: "__setup__", Err: err}
		printStepFail(output, style, "__setup__", 0, err)
		printSummary(output, style, []StepResult{result})
		return []StepResult{result}
	}

	stepRunner, cleanup, err := prepareStepRunner(p, opts, env, output, style)
	if err != nil {
		result := StepResult{Name: "__setup__", Err: err}
		printStepFail(output, style, "__setup__", 0, err)
		printSummary(output, style, []StepResult{result})
		return []StepResult{result}
	}
	defer cleanup()

	results := executePlan(plan, opts.Branch, canParallel, output, stepRunner, style)
	printSummary(output, style, results)
	return results
}

func buildExecutionPlan(steps []Step, onlyStep string) ([]plannedStep, error) {
	filtered := append([]Step{}, steps...)
	if strings.TrimSpace(onlyStep) != "" {
		filtered = nil
		for _, s := range steps {
			if s.Name == onlyStep {
				filtered = append(filtered, s)
			}
		}
	}
	if len(filtered) == 0 {
		return nil, fmt.Errorf("no steps selected")
	}

	for i := range filtered {
		if strings.TrimSpace(filtered[i].Name) == "" {
			filtered[i].Name = fmt.Sprintf("step-%d", i+1)
		}
	}

	seenNames := make(map[string]struct{}, len(filtered))
	for _, s := range filtered {
		if _, ok := seenNames[s.Name]; ok {
			return nil, fmt.Errorf("duplicate step name %q", s.Name)
		}
		seenNames[s.Name] = struct{}{}
	}

	hasExplicitDeps := false
	for _, s := range filtered {
		if len(s.DependencyNames()) > 0 {
			hasExplicitDeps = true
			break
		}
	}

	legacyDeps := legacyDependenciesForSteps(filtered)
	plan := make([]plannedStep, 0, len(filtered))
	for i, s := range filtered {
		deps := append([]string{}, legacyDeps[s.Name]...)
		if strings.TrimSpace(onlyStep) != "" {
			deps = nil
		} else if hasExplicitDeps {
			if explicit := uniqStrings(s.DependencyNames()); len(explicit) > 0 {
				deps = explicit
			}
		}
		plan = append(plan, plannedStep{step: s, deps: deps, order: i})
	}

	if err := validatePlanDependencies(plan); err != nil {
		return nil, err
	}
	if err := validatePlanAcyclic(plan); err != nil {
		return nil, err
	}

	return plan, nil
}

func legacyDependenciesForSteps(steps []Step) map[string][]string {
	deps := make(map[string][]string, len(steps))
	groups := groupSteps(steps, "")
	prevGroupNames := []string{}
	for _, g := range groups {
		currNames := make([]string, 0, len(g.steps))
		for _, s := range g.steps {
			currNames = append(currNames, s.Name)
		}
		for _, s := range g.steps {
			deps[s.Name] = append([]string{}, prevGroupNames...)
		}
		prevGroupNames = currNames
	}
	return deps
}

func validatePlanDependencies(plan []plannedStep) error {
	names := make(map[string]struct{}, len(plan))
	for _, p := range plan {
		names[p.step.Name] = struct{}{}
	}
	for _, p := range plan {
		for _, dep := range p.deps {
			if dep == p.step.Name {
				return fmt.Errorf("step %q cannot depend on itself", p.step.Name)
			}
			if _, ok := names[dep]; !ok {
				return fmt.Errorf("step %q depends on unknown step %q", p.step.Name, dep)
			}
		}
	}
	return nil
}

func validatePlanAcyclic(plan []plannedStep) error {
	indeg := make(map[string]int, len(plan))
	edges := make(map[string][]string, len(plan))
	for _, p := range plan {
		if _, ok := indeg[p.step.Name]; !ok {
			indeg[p.step.Name] = 0
		}
		for _, dep := range p.deps {
			edges[dep] = append(edges[dep], p.step.Name)
			indeg[p.step.Name]++
		}
	}

	queue := make([]string, 0, len(plan))
	for name, d := range indeg {
		if d == 0 {
			queue = append(queue, name)
		}
	}
	sort.Strings(queue)

	visited := 0
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		visited++
		for _, to := range edges[n] {
			indeg[to]--
			if indeg[to] == 0 {
				queue = append(queue, to)
			}
		}
	}
	if visited != len(plan) {
		return fmt.Errorf("step dependency cycle detected")
	}
	return nil
}

func executePlan(plan []plannedStep, branch string, canParallel bool, out io.Writer, run stepRunnerFunc, style logStyle) []StepResult {
	completed := make(map[string]StepResult, len(plan))
	pipelineFailed := false
	results := make([]StepResult, 0, len(plan))

	for len(completed) < len(plan) {
		ready := findReadySteps(plan, completed)
		if len(ready) == 0 {
			result := StepResult{Name: "__setup__", Err: fmt.Errorf("execution deadlock: unresolved step dependencies")}
			printStepFail(out, style, "__setup__", 0, result.Err)
			results = append(results, result)
			break
		}

		runnable := make([]plannedStep, 0, len(ready))
		for _, ps := range ready {
			if !ps.step.ShouldRun(branch) {
				printSkip(out, style, ps.step.Name, branch)
				r := StepResult{Name: ps.step.Name, Skipped: true}
				completed[r.Name] = r
				results = append(results, r)
				continue
			}
			if !ps.step.ShouldRunForPipelineStatus(pipelineFailed) {
				printSkipForStatus(out, style, ps.step.Name, pipelineFailed)
				r := StepResult{Name: ps.step.Name, Skipped: true}
				completed[r.Name] = r
				results = append(results, r)
				continue
			}
			runnable = append(runnable, ps)
		}
		if len(runnable) == 0 {
			continue
		}

		if !canParallel && len(runnable) > 1 {
			runnable = runnable[:1]
		}

		if len(runnable) == 1 {
			r := runSinglePlannedStep(runnable[0], out, run, style, false)
			completed[r.Name] = r
			results = append(results, r)
			if r.Failed() {
				pipelineFailed = true
			}
			continue
		}

		type parallelOut struct {
			result StepResult
			buf    []byte
		}
		outputs := make([]parallelOut, len(runnable))
		var wg sync.WaitGroup
		for i, ps := range runnable {
			wg.Add(1)
			go func(idx int, item plannedStep) {
				defer wg.Done()
				var buf bytes.Buffer
				outputs[idx] = parallelOut{result: runSinglePlannedStep(item, &buf, run, style, true), buf: buf.Bytes()}
			}(i, ps)
		}
		wg.Wait()

		for _, o := range outputs {
			out.Write(o.buf)
			completed[o.result.Name] = o.result
			results = append(results, o.result)
			if o.result.Failed() {
				pipelineFailed = true
			}
		}
	}

	return results
}

func findReadySteps(plan []plannedStep, completed map[string]StepResult) []plannedStep {
	ready := make([]plannedStep, 0, len(plan))
	for _, ps := range plan {
		if _, ok := completed[ps.step.Name]; ok {
			continue
		}
		depsDone := true
		for _, dep := range ps.deps {
			if _, ok := completed[dep]; !ok {
				depsDone = false
				break
			}
		}
		if depsDone {
			ready = append(ready, ps)
		}
	}
	return ready
}

func runSinglePlannedStep(ps plannedStep, out io.Writer, run stepRunnerFunc, style logStyle, parallel bool) StepResult {
	printStepStart(out, style, ps.step.Name, parallel)
	start := time.Now()
	err := run(ps.step, out)
	dur := time.Since(start)

	result := StepResult{Name: ps.step.Name, Duration: dur, Parallel: parallel, Err: err}
	if err == nil {
		printStepOK(out, style, ps.step.Name, dur)
		return result
	}
	if ps.step.FailureIgnored() {
		result.FailureIgnored = true
		printStepSoftFail(out, style, ps.step.Name, dur, err)
		return result
	}
	printStepFail(out, style, ps.step.Name, dur, err)
	return result
}

func prepareStepRunner(p *Pipeline, opts RunOptions, env map[string]string, out io.Writer, style logStyle) (stepRunnerFunc, func(), error) {
	mode, err := normalizeExecutorMode(opts.Executor)
	if err != nil {
		return nil, func() {}, err
	}
	env["PIPE_EXECUTOR_MODE"] = mode

	pullPolicy, err := normalizePullPolicy(opts.PullPolicy)
	if err != nil {
		return nil, func() {}, err
	}
	if pullPolicy != "" {
		env["PIPE_PULL_POLICY"] = pullPolicy
	}

	var rt *containerRuntime
	if mode != "host" {
		rt, err = detectContainerRuntime(opts.ContainerEngine, opts.ContainerSocket)
		if err != nil {
			if mode == "container" {
				return nil, func() {}, err
			}
			fmt.Fprintf(out, "%s[pipe] container runtime not available, falling back to host: %v%s\n", style.palette.Yellow, err, style.palette.Reset)
		} else {
			env["PIPE_CONTAINER_ENGINE"] = rt.Name
			if rt.SocketPath != "" {
				env["PIPE_CONTAINER_SOCKET"] = rt.SocketPath
			}
			socketLabel := "default context"
			if rt.SocketPath != "" {
				socketLabel = rt.SocketPath
			}
			fmt.Fprintf(out, "%s[pipe] executor=%s engine=%s socket=%s%s\n", style.palette.Gray, mode, rt.Name, socketLabel, style.palette.Reset)
		}
	} else {
		fmt.Fprintf(out, "%s[pipe] executor=host%s\n", style.palette.Gray, style.palette.Reset)
	}

	if len(p.Services) > 0 && rt == nil {
		return nil, func() {}, fmt.Errorf("services require container executor/runtime")
	}

	cleanup := func() {}
	imageCache := newImagePullCache()
	serviceNetwork := ""
	if rt != nil && len(p.Services) > 0 {
		ss, err := startServiceSet(*rt, p.Services, pullPolicy, imageCache, out, style)
		if err != nil {
			return nil, func() {}, err
		}
		serviceNetwork = ss.network
		cleanup = ss.Close
	}

	var hostWarnOnce sync.Once
	return func(step Step, stepOut io.Writer) error {
		script := buildStepScript(step.Run, env)
		if rt != nil {
			image := resolveStepImage(step, p, opts.ContainerImage)
			if image != "" {
				if err := ensureContainerImage(*rt, image, pullPolicy, imageCache, stepOut, style); err != nil {
					return err
				}
				return runStepInContainer(*rt, image, opts.Dir, env, script, stepOut, serviceNetwork)
			}
			if mode == "container" {
				return fmt.Errorf("step %q has no image (set step.image, pipeline image, or --image)", step.Name)
			}
		}

		hostWarnOnce.Do(func() {
			fmt.Fprintf(stepOut, "%s[pipe] host execution is deprecated; prefer container images (pipeline image, step.image, or --image)%s\n", style.palette.Yellow, style.palette.Reset)
		})
		return runStepOnHost(opts.Dir, env, script, stepOut)
	}, cleanup, nil
}

func normalizeExecutorMode(mode string) (string, error) {
	m := strings.ToLower(strings.TrimSpace(mode))
	if m == "" {
		m = "auto"
	}
	switch m {
	case "auto", "container", "host":
		return m, nil
	default:
		return "", fmt.Errorf("invalid executor %q (allowed: auto, container, host)", mode)
	}
}

func normalizePullPolicy(policy string) (string, error) {
	p := strings.ToLower(strings.TrimSpace(policy))
	if p == "" || p == "auto" {
		return "missing", nil
	}
	switch p {
	case "missing", "always", "never":
		return p, nil
	default:
		return "", fmt.Errorf("invalid pull policy %q (allowed: missing, always, never)", policy)
	}
}

func resolveStepImage(step Step, p *Pipeline, defaultImage string) string {
	if img := strings.TrimSpace(step.Image); img != "" {
		return img
	}
	if p != nil {
		if img := strings.TrimSpace(p.Image); img != "" {
			return img
		}
	}
	if img := strings.TrimSpace(defaultImage); img != "" {
		return img
	}
	return ""
}

func runStepOnHost(dir string, env map[string]string, script string, out io.Writer) error {
	shell, err := exec.LookPath("bash")
	if err != nil {
		return fmt.Errorf("bash not found in PATH: %w", err)
	}
	cmd := exec.Command(shell, "-c", script)
	cmd.Dir = dir
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Env = os.Environ()
	for _, kv := range envPairs(env) {
		cmd.Env = append(cmd.Env, kv)
	}
	return cmd.Run()
}

func runStepInContainer(rt containerRuntime, image, dir string, env map[string]string, script string, out io.Writer, network string) error {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve working dir: %w", err)
	}

	args := []string{
		"run",
		"--rm",
		"--workdir", containerWorkspaceDir,
		"--volume", absDir + ":" + containerWorkspaceDir,
	}
	if strings.TrimSpace(network) != "" {
		args = append(args, "--network", network)
	}
	args = appendGitSafeDirectoryEnv(args, env)
	for _, kv := range envPairs(env) {
		args = append(args, "--env", kv)
	}
	args = append(args, image, "bash", "-lc", script)

	cmd := exec.Command(rt.Binary, args...)
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Env = runtimeCommandEnv(rt)
	return cmd.Run()
}

func appendGitSafeDirectoryEnv(args []string, env map[string]string) []string {
	if _, ok := env["GIT_CONFIG_COUNT"]; ok {
		return args
	}
	if _, ok := env["GIT_CONFIG_KEY_0"]; ok {
		return args
	}
	if _, ok := env["GIT_CONFIG_VALUE_0"]; ok {
		return args
	}
	return append(args,
		"--env", "GIT_CONFIG_COUNT=1",
		"--env", "GIT_CONFIG_KEY_0=safe.directory",
		"--env", "GIT_CONFIG_VALUE_0="+containerWorkspaceDir,
	)
}

func envPairs(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, fmt.Sprintf("%s=%s", k, env[k]))
	}
	return out
}

func hardenPathEnv(env map[string]string) {
	path, ok := env["PATH"]
	if !ok {
		return
	}
	env["PATH"] = ensurePathContains(path, linuxSystemPathDirs)
}

var linuxSystemPathDirs = []string{
	"/usr/local/sbin",
	"/usr/local/bin",
	"/usr/sbin",
	"/usr/bin",
	"/sbin",
	"/bin",
}

func ensurePathContains(path string, requiredDirs []string) string {
	seen := make(map[string]struct{})
	for _, part := range strings.Split(path, ":") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		seen[p] = struct{}{}
	}

	var sb strings.Builder
	sb.WriteString(path)
	endsWithColon := strings.HasSuffix(path, ":")
	for _, d := range requiredDirs {
		if _, ok := seen[d]; ok {
			continue
		}
		if sb.Len() > 0 && !endsWithColon {
			sb.WriteByte(':')
		}
		sb.WriteString(d)
		endsWithColon = false
	}
	return sb.String()
}

func buildStepScript(run string, env map[string]string) string {
	var sb strings.Builder
	sb.WriteString("set -euo pipefail\n")
	sb.WriteString(pathBootstrapShellSnippet)
	sb.WriteString("\n")
	if strings.TrimSpace(env["PIPE_ACTIONS_URL"]) != "" {
		sb.WriteString(pipeActionShellFunc)
		sb.WriteString("\n")
	}
	sb.WriteString(run)
	return sb.String()
}

const pathBootstrapShellSnippet = `
if [ -d /usr/local/go/bin ]; then
  export PATH="/usr/local/go/bin:$PATH"
fi
if [ -d /root/.cargo/bin ]; then
  export PATH="/root/.cargo/bin:$PATH"
fi
if [ -d /usr/local/cargo/bin ]; then
  export PATH="/usr/local/cargo/bin:$PATH"
fi
`

const containerWorkspaceDir = "/workspace"

const pipeActionShellFunc = `pipe_action() {
  if [ "$#" -lt 1 ]; then
    echo "pipe_action: usage: pipe_action <path> [args...]" >&2
    return 2
  fi
  if [ -z "${PIPE_ACTIONS_URL:-}" ]; then
    echo "pipe_action: PIPE_ACTIONS_URL is empty" >&2
    return 2
  fi

  local action_path="$1"
  shift || true

  case "$action_path" in
    ""|/*|*..*|*//*)
      echo "pipe_action: invalid action path: $action_path" >&2
      return 2
      ;;
  esac

  local base="${PIPE_ACTIONS_URL%/}"
  local tmp
  tmp="$(mktemp)"
  trap 'rm -f "$tmp"' RETURN

  curl -fsSL "${base}/${action_path}" -o "$tmp"
  chmod +x "$tmp"
  "$tmp" "$@"
}
`

// stripBranch converts "refs/heads/main" -> "main".
func stripBranch(ref string) string {
	return strings.TrimPrefix(ref, "refs/heads/")
}

// -- service + image helpers --------------------------------------------------

type serviceSet struct {
	rt         containerRuntime
	network    string
	containers []string
}

func startServiceSet(rt containerRuntime, services []Service, pullPolicy string, imageCache *imagePullCache, out io.Writer, style logStyle) (*serviceSet, error) {
	network := fmt.Sprintf("pipe-net-%d-%d", os.Getpid(), time.Now().UnixNano())
	if err := runRuntimeCommand(rt, []string{"network", "create", network}, out); err != nil {
		return nil, fmt.Errorf("create service network: %w", err)
	}

	ss := &serviceSet{rt: rt, network: network}
	cleanupOnError := func(err error) (*serviceSet, error) {
		ss.Close()
		return nil, err
	}

	for i, svc := range services {
		image := strings.TrimSpace(svc.Image)
		if image == "" {
			return cleanupOnError(fmt.Errorf("service[%d] has empty image", i))
		}
		if err := ensureContainerImage(rt, image, pullPolicy, imageCache, out, style); err != nil {
			return cleanupOnError(fmt.Errorf("prepare service %q image %q: %w", svc.Name, image, err))
		}
		alias := sanitizeServiceName(svc.Name)
		if alias == "" {
			alias = fmt.Sprintf("service-%d", i+1)
		}
		containerName := truncateContainerName(network + "-" + alias)

		if strings.TrimSpace(svc.Run) != "" && len(svc.Command) > 0 {
			return cleanupOnError(fmt.Errorf("service %q cannot set both run and command", alias))
		}

		args := []string{"run", "-d", "--rm", "--name", containerName, "--network", network, "--network-alias", alias}
		for _, kv := range envPairs(svc.Env) {
			args = append(args, "--env", kv)
		}
		args = append(args, image)
		if cmd := strings.TrimSpace(svc.Run); cmd != "" {
			args = append(args, "sh", "-lc", cmd)
		} else if len(svc.Command) > 0 {
			args = append(args, svc.Command...)
		}

		fmt.Fprintf(out, "%s[pipe] service start name=%s image=%s%s\n", style.palette.Gray, alias, image, style.palette.Reset)
		if err := runRuntimeCommand(rt, args, out); err != nil {
			return cleanupOnError(fmt.Errorf("start service %q: %w", alias, err))
		}
		ss.containers = append(ss.containers, containerName)
	}

	return ss, nil
}

func (s *serviceSet) Close() {
	if s == nil {
		return
	}
	for i := len(s.containers) - 1; i >= 0; i-- {
		_ = runRuntimeCommand(s.rt, []string{"rm", "-f", s.containers[i]}, io.Discard)
	}
	if strings.TrimSpace(s.network) != "" {
		_ = runRuntimeCommand(s.rt, []string{"network", "rm", s.network}, io.Discard)
	}
}

func sanitizeServiceName(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	if s == "" {
		return ""
	}
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	return out
}

func truncateContainerName(name string) string {
	max := 63
	if len(name) <= max {
		return name
	}
	return name[:max]
}

type imagePullCache struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

func newImagePullCache() *imagePullCache {
	return &imagePullCache{seen: make(map[string]struct{})}
}

func (c *imagePullCache) claim(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.seen[key]; ok {
		return false
	}
	c.seen[key] = struct{}{}
	return true
}

func (c *imagePullCache) release(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.seen, key)
}

func ensureContainerImage(rt containerRuntime, image, policy string, cache *imagePullCache, out io.Writer, style logStyle) error {
	if cache == nil {
		cache = newImagePullCache()
	}
	if strings.TrimSpace(image) == "" {
		return nil
	}

	key := policy + "|" + image
	if !cache.claim(key) {
		return nil
	}
	success := false
	defer func() {
		if !success {
			cache.release(key)
		}
	}()

	switch policy {
	case "never":
		if !imageExists(rt, image) {
			return fmt.Errorf("image %q not found locally (pull policy=never)", image)
		}
		success = true
		return nil
	case "missing":
		if imageExists(rt, image) {
			success = true
			return nil
		}
		fmt.Fprintf(out, "%s[pipe] pull image=%s policy=missing%s\n", style.palette.Gray, image, style.palette.Reset)
		if err := runRuntimeCommand(rt, []string{"pull", image}, out); err != nil {
			return fmt.Errorf("pull image %q: %w", image, err)
		}
		success = true
		return nil
	case "always":
		fmt.Fprintf(out, "%s[pipe] pull image=%s policy=always%s\n", style.palette.Gray, image, style.palette.Reset)
		if err := runRuntimeCommand(rt, []string{"pull", image}, out); err != nil {
			return fmt.Errorf("pull image %q: %w", image, err)
		}
		success = true
		return nil
	default:
		success = true
		return nil
	}
}

func imageExists(rt containerRuntime, image string) bool {
	return runRuntimeCommand(rt, []string{"image", "inspect", image}, io.Discard) == nil
}

func runtimeCommandEnv(rt containerRuntime) []string {
	env := os.Environ()
	if rt.HostEnvKey != "" && rt.HostEnvValue != "" {
		env = append(env, fmt.Sprintf("%s=%s", rt.HostEnvKey, rt.HostEnvValue))
	}
	return env
}

func runRuntimeCommand(rt containerRuntime, args []string, out io.Writer) error {
	cmd := exec.Command(rt.Binary, args...)
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Env = runtimeCommandEnv(rt)
	return cmd.Run()
}

func uniqStrings(items []string) []string {
	seen := make(map[string]struct{}, len(items))
	out := make([]string, 0, len(items))
	for _, raw := range items {
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// -- output formatting --------------------------------------------------------

const (
	ansiReset  = "\033[0m"
	ansiGreen  = "\033[32m"
	ansiRed    = "\033[31m"
	ansiYellow = "\033[33m"
	ansiCyan   = "\033[36m"
	ansiGray   = "\033[90m"
	ansiBold   = "\033[1m"
)

type logPalette struct {
	Reset  string
	Green  string
	Red    string
	Yellow string
	Cyan   string
	Gray   string
	Bold   string
}

type logFormat string

const (
	logFormatPretty logFormat = "pretty"
	logFormatPlain  logFormat = "plain"
)

type logStyle struct {
	palette logPalette
	format  logFormat
}

func newLogPalette(enabled bool) logPalette {
	if !enabled {
		return logPalette{}
	}
	return logPalette{
		Reset:  ansiReset,
		Green:  ansiGreen,
		Red:    ansiRed,
		Yellow: ansiYellow,
		Cyan:   ansiCyan,
		Gray:   ansiGray,
		Bold:   ansiBold,
	}
}

func detectColorEnabled(out io.Writer, noColor bool) bool {
	if noColor {
		return false
	}
	if strings.TrimSpace(os.Getenv("NO_COLOR")) != "" {
		return false
	}

	switch strings.ToLower(strings.TrimSpace(os.Getenv("PIPE_COLOR"))) {
	case "1", "true", "always", "on":
		return true
	case "0", "false", "never", "off":
		return false
	}

	if strings.EqualFold(strings.TrimSpace(os.Getenv("TERM")), "dumb") {
		return false
	}

	if f, ok := out.(*os.File); ok {
		return isCharDevice(f)
	}
	return isCharDevice(os.Stdout)
}

func resolveLogFormat(out io.Writer, requested string) logFormat {
	mode := strings.ToLower(strings.TrimSpace(requested))
	if mode == "" || mode == "auto" {
		mode = strings.ToLower(strings.TrimSpace(os.Getenv("PIPE_LOG_FORMAT")))
	}

	switch mode {
	case string(logFormatPretty):
		return logFormatPretty
	case string(logFormatPlain):
		return logFormatPlain
	}

	if f, ok := out.(*os.File); ok && isCharDevice(f) {
		return logFormatPretty
	}
	if isCharDevice(os.Stdout) {
		return logFormatPretty
	}
	return logFormatPlain
}

func isCharDevice(f *os.File) bool {
	if f == nil {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func printHeader(w io.Writer, style logStyle, name string, canParallel bool) {
	cpus := runtime.NumCPU()
	par := "sequential"
	if canParallel {
		par = fmt.Sprintf("parallel ok  cpus=%d", cpus)
	}
	if style.format == logFormatPlain {
		fmt.Fprintf(w, "[pipe] ┌ pipeline=%s mode=%s\n", name, par)
		return
	}

	palette := style.palette
	fmt.Fprintf(w, "\n%s%s╔══ pipe: %s ══╗%s  %s(%s)%s\n\n",
		palette.Bold, palette.Cyan, name, palette.Reset,
		palette.Gray, par, palette.Reset)
}

func printStepStart(w io.Writer, style logStyle, name string, parallel bool) {
	ts := time.Now().Format("15:04:05")
	marker := "▶"
	plainMarker := "->"
	if parallel {
		marker = "⇉"
		plainMarker = "=>"
	}
	if style.format == logFormatPlain {
		fmt.Fprintf(w, "[%s] │ start %s %s\n", ts, plainMarker, name)
		return
	}

	palette := style.palette
	fmt.Fprintf(w, "%s[%s]%s %s%s  %s%s\n",
		palette.Gray, ts, palette.Reset, palette.Cyan, marker, name, palette.Reset)
}

func printStepOK(w io.Writer, style logStyle, name string, d time.Duration) {
	if style.format == logFormatPlain {
		fmt.Fprintf(w, "[%s] │ ok    %s (%s)\n", time.Now().Format("15:04:05"), name, d.Round(time.Millisecond))
		return
	}
	palette := style.palette
	fmt.Fprintf(w, "%s✓  %s%s %s(%s)%s\n",
		palette.Green, name, palette.Reset,
		palette.Gray, d.Round(time.Millisecond), palette.Reset)
}

func printStepSoftFail(w io.Writer, style logStyle, name string, d time.Duration, err error) {
	if style.format == logFormatPlain {
		fmt.Fprintf(w, "[%s] │ warn  %s (%s): %v (ignored)\n", time.Now().Format("15:04:05"), name, d.Round(time.Millisecond), err)
		return
	}
	palette := style.palette
	fmt.Fprintf(w, "%s⚠  %s%s %s(%s): %v (ignored)%s\n",
		palette.Yellow, name, palette.Reset,
		palette.Gray, d.Round(time.Millisecond), err, palette.Reset)
}

func printStepFail(w io.Writer, style logStyle, name string, d time.Duration, err error) {
	if style.format == logFormatPlain {
		fmt.Fprintf(w, "[%s] │ fail  %s (%s): %v\n", time.Now().Format("15:04:05"), name, d.Round(time.Millisecond), err)
		return
	}
	palette := style.palette
	fmt.Fprintf(w, "%s✗  %s%s %s(%s): %v%s\n",
		palette.Red, name, palette.Reset,
		palette.Gray, d.Round(time.Millisecond), err, palette.Reset)
}

func printSkip(w io.Writer, style logStyle, name, branch string) {
	if style.format == logFormatPlain {
		fmt.Fprintf(w, "[%s] │ skip  %s (branch filter: %q)\n", time.Now().Format("15:04:05"), name, branch)
		return
	}
	palette := style.palette
	fmt.Fprintf(w, "%s⊘  %s%s %s(branch filter: %q)%s\n",
		palette.Yellow, name, palette.Reset,
		palette.Gray, branch, palette.Reset)
}

func printSkipForStatus(w io.Writer, style logStyle, name string, failed bool) {
	reason := "runs_on: success"
	if !failed {
		reason = "runs_on: failure"
	}
	if style.format == logFormatPlain {
		fmt.Fprintf(w, "[%s] │ skip  %s (%s)\n", time.Now().Format("15:04:05"), name, reason)
		return
	}
	palette := style.palette
	fmt.Fprintf(w, "%s⊘  %s%s %s(%s)%s\n",
		palette.Yellow, name, palette.Reset,
		palette.Gray, reason, palette.Reset)
}

func printSummary(w io.Writer, style logStyle, results []StepResult) {
	var passed, failed, skipped, ignored int
	var total time.Duration
	for _, r := range results {
		switch {
		case r.Skipped:
			skipped++
		case r.Failed():
			failed++
			total += r.Duration
		case r.FailureIgnored:
			ignored++
			total += r.Duration
		default:
			passed++
			total += r.Duration
		}
	}
	if style.format == logFormatPlain {
		verdict := "PASSED"
		if failed > 0 {
			verdict = "FAILED"
		}
		fmt.Fprintf(w, "[pipe] └ summary status=%s passed=%d failed=%d ignored=%d skipped=%d time=%s\n", verdict, passed, failed, ignored, skipped, total.Round(time.Millisecond))
		return
	}

	palette := style.palette
	verdict := palette.Green + "PASSED" + palette.Reset
	if failed > 0 {
		verdict = palette.Red + "FAILED" + palette.Reset
	}
	fmt.Fprintf(w, "\n%s────────────────────────────────%s\n", palette.Gray, palette.Reset)
	fmt.Fprintf(w, "  %s  %spassed=%d  failed=%d  ignored=%d  skipped=%d  time=%s%s\n\n",
		verdict,
		palette.Gray, passed, failed, ignored, skipped, total.Round(time.Millisecond), palette.Reset)
}
