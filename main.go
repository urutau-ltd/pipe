package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

const helpText = `pipe — lightweight CI runner for soft-serve and local machines

Commands:
  run     Run a pipeline from a local repository
  server  Start webhook server (receives pushes from soft-serve)

Examples:
  pipe run                        # run all steps in current directory
  pipe run --step build           # run a single step
  pipe run --branch main          # simulate a branch-filtered run
  pipe run --dir /path/to/repo    # run in a specific directory

  pipe server                     # listen on :9000, clone via http://soft-serve:23232
  pipe server --port 8080 --clone ssh://git.example.com:23231

Use 'pipe <command> --help' for all flags.
`

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
		fmt.Println("pipe v0.1.0")
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %q\n\n", cmd)
		fmt.Fprint(os.Stderr, helpText)
		os.Exit(1)
	}
}

// runCommand handles 'pipe run [flags]'
func runCommand(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	file   := fs.String("file", ".pipe.yml", "pipeline file name")
	step   := fs.String("step", "", "run a single named step only")
	dir    := fs.String("dir", ".", "repository directory")
	branch := fs.String("branch", "", "branch name for step filtering (e.g. main)")

	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: pipe run [flags]")
		fmt.Fprintln(os.Stderr)
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	p, err := LoadPipeline(*dir, *file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
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
		Dir:      *dir,
		Branch:   branchVal,
		OnlyStep: *step,
		Output:   os.Stdout,
		Env: map[string]string{
			"PIPE_REPO":   filepath.Base(abs),
			"PIPE_BRANCH": branchVal,
			"PIPE_COMMIT": resolveCommit(*dir),
			"PIPE_REF":    "refs/heads/" + branchVal,
		},
	})

	if HasFailure(results) {
		os.Exit(1)
	}
}

// serverCommand handles 'pipe server [flags]'
func serverCommand(args []string) {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	port    := fs.Int("port", 9000, "listen port")
	clone   := fs.String("clone", "http://soft-serve:23232", "git base URL for cloning repos")
	workdir := fs.String("workdir", "/tmp/pipe", "working directory for clones and logs")
	file    := fs.String("file", ".pipe.yml", "pipeline file name to look for in each repo")

	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: pipe server [flags]")
		fmt.Fprintln(os.Stderr)
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	StartServer(ServerConfig{
		Port:         *port,
		CloneBaseURL: *clone,
		WorkDir:      *workdir,
		PipelineFile: *file,
	})
}
