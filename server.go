package main

import (
	"bytes"
	"context"
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

	aile "codeberg.org/urutau-ltd/aile/v2"
)

const (
	runBodyLimitBytes       = 64 << 10
	defaultJobQueueSize     = 32
	maxPipelinesPerRequest  = 16
	serverReadTimeout       = 10 * time.Second
	serverReadHeaderTimeout = 5 * time.Second
	serverWriteTimeout      = 30 * time.Second
	serverIdleTimeout       = 60 * time.Second
	serverMaxHeaderBytes    = 1 << 20
	gitCommandTimeout       = 5 * time.Minute
	gotifyRequestTimeout    = 5 * time.Second
)

// ServerConfig holds runtime configuration for the webhook server.
type ServerConfig struct {
	Port           int
	CloneBaseURL   string
	WorkDir        string
	PipelineFile   string
	ActionsURL     string
	GotifyEndpoint string
	GotifyToken    string
	GotifyPriority int
	GotifyOn       string
}

type pushPayload struct {
	Repo      string   `json:"repo"`
	Ref       string   `json:"ref"`
	Old       string   `json:"old"`
	New       string   `json:"new"`
	Pipeline  string   `json:"pipeline,omitempty"`
	Pipelines []string `json:"pipelines,omitempty"`
}

type job struct {
	payload      pushPayload
	logPath      string
	pipelineFile string
}

type jobResultStatus string

const (
	jobStatusOK      jobResultStatus = "ok"
	jobStatusFail    jobResultStatus = "fail"
	jobStatusIgnored jobResultStatus = "ignored"
)

func StartServer(cfg ServerConfig) {
	if cfg.GotifyPriority == 0 {
		cfg.GotifyPriority = 5
	}
	if cfg.GotifyOn == "" {
		cfg.GotifyOn = "all"
	}

	if err := os.MkdirAll(filepath.Join(cfg.WorkDir, "logs"), 0o755); err != nil {
		log.Fatalf("pipe: creating workdir: %v", err)
	}

	jobs := make(chan job, defaultJobQueueSize)
	go func() {
		for j := range jobs {
			processJob(j, cfg)
		}
	}()

	addr := fmt.Sprintf(":%d", cfg.Port)
	app, err := aile.New(
		aile.WithConfig(aile.Config{
			Addr:              addr,
			ReadTimeout:       serverReadTimeout,
			ReadHeaderTimeout: serverReadHeaderTimeout,
			WriteTimeout:      serverWriteTimeout,
			IdleTimeout:       serverIdleTimeout,
			ShutdownTimeout:   serverWriteTimeout,
			MaxHeaderBytes:    serverMaxHeaderBytes,
		}),
	)
	if err != nil {
		log.Fatalf("pipe: creating server: %v", err)
	}
	app.Use(aile.Recovery())

	app.POST("/run", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, runBodyLimitBytes)
		defer r.Body.Close()

		var p pushPayload
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&p); err != nil {
			http.Error(w, fmt.Sprintf("bad JSON: %v", err), http.StatusBadRequest)
			return
		}
		if err := dec.Decode(&struct{}{}); err != io.EOF {
			http.Error(w, "bad JSON: trailing content", http.StatusBadRequest)
			return
		}

		if p.Repo == "" || p.Ref == "" {
			http.Error(w, "missing repo or ref", http.StatusBadRequest)
			return
		}

		repoName, err := sanitizeRepo(p.Repo)
		if err != nil {
			http.Error(w, fmt.Sprintf("invalid repo: %v", err), http.StatusBadRequest)
			log.Printf("pipe: rejected invalid repo %q", p.Repo)
			return
		}
		if err := validateRef(p.Ref); err != nil {
			http.Error(w, fmt.Sprintf("invalid ref: %v", err), http.StatusBadRequest)
			log.Printf("pipe: rejected invalid ref %q", p.Ref)
			return
		}
		pipelineFiles, err := resolveRequestedPipelines(cfg.PipelineFile, p.Pipeline, p.Pipelines)
		if err != nil {
			http.Error(w, fmt.Sprintf("invalid pipeline: %v", err), http.StatusBadRequest)
			log.Printf("pipe: rejected invalid pipeline payload repo=%q pipeline=%q pipelines=%v", p.Repo, p.Pipeline, p.Pipelines)
			return
		}

		p.Repo = repoName
		var queued, requested int
		var logNames []string
		requested = len(pipelineFiles)

		for idx, pipelineFile := range pipelineFiles {
			logName := buildPipelineLogName(repoName, pipelineFile, idx)
			logPath := filepath.Join(cfg.WorkDir, "logs", logName)
			select {
			case jobs <- job{payload: p, logPath: logPath, pipelineFile: pipelineFile}:
				queued++
				logNames = append(logNames, logName)
				log.Printf("pipe: queued  repo=%s ref=%s pipeline=%s", p.Repo, p.Ref, pipelineFile)
			default:
				if queued == 0 {
					http.Error(w, "queue full", http.StatusServiceUnavailable)
					return
				}
				w.WriteHeader(http.StatusAccepted)
				fmt.Fprintf(w, "partially queued  repo=%s ref=%s queued=%d requested=%d logs=%s\n",
					p.Repo, p.Ref, queued, requested, strings.Join(logNames, ","))
				return
			}
		}

		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintf(w, "queued  repo=%s ref=%s pipelines=%d logs=%s\n",
			p.Repo, p.Ref, queued, strings.Join(logNames, ","))
	})

	app.GET("/health", func(w http.ResponseWriter, _ *http.Request) {
		aile.Text(w, http.StatusOK, "ok\n")
	})
	mode := cfg.GotifyOn
	if cfg.GotifyEndpoint == "" {
		mode = "off"
	}
	log.Printf("pipe: listening on %s  clone=%s  workdir=%s  cpus=%d  gotify=%s",
		addr, cfg.CloneBaseURL, cfg.WorkDir, runtime.NumCPU(), mode)
	log.Fatal(app.Run(context.Background()))
}

func processJob(j job, cfg ServerConfig) {
	p := j.payload
	branch := stripBranch(p.Ref)
	repoName, err := sanitizeRepo(p.Repo)
	if err != nil {
		log.Printf("pipe: invalid repo: %v", err)
		return
	}
	logName := filepath.Base(j.logPath)

	status := jobStatusFail
	detail := "internal error"
	commitSHA := p.New
	pipelineFile := j.pipelineFile
	if pipelineFile == "" {
		pipelineFile = cfg.PipelineFile
	}
	actionsURL := strings.TrimSpace(cfg.ActionsURL)
	defer func() {
		if err := notifyGotify(cfg, p, pipelineFile, branch, commitSHA, status, detail, logName); err != nil {
			log.Printf("pipe: gotify notify failed: %v", err)
		}
	}()

	repoDir := filepath.Join(cfg.WorkDir, repoName)
	cloneURL := strings.TrimRight(cfg.CloneBaseURL, "/") + "/" + repoName
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

	logf("triggered  repo=%s ref=%s branch=%s pipeline=%s", p.Repo, p.Ref, branch, pipelineFile)

	if _, err := os.Stat(repoDir); os.IsNotExist(err) {
		logf("cloning %s", cloneURL)
		if err := gitRun(out, "clone", cloneURL, repoDir); err != nil {
			detail = fmt.Sprintf("clone failed: %v", err)
			logf("%s", detail)
			return
		}
	} else if err != nil {
		detail = fmt.Sprintf("stat failed: %v", err)
		logf("%s", detail)
		return
	} else {
		logf("fetching %s", repoDir)
		if err := gitRun(out, "-C", repoDir, "fetch", "--all"); err != nil {
			detail = fmt.Sprintf("fetch failed: %v", err)
			logf("%s", detail)
			return
		}
		if err := gitRun(out, "-C", repoDir, "reset", "--hard", "origin/"+branch); err != nil {
			detail = fmt.Sprintf("reset failed: %v", err)
			logf("%s", detail)
			return
		}
	}

	const nullSHA = "0000000000000000000000000000000000000000"
	if p.New != "" && p.New != nullSHA {
		if err := gitRun(out, "-C", repoDir, "checkout", p.New); err != nil {
			detail = fmt.Sprintf("checkout failed: %v", err)
			logf("%s", detail)
			return
		}
	}

	commitSHA = resolveCommit(repoDir)
	if actionsURL != "" {
		logf("actions  url=%s", actionsURL)
	}

	pipeline, err := LoadPipeline(repoDir, pipelineFile)
	if err != nil {
		status = jobStatusIgnored
		detail = fmt.Sprintf("no pipeline: %v", err)
		logf("%s", detail)
		return
	}

	envMap := map[string]string{
		"PIPE_REPO":     p.Repo,
		"PIPE_BRANCH":   branch,
		"PIPE_COMMIT":   commitSHA,
		"PIPE_REF":      p.Ref,
		"PIPE_PIPELINE": pipelineFile,
	}
	if actionsURL != "" {
		envMap["PIPE_ACTIONS_URL"] = actionsURL
	}

	results := RunPipeline(pipeline, RunOptions{
		Dir:    repoDir,
		Branch: branch,
		Output: out,
		Env:    envMap,
	})

	if HasFailure(results) {
		status = jobStatusFail
		detail = "pipeline failed"
		logf("FAILED  repo=%s branch=%s", p.Repo, branch)
		return
	}

	status = jobStatusOK
	detail = "pipeline passed"
	logf("OK  repo=%s branch=%s", p.Repo, branch)
}

func notifyGotify(cfg ServerConfig, p pushPayload, pipelineFile, branch, commit string, status jobResultStatus, detail, logName string) error {
	if !shouldNotifyGotify(cfg, status) {
		return nil
	}
	if commit == "" {
		commit = "unknown"
	}

	payload := map[string]any{
		"title": fmt.Sprintf("pipe %s %s@%s", strings.ToUpper(string(status)), p.Repo, branch),
		"message": fmt.Sprintf(
			"repo=%s\nbranch=%s\npipeline=%s\nref=%s\ncommit=%s\nstatus=%s\ndetail=%s\nlog=%s",
			p.Repo, branch, pipelineFile, p.Ref, commit, status, detail, logName,
		),
		"priority": cfg.GotifyPriority,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal gotify payload: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, cfg.GotifyEndpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build gotify request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token := strings.TrimSpace(cfg.GotifyToken); token != "" {
		req.Header.Set("X-Gotify-Key", token)
	}

	client := &http.Client{Timeout: gotifyRequestTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("send gotify request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("gotify returned %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}

	return nil
}

func shouldNotifyGotify(cfg ServerConfig, status jobResultStatus) bool {
	if strings.TrimSpace(cfg.GotifyEndpoint) == "" {
		return false
	}

	switch strings.ToLower(strings.TrimSpace(cfg.GotifyOn)) {
	case "fail":
		return status == jobStatusFail
	default:
		return true
	}
}

func gitRun(w io.Writer, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), gitCommandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Stdout = w
	cmd.Stderr = w
	return cmd.Run()
}

// sanitizeRepo protects against path traversal and weird repo names.
func sanitizeRepo(repo string) (string, error) {
	if repo == "" || repo != strings.TrimSpace(repo) {
		return "", fmt.Errorf("invalid repo name: %q", repo)
	}

	clean := filepath.Base(repo)
	if clean == "." || clean == ".." || clean == "" {
		return "", fmt.Errorf("invalid repo name: %q", repo)
	}
	if clean != repo {
		return "", fmt.Errorf("repo must not include path separators: %q", repo)
	}
	if strings.ContainsAny(clean, "/\\\x00") {
		return "", fmt.Errorf("invalid characters in repo name: %q", repo)
	}
	if strings.Contains(clean, "..") {
		return "", fmt.Errorf("invalid repo name: %q", repo)
	}
	return clean, nil
}

// validateRef allows only regular branch refs.
func validateRef(ref string) error {
	const prefix = "refs/heads/"
	if !strings.HasPrefix(ref, prefix) {
		return fmt.Errorf("only refs/heads/* are supported")
	}

	branch := stripBranch(ref)
	if branch == "" {
		return fmt.Errorf("branch is empty")
	}
	if strings.HasPrefix(branch, "/") || strings.HasSuffix(branch, "/") {
		return fmt.Errorf("invalid branch name")
	}
	if strings.Contains(branch, "//") || strings.Contains(branch, "..") || strings.Contains(branch, "@{") {
		return fmt.Errorf("invalid branch name")
	}
	if strings.HasPrefix(branch, ".") || strings.HasSuffix(branch, ".") || strings.HasSuffix(branch, ".lock") {
		return fmt.Errorf("invalid branch name")
	}
	if strings.ContainsAny(branch, " \t\n\r~^:?*[]\\\x00") {
		return fmt.Errorf("invalid branch name")
	}

	return nil
}

func resolveRequestedPipeline(defaultFile, selector string) (string, error) {
	if strings.TrimSpace(selector) == "" {
		return defaultFile, nil
	}
	return pipelineFileFromSelector(selector)
}

func resolveRequestedPipelines(defaultFile, selector string, selectors []string) ([]string, error) {
	if strings.TrimSpace(selector) != "" && len(selectors) > 0 {
		return nil, fmt.Errorf("use either pipeline or pipelines, not both")
	}

	if len(selectors) > 0 {
		if len(selectors) > maxPipelinesPerRequest {
			return nil, fmt.Errorf("too many pipelines requested (max %d)", maxPipelinesPerRequest)
		}
		out := make([]string, 0, len(selectors))
		seen := make(map[string]struct{}, len(selectors))
		for _, sel := range selectors {
			pf, err := pipelineFileFromSelector(sel)
			if err != nil {
				return nil, err
			}
			if _, ok := seen[pf]; ok {
				continue
			}
			seen[pf] = struct{}{}
			out = append(out, pf)
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("pipelines list is empty")
		}
		return out, nil
	}

	pf, err := resolveRequestedPipeline(defaultFile, selector)
	if err != nil {
		return nil, err
	}
	return []string{pf}, nil
}

func buildPipelineLogName(repoName, pipelineFile string, idx int) string {
	label := strings.TrimSuffix(filepath.Base(pipelineFile), filepath.Ext(pipelineFile))
	if label == "" {
		label = "pipeline"
	}
	return fmt.Sprintf("%s-%s-%d-%d.log", repoName, label, time.Now().UnixNano(), idx)
}

func pipelineFileFromSelector(selector string) (string, error) {
	name := strings.TrimSpace(selector)
	if name == "" {
		return "", fmt.Errorf("pipeline selector is empty")
	}
	if strings.ContainsAny(name, "/\\\x00") || strings.Contains(name, "..") {
		return "", fmt.Errorf("pipeline selector must be a plain name")
	}
	if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") {
		return "", fmt.Errorf("invalid pipeline selector %q", selector)
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			continue
		}
		if r == '-' || r == '_' || r == '.' {
			continue
		}
		return "", fmt.Errorf("invalid character %q in pipeline selector", r)
	}

	ext := filepath.Ext(name)
	switch ext {
	case "":
		return filepath.Join(".pipe", name+".yml"), nil
	case ".yml", ".yaml":
		base := strings.TrimSuffix(name, ext)
		if base == "" {
			return "", fmt.Errorf("invalid pipeline selector %q", selector)
		}
		return filepath.Join(".pipe", name), nil
	default:
		return "", fmt.Errorf("pipeline selector must end with .yml or .yaml")
	}
}
