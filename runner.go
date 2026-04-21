package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
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
	Dir      string            // working directory for steps
	Branch   string            // branch name for step filtering
	OnlyStep string            // if set, only run this step by name
	Env      map[string]string // extra env vars merged over pipeline-level env
	Output   io.Writer         // destination for all output (default: os.Stdout)
}

// StepResult holds the outcome of a single step.
type StepResult struct {
	Name     string
	Skipped  bool
	Parallel bool
	Duration time.Duration
	Err      error
}

// HasFailure reports whether any step in results failed.
func HasFailure(results []StepResult) bool {
	for _, r := range results {
		if r.Err != nil {
			return true
		}
	}
	return false
}

// RunPipeline executes all steps in p according to opts.
//
// Steps marked parallel:true are grouped with adjacent parallel steps and
// executed concurrently — but only when runtime.NumCPU() > 1. On single-core
// hosts every step runs sequentially regardless of the flag.
//
// Within a parallel group each step's output is buffered and flushed
// atomically in declaration order once all steps in the group finish,
// so logs are never interleaved.
func RunPipeline(p *Pipeline, opts RunOptions) []StepResult {
	if opts.Output == nil {
		opts.Output = os.Stdout
	}

	// Merge env: pipeline base, then caller overrides.
	env := make(map[string]string, len(p.Env)+len(opts.Env))
	for k, v := range p.Env {
		env[k] = v
	}
	for k, v := range opts.Env {
		env[k] = v
	}

	canParallel := runtime.NumCPU() > 1
	groups := groupSteps(p.Steps, opts.OnlyStep)
	var allResults []StepResult

	printHeader(opts.Output, p.Name, canParallel)

	for _, g := range groups {
		var groupResults []StepResult

		// Filter skipped steps out before deciding on parallelism.
		var candidates []maybeStep
		for _, s := range g.steps {
			candidates = append(candidates, maybeStep{
				step:    s,
				skipped: !s.ShouldRun(opts.Branch),
			})
		}

		if g.parallel && canParallel && len(g.steps) > 1 {
			groupResults = runParallelGroup(candidates, opts.Dir, env, opts.Branch, opts.Output)
		} else {
			groupResults = runSequentialGroup(candidates, opts.Dir, env, opts.Branch, opts.Output)
		}

		allResults = append(allResults, groupResults...)

		// Fail-fast: stop processing further groups if any step failed.
		if HasFailure(groupResults) {
			break
		}
	}

	printSummary(opts.Output, allResults)
	return allResults
}

// ── execution helpers ─────────────────────────────────────────────────────────

func runSequentialGroup(items []maybeStep, dir string, env map[string]string, branch string, out io.Writer) []StepResult {
	var results []StepResult
	for _, item := range items {
		if item.skipped {
			printSkip(out, item.step.Name, branch)
			results = append(results, StepResult{Name: item.step.Name, Skipped: true})
			continue
		}
		printStepStart(out, item.step.Name, false)
		start := time.Now()
		err := runStep(item.step, dir, env, out)
		dur := time.Since(start)
		r := StepResult{Name: item.step.Name, Duration: dur, Err: err}
		results = append(results, r)
		if err != nil {
			printStepFail(out, item.step.Name, dur, err)
			break
		}
		printStepOK(out, item.step.Name, dur)
	}
	return results
}

type parallelOut struct {
	result StepResult
	buf    []byte
}

func runParallelGroup(items []maybeStep, dir string, env map[string]string, branch string, out io.Writer) []StepResult {
	outputs := make([]parallelOut, len(items))
	var wg sync.WaitGroup

	for i, item := range items {
		if item.skipped {
			var buf bytes.Buffer
			printSkip(&buf, item.step.Name, branch)
			outputs[i] = parallelOut{
				result: StepResult{Name: item.step.Name, Skipped: true},
				buf:    buf.Bytes(),
			}
			continue
		}

		wg.Add(1)
		go func(idx int, s Step) {
			defer wg.Done()
			var buf bytes.Buffer
			printStepStart(&buf, s.Name, true)
			start := time.Now()
			err := runStep(s, dir, env, &buf)
			dur := time.Since(start)
			if err != nil {
				printStepFail(&buf, s.Name, dur, err)
			} else {
				printStepOK(&buf, s.Name, dur)
			}
			outputs[idx] = parallelOut{
				result: StepResult{Name: s.Name, Parallel: true, Duration: dur, Err: err},
				buf:    buf.Bytes(),
			}
		}(i, item.step)
	}

	wg.Wait()

	// Flush buffered output in declaration order — no interleaving.
	results := make([]StepResult, len(items))
	for i, o := range outputs {
		out.Write(o.buf)
		results[i] = o.result
	}
	return results
}

// runStep executes a step's shell script in dir, streaming output to out.
// set -euo pipefail so any command failure exits the step immediately.
func runStep(s Step, dir string, env map[string]string, out io.Writer) error {
	shell, err := exec.LookPath("bash")
	if err != nil {
		return fmt.Errorf("bash not found in PATH: %w", err)
	}
	cmd := exec.Command(shell, "-c", buildStepScript(s.Run, env))
	cmd.Dir = dir
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}
	return cmd.Run()
}

func buildStepScript(run string, env map[string]string) string {
	var sb strings.Builder
	sb.WriteString("set -euo pipefail\n")
	if strings.TrimSpace(env["PIPE_ACTIONS_URL"]) != "" {
		sb.WriteString(pipeActionShellFunc)
		sb.WriteString("\n")
	}
	sb.WriteString(run)
	return sb.String()
}

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

// stripBranch converts "refs/heads/main" → "main".
func stripBranch(ref string) string {
	return strings.TrimPrefix(ref, "refs/heads/")
}

// ── output formatting ─────────────────────────────────────────────────────────

const (
	ansiReset  = "\033[0m"
	ansiGreen  = "\033[32m"
	ansiRed    = "\033[31m"
	ansiYellow = "\033[33m"
	ansiCyan   = "\033[36m"
	ansiGray   = "\033[90m"
	ansiBold   = "\033[1m"
)

func printHeader(w io.Writer, name string, canParallel bool) {
	cpus := runtime.NumCPU()
	par := "sequential"
	if canParallel {
		par = fmt.Sprintf("parallel ok  cpus=%d", cpus)
	}
	fmt.Fprintf(w, "\n%s%s╔══ pipe: %s ══╗%s  %s(%s)%s\n\n",
		ansiBold, ansiCyan, name, ansiReset,
		ansiGray, par, ansiReset)
}

func printStepStart(w io.Writer, name string, parallel bool) {
	ts := time.Now().Format("15:04:05")
	marker := "▶"
	if parallel {
		marker = "⇉"
	}
	fmt.Fprintf(w, "%s[%s]%s %s%s  %s%s\n",
		ansiGray, ts, ansiReset, ansiCyan, marker, name, ansiReset)
}

func printStepOK(w io.Writer, name string, d time.Duration) {
	fmt.Fprintf(w, "%s✓  %s%s %s(%s)%s\n",
		ansiGreen, name, ansiReset,
		ansiGray, d.Round(time.Millisecond), ansiReset)
}

func printStepFail(w io.Writer, name string, d time.Duration, err error) {
	fmt.Fprintf(w, "%s✗  %s%s %s(%s): %v%s\n",
		ansiRed, name, ansiReset,
		ansiGray, d.Round(time.Millisecond), err, ansiReset)
}

func printSkip(w io.Writer, name, branch string) {
	fmt.Fprintf(w, "%s⊘  %s%s %s(branch filter: %q)%s\n",
		ansiYellow, name, ansiReset,
		ansiGray, branch, ansiReset)
}

func printSummary(w io.Writer, results []StepResult) {
	var passed, failed, skipped int
	var total time.Duration
	for _, r := range results {
		switch {
		case r.Skipped:
			skipped++
		case r.Err != nil:
			failed++
		default:
			passed++
			total += r.Duration
		}
	}
	verdict := ansiGreen + "PASSED" + ansiReset
	if failed > 0 {
		verdict = ansiRed + "FAILED" + ansiReset
	}
	fmt.Fprintf(w, "\n%s────────────────────────────────%s\n", ansiGray, ansiReset)
	fmt.Fprintf(w, "  %s  %spassed=%d  failed=%d  skipped=%d  time=%s%s\n\n",
		verdict,
		ansiGray, passed, failed, skipped, total.Round(time.Millisecond), ansiReset)
}
