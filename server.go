package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ServerConfig holds runtime configuration for the webhook server.
type ServerConfig struct {
	Port         int
	CloneBaseURL string
	WorkDir      string
	PipelineFile string
}

type pushPayload struct {
	Repo string `json:"repo"`
	Ref  string `json:"ref"`
	Old  string `json:"old"`
	New  string `json:"new"`
}

type job struct {
	payload pushPayload
	logPath string
}

func StartServer(cfg ServerConfig) {
	if err := os.MkdirAll(filepath.Join(cfg.WorkDir, "logs"), 0o755); err != nil {
		log.Fatalf("pipe: creating workdir: %v", err)
	}

	jobs := make(chan job, 32)
	go func() {
		for j := range jobs {
			processJob(j, cfg)
		}
	}()

	mux := http.NewServeMux()

	mux.HandleFunc("/run", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var p pushPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, fmt.Sprintf("bad JSON: %v", err), http.StatusBadRequest)
			return
		}
		if p.Repo == "" || p.Ref == "" {
			http.Error(w, "missing repo or ref", http.StatusBadRequest)
			return
		}
		logName := fmt.Sprintf("%s-%d.log", p.Repo, time.Now().UnixMilli())
		logPath := filepath.Join(cfg.WorkDir, "logs", logName)
		select {
		case jobs <- job{payload: p, logPath: logPath}:
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprintf(w, "queued  repo=%s ref=%s log=%s\n", p.Repo, p.Ref, logName)
			log.Printf("pipe: queued  repo=%s ref=%s", p.Repo, p.Ref)
		default:
			http.Error(w, "queue full", http.StatusServiceUnavailable)
		}
	})

	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	addr := fmt.Sprintf(":%d", cfg.Port)
	log.Printf("pipe: listening on %s  clone=%s  workdir=%s  cpus=%d",
		addr, cfg.CloneBaseURL, cfg.WorkDir, runtime.NumCPU())
	log.Fatal(http.ListenAndServe(addr, mux))
}

func processJob(j job, cfg ServerConfig) {
	p := j.payload
	branch := stripBranch(p.Ref)
	repoDir := filepath.Join(cfg.WorkDir, p.Repo)
	cloneURL := strings.TrimRight(cfg.CloneBaseURL, "/") + "/" + p.Repo

	lf, err := os.Create(j.logPath)
	if err != nil {
		log.Printf("pipe: cannot create log %s: %v", j.logPath, err)
		lf = os.Stdout
	} else {
		defer lf.Close()
	}

	out := io.MultiWriter(os.Stdout, lf)
	logf := func(format string, a ...any) {
		fmt.Fprintf(out, "[pipe] "+format+"\n", a...)
	}

	logf("triggered  repo=%s ref=%s branch=%s", p.Repo, p.Ref, branch)

	if _, err := os.Stat(repoDir); os.IsNotExist(err) {
		logf("cloning %s", cloneURL)
		if err := gitRun(out, "clone", cloneURL, repoDir); err != nil {
			logf("clone failed: %v", err)
			return
		}
	} else {
		logf("fetching %s", repoDir)
		if err := gitRun(out, "-C", repoDir, "fetch", "--all"); err != nil {
			logf("fetch failed: %v", err)
			return
		}
		if err := gitRun(out, "-C", repoDir, "reset", "--hard",
			"origin/"+branch); err != nil {
			logf("reset failed: %v", err)
			return
		}
	}

	const nullSHA = "0000000000000000000000000000000000000000"
	if p.New != "" && p.New != nullSHA {
		_ = gitRun(out, "-C", repoDir, "checkout", p.New)
	}

	commitSHA := resolveCommit(repoDir)

	pipeline, err := LoadPipeline(repoDir, cfg.PipelineFile)
	if err != nil {
		logf("no pipeline: %v", err)
		return
	}

	results := RunPipeline(pipeline, RunOptions{
		Dir:    repoDir,
		Branch: branch,
		Output: out,
		Env: map[string]string{
			"PIPE_REPO":   p.Repo,
			"PIPE_BRANCH": branch,
			"PIPE_COMMIT": commitSHA,
			"PIPE_REF":    p.Ref,
		},
	})

	if HasFailure(results) {
		logf("FAILED  repo=%s branch=%s", p.Repo, branch)
	} else {
		logf("OK  repo=%s branch=%s", p.Repo, branch)
	}
}

func gitRun(w io.Writer, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Stdout = w
	cmd.Stderr = w
	return cmd.Run()
}
