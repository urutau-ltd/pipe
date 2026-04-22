package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var version = "dev"

const helpText = `pipe — lightweight CI runner for soft-serve and local machines

Commands:
  run     Run a pipeline from a local repository
  server  Start webhook server (receives pushes from soft-serve)

Examples:
  pipe run                        # run all steps in current directory
  pipe run --executor container   # force container execution
  pipe run --step build           # run a single step
  pipe run --branch main          # simulate a branch-filtered run
  pipe run --pipeline ci          # run .pipe/ci.yml
  pipe run --dir /path/to/repo    # run in a specific directory

  pipe server                     # listen on :9000, clone via http://soft-serve:23232
  pipe server --engine docker     # force docker runtime
  pipe server --port 8080 --clone ssh://git.example.com:23231

Use 'pipe <command> --help' for all flags.
`

type stringSliceFlag []string

func (s *stringSliceFlag) String() string {
	if s == nil {
		return ""
	}
	return strings.Join(*s, ",")
}

func (s *stringSliceFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, helpText)
		os.Exit(1)
	}

	cmd, args := os.Args[1], os.Args[2:]

	switch cmd {
	case "run":
		runCommand(args)
	case "server":
		serverCommand(args)
	case "version":
		fmt.Printf("pipe %s\n", version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %q\n\n", cmd)
		fmt.Fprint(os.Stderr, helpText)
		os.Exit(1)
	}
}

// runCommand handles 'pipe run [flags]'
func runCommand(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	file := fs.String("file", ".pipe.yml", "pipeline file name")
	pipeline := fs.String("pipeline", "", "pipeline selector under .pipe/ (e.g. ci -> .pipe/ci.yml)")
	step := fs.String("step", "", "run a single named step only")
	dir := fs.String("dir", ".", "repository directory")
	branch := fs.String("branch", "", "branch name for step filtering (e.g. main)")
	executor := fs.String("executor", "auto", "execution mode: auto, container, host")
	engine := fs.String("engine", "auto", "container engine: auto, docker, podman")
	socket := fs.String("socket", "", "optional absolute unix socket path for container engine")
	image := fs.String("image", "", "default container image fallback (step.image > pipeline image > --image)")
	pullPolicy := fs.String("pull-policy", "missing", "container image pull policy: missing, always, never")
	var secretEnv stringSliceFlag
	fs.Var(&secretEnv, "secret-env", "host env var name to inject and redact from logs (repeatable, suffix ? for optional)")
	var secretMask stringSliceFlag
	fs.Var(&secretMask, "mask", "literal value to redact from logs (repeatable)")
	logFormat := fs.String("log-format", "auto", "log format: auto, pretty, plain")
	noColor := fs.Bool("no-color", false, "disable ANSI colors in logs")
	noMaskSecrets := fs.Bool("no-mask-secrets", false, "disable secret masking in logs")
	isolate := fs.Bool("isolate", true, "run in a temporary isolated workspace")
	keepWorkdir := fs.Bool("keep-workdir", false, "keep temporary workspace after run (requires --isolate)")
	var artifacts stringSliceFlag
	fs.Var(&artifacts, "artifact", "artifact path/pattern to copy back from isolated workspace (repeatable)")

	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: pipe run [flags]")
		fmt.Fprintln(os.Stderr)
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	pipelineFile := *file
	if strings.TrimSpace(*pipeline) != "" {
		if *file != ".pipe.yml" {
			fmt.Fprintln(os.Stderr, "error: use either --file or --pipeline, not both")
			os.Exit(2)
		}
		resolved, err := pipelineFileFromSelector(*pipeline)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: invalid --pipeline: %v\n", err)
			os.Exit(2)
		}
		pipelineFile = resolved
	}

	origDir := *dir
	runDir := origDir
	cleanupWorkspace := func() {}
	if *isolate {
		tmpDir, err := createIsolatedWorkspace(origDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		runDir = tmpDir
		cleanupWorkspace = func() {
			if *keepWorkdir {
				fmt.Fprintf(os.Stderr, "pipe: kept isolated workspace at %s\n", runDir)
				return
			}
			_ = os.RemoveAll(runDir)
		}
	}
	exit := func(code int) {
		cleanupWorkspace()
		os.Exit(code)
	}

	if *keepWorkdir && !*isolate {
		fmt.Fprintln(os.Stderr, "error: --keep-workdir requires --isolate")
		exit(2)
	}

	p, err := LoadPipeline(runDir, pipelineFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		exit(1)
	}

	abs := *dir
	if abs == "." {
		var err error
		abs, err = os.Getwd()
		if err != nil {
			abs = "."
		}
	}
	branchVal := *branch
	if branchVal == "" {
		branchVal = "local"
	}
	results := RunPipeline(p, RunOptions{
		Dir:             runDir,
		Branch:          branchVal,
		OnlyStep:        *step,
		Output:          os.Stdout,
		Executor:        *executor,
		ContainerEngine: *engine,
		ContainerSocket: *socket,
		ContainerImage:  *image,
		PullPolicy:      *pullPolicy,
		LogFormat:       *logFormat,
		SecretEnv:       []string(secretEnv),
		SecretMask:      []string(secretMask),
		NoColor:         *noColor,
		NoMaskSecrets:   *noMaskSecrets,
		Env: map[string]string{
			"PIPE_REPO":     filepath.Base(abs),
			"PIPE_BRANCH":   branchVal,
			"PIPE_COMMIT":   resolveCommit(runDir),
			"PIPE_REF":      "refs/heads/" + branchVal,
			"PIPE_PIPELINE": pipelineFile,
		},
	})

	if HasFailure(results) {
		exit(1)
	}
	if *isolate && len(artifacts) > 0 {
		if err := exportArtifacts(runDir, origDir, []string(artifacts)); err != nil {
			fmt.Fprintf(os.Stderr, "error exporting artifacts: %v\n", err)
			exit(1)
		}
	}
	cleanupWorkspace()
}

// serverCommand handles 'pipe server [flags]'
func serverCommand(args []string) {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	port := fs.Int("port", 9000, "listen port")
	clone := fs.String("clone", "http://soft-serve:23232", "git base URL for cloning repos")
	workdir := fs.String("workdir", "/tmp/pipe", "working directory for clones and logs")
	file := fs.String("file", ".pipe.yml", "pipeline file name to look for in each repo")
	workers := fs.Int("workers", 1, "number of concurrent pipeline workers")
	queueSize := fs.Int("queue-size", 32, "max queued pipeline jobs before /run returns queue full")
	logRetentionDays := fs.Int("log-retention-days", 14, "delete logs older than N days (0 disables age pruning)")
	logRetentionCount := fs.Int("log-retention-count", 2000, "max number of log files to keep (0 disables count pruning)")
	actionsURL := fs.String("actions-url", "", "optional base URL for shared actions (e.g. raw.githubusercontent.com/.../actions)")
	executor := fs.String("executor", "auto", "execution mode: auto, container, host")
	engine := fs.String("engine", "auto", "container engine: auto, docker, podman")
	socket := fs.String("socket", "", "optional absolute unix socket path for container engine")
	image := fs.String("image", "", "default container image fallback for steps without pipeline/step image")
	pullPolicy := fs.String("pull-policy", "missing", "container image pull policy: missing, always, never")
	var labels stringSliceFlag
	fs.Var(&labels, "label", "server label key=value (repeatable)")
	var secretEnv stringSliceFlag
	fs.Var(&secretEnv, "secret-env", "host env var name to inject and redact from logs (repeatable, suffix ? for optional)")
	var secretMask stringSliceFlag
	fs.Var(&secretMask, "mask", "literal value to redact from logs (repeatable)")
	logFormat := fs.String("log-format", "auto", "log format: auto, pretty, plain")
	noColor := fs.Bool("no-color", false, "disable ANSI colors in logs")
	noMaskSecrets := fs.Bool("no-mask-secrets", false, "disable secret masking in logs")
	gotifyEndpoint := fs.String("gotify-endpoint", "", "optional Gotify endpoint (e.g. https://gotify.local/message)")
	gotifyToken := fs.String("gotify-token", "", "optional Gotify app token (sent as X-Gotify-Key)")
	gotifyPriority := fs.Int("gotify-priority", 5, "Gotify priority (when notifications are enabled)")
	gotifyOn := fs.String("gotify-on", "all", "Gotify mode: all or fail")

	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: pipe server [flags]")
		fmt.Fprintln(os.Stderr)
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	on := strings.ToLower(strings.TrimSpace(*gotifyOn))
	if on != "fail" && on != "all" {
		fmt.Fprintf(os.Stderr, "error: invalid --gotify-on value %q (allowed: fail, all)\n", *gotifyOn)
		os.Exit(2)
	}
	labelMap, err := parseLabelMap([]string(labels))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: invalid --label: %v\n", err)
		os.Exit(2)
	}

	StartServer(ServerConfig{
		Port:              *port,
		CloneBaseURL:      *clone,
		WorkDir:           *workdir,
		PipelineFile:      *file,
		Workers:           *workers,
		QueueSize:         *queueSize,
		LogRetentionDays:  *logRetentionDays,
		LogRetentionCount: *logRetentionCount,
		ActionsURL:        *actionsURL,
		Executor:          *executor,
		ContainerEngine:   *engine,
		ContainerSocket:   *socket,
		ContainerImage:    *image,
		PullPolicy:        *pullPolicy,
		Labels:            labelMap,
		LogFormat:         *logFormat,
		NoColor:           *noColor,
		SecretEnv:         []string(secretEnv),
		SecretMask:        []string(secretMask),
		NoMaskSecrets:     *noMaskSecrets,
		GotifyEndpoint:    *gotifyEndpoint,
		GotifyToken:       *gotifyToken,
		GotifyPriority:    *gotifyPriority,
		GotifyOn:          on,
	})
}
