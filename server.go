package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	aile "codeberg.org/urutau-ltd/aile/v2"
)

const (
	runBodyLimitBytes       = 64 << 10
	defaultJobQueueSize     = 32
	defaultServerWorkers    = 1
	maxPipelinesPerRequest  = 16
	defaultRunsListLimit    = 20
	maxRunsListLimit        = 200
	logPruneInterval        = 1 * time.Hour
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
	Port              int
	CloneBaseURL      string
	WorkDir           string
	PipelineFile      string
	Workers           int
	QueueSize         int
	LogRetentionDays  int
	LogRetentionCount int
	ActionsURL        string
	Executor          string
	ContainerEngine   string
	ContainerSocket   string
	ContainerImage    string
	PullPolicy        string
	Labels            map[string]string
	NoColor           bool
	LogFormat         string
	SecretEnv         []string
	SecretMask        []string
	NoMaskSecrets     bool
	GotifyEndpoint    string
	GotifyToken       string
	GotifyPriority    int
	GotifyOn          string
	workspaceManager  *repoWorkspaceManager
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
	runID        string
}

type jobResultStatus string

const (
	jobStatusQueued  jobResultStatus = "queued"
	jobStatusRunning jobResultStatus = "running"
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
	if strings.TrimSpace(cfg.Executor) == "" {
		cfg.Executor = "auto"
	}
	if strings.TrimSpace(cfg.ContainerEngine) == "" {
		cfg.ContainerEngine = "auto"
	}
	if strings.TrimSpace(cfg.PullPolicy) == "" {
		cfg.PullPolicy = "missing"
	}
	if strings.TrimSpace(cfg.LogFormat) == "" {
		cfg.LogFormat = "auto"
	}
	if cfg.Workers <= 0 {
		cfg.Workers = defaultServerWorkers
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultJobQueueSize
	}
	if cfg.LogRetentionDays < 0 {
		cfg.LogRetentionDays = 0
	}
	if cfg.LogRetentionCount < 0 {
		cfg.LogRetentionCount = 0
	}
	if cfg.Labels == nil {
		cfg.Labels = map[string]string{}
	}
	if cfg.workspaceManager == nil {
		cfg.workspaceManager = newRepoWorkspaceManager(cfg.WorkDir)
	}
	if err := validateServerRuntime(cfg); err != nil {
		log.Fatalf("pipe: runtime preflight failed: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(cfg.WorkDir, "logs"), 0o755); err != nil {
		log.Fatalf("pipe: creating workdir: %v", err)
	}
	if removed, err := pruneServerLogs(filepath.Join(cfg.WorkDir, "logs"), cfg.LogRetentionDays, cfg.LogRetentionCount); err != nil {
		log.Printf("pipe: log prune failed: %v", err)
	} else if removed > 0 {
		log.Printf("pipe: log prune removed=%d", removed)
	}
	go func() {
		ticker := time.NewTicker(logPruneInterval)
		defer ticker.Stop()
		for range ticker.C {
			removed, err := pruneServerLogs(filepath.Join(cfg.WorkDir, "logs"), cfg.LogRetentionDays, cfg.LogRetentionCount)
			if err != nil {
				log.Printf("pipe: periodic log prune failed: %v", err)
				continue
			}
			if removed > 0 {
				log.Printf("pipe: periodic log prune removed=%d", removed)
			}
		}
	}()

	jobs := make(chan job, cfg.QueueSize)
	tracker := newRunTracker(defaultRunHistoryLimit)
	for i := 0; i < cfg.Workers; i++ {
		go func() {
			for j := range jobs {
				processJob(j, cfg, tracker)
			}
		}()
	}

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
		var runIDs []string
		requested = len(pipelineFiles)

		for idx, pipelineFile := range pipelineFiles {
			logName := buildPipelineLogName(repoName, pipelineFile, idx)
			logPath := filepath.Join(cfg.WorkDir, "logs", logName)
			runID := tracker.enqueue(p, pipelineFile, logName)
			select {
			case jobs <- job{
				payload:      p,
				logPath:      logPath,
				pipelineFile: pipelineFile,
				runID:        runID,
			}:
				queued++
				logNames = append(logNames, logName)
				runIDs = append(runIDs, runID)
				log.Printf("pipe: queued  repo=%s ref=%s pipeline=%s", p.Repo, p.Ref, pipelineFile)
			default:
				tracker.drop(runID)
				if queued == 0 {
					http.Error(w, "queue full", http.StatusServiceUnavailable)
					return
				}
				w.WriteHeader(http.StatusAccepted)
				fmt.Fprintf(w, "partially queued  repo=%s ref=%s queued=%d requested=%d logs=%s runs=%s\n",
					p.Repo, p.Ref, queued, requested, strings.Join(logNames, ","), strings.Join(runIDs, ","))
				return
			}
		}

		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintf(w, "queued  repo=%s ref=%s pipelines=%d logs=%s runs=%s\n",
			p.Repo, p.Ref, queued, strings.Join(logNames, ","), strings.Join(runIDs, ","))
	})

	app.GET("/runs", func(w http.ResponseWriter, r *http.Request) {
		runID := strings.TrimSpace(r.URL.Query().Get("id"))
		if runID != "" {
			rec, ok := tracker.get(runID)
			if !ok {
				http.Error(w, "run not found", http.StatusNotFound)
				return
			}
			writeJSON(w, http.StatusOK, rec)
			return
		}

		limit := parseRunsListLimit(r.URL.Query().Get("limit"))
		writeJSON(w, http.StatusOK, tracker.snapshot(limit))
	})

	app.GET("/runs/log", func(w http.ResponseWriter, r *http.Request) {
		runID := strings.TrimSpace(r.URL.Query().Get("id"))
		if runID == "" {
			http.Error(w, "missing id", http.StatusBadRequest)
			return
		}
		rec, ok := tracker.get(runID)
		if !ok {
			http.Error(w, "run not found", http.StatusNotFound)
			return
		}
		logPath, err := resolveRunLogPath(filepath.Join(cfg.WorkDir, "logs"), rec.Log)
		if err != nil {
			http.Error(w, "invalid log path", http.StatusBadRequest)
			return
		}
		f, err := os.Open(logPath)
		if err != nil {
			if os.IsNotExist(err) {
				http.Error(w, "log not found", http.StatusNotFound)
				return
			}
			http.Error(w, "cannot open log", http.StatusInternalServerError)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, f)
	})

	app.GET("/health", func(w http.ResponseWriter, _ *http.Request) {
		aile.Text(w, http.StatusOK, "ok\n")
	})
	mode := cfg.GotifyOn
	if cfg.GotifyEndpoint == "" {
		mode = "off"
	}
	log.Printf("pipe: listening on %s  clone=%s  workdir=%s  cpus=%d  workers=%d  queue=%d  executor=%s  engine=%s  pull=%s  log-format=%s  log-retention-days=%d  log-retention-count=%d  labels=%v  gotify=%s",
		addr, cfg.CloneBaseURL, cfg.WorkDir, runtime.NumCPU(), cfg.Workers, cfg.QueueSize, cfg.Executor, cfg.ContainerEngine, cfg.PullPolicy, cfg.LogFormat, cfg.LogRetentionDays, cfg.LogRetentionCount, cfg.Labels, mode)
	log.Fatal(app.Run(context.Background()))
}

func processJob(j job, cfg ServerConfig, tracker *runTracker) {
	p := j.payload
	branch := stripBranch(p.Ref)
	status := jobStatusFail
	detail := "internal error"
	commitSHA := p.New
	pipelineFile := j.pipelineFile
	if pipelineFile == "" {
		pipelineFile = cfg.PipelineFile
	}
	logName := filepath.Base(j.logPath)
	actionsURL := strings.TrimSpace(cfg.ActionsURL)
	if tracker != nil && strings.TrimSpace(j.runID) != "" {
		tracker.markRunning(j.runID)
	}
	defer func() {
		if tracker != nil && strings.TrimSpace(j.runID) != "" {
			tracker.finish(j.runID, status, detail, commitSHA)
		}
		if removed, err := pruneServerLogs(filepath.Join(cfg.WorkDir, "logs"), cfg.LogRetentionDays, cfg.LogRetentionCount); err != nil {
			log.Printf("pipe: log prune failed: %v", err)
		} else if removed > 0 {
			log.Printf("pipe: log prune removed=%d", removed)
		}
		if err := notifyGotify(cfg, p, pipelineFile, branch, commitSHA, status, detail, logName, j.runID); err != nil {
			log.Printf("pipe: gotify notify failed: %v", err)
		}
	}()

	repoName, err := sanitizeRepo(p.Repo)
	if err != nil {
		detail = fmt.Sprintf("invalid repo: %v", err)
		log.Printf("pipe: %s", detail)
		return
	}

	cloneURL := strings.TrimRight(cfg.CloneBaseURL, "/") + "/" + repoName
	lf, err := os.OpenFile(j.logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
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
	workspace, err := cfg.workspaceManager.prepareRun(out, repoWorkspaceRequest{
		RepoName:  repoName,
		CloneURL:  cloneURL,
		Branch:    branch,
		CommitSHA: p.New,
		NullSHA:   nullSHA,
		RunID:     j.runID,
		Logf:      logf,
	})
	if err != nil {
		detail = err.Error()
		logf("%s", detail)
		return
	}
	defer func() {
		if err := os.RemoveAll(workspace.RunDir); err != nil {
			log.Printf("pipe: cleanup run workspace %s failed: %v", workspace.RunDir, err)
		}
	}()

	repoDir := workspace.RunDir
	commitSHA = workspace.CommitSHA
	if actionsURL != "" {
		logf("actions  url=%s", actionsURL)
	}

	pipeline, err := LoadPipeline(repoDir, pipelineFile)
	if err != nil {
		status = jobStatusIgnored
		if errors.Is(err, fs.ErrNotExist) {
			detail = fmt.Sprintf("pipeline %s not present at commit %s", pipelineFile, commitSHA)
		} else {
			detail = fmt.Sprintf("no pipeline: %v", err)
		}
		logf("%s", detail)
		return
	}
	if !pipelineMatchesLabels(pipeline.Labels, cfg.Labels) {
		status = jobStatusIgnored
		detail = fmt.Sprintf("labels mismatch pipeline=%v server=%v", pipeline.Labels, cfg.Labels)
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
		Dir:             repoDir,
		Branch:          branch,
		Output:          out,
		NoColor:         cfg.NoColor,
		LogFormat:       cfg.LogFormat,
		PullPolicy:      cfg.PullPolicy,
		Executor:        cfg.Executor,
		ContainerEngine: cfg.ContainerEngine,
		ContainerSocket: cfg.ContainerSocket,
		ContainerImage:  cfg.ContainerImage,
		SecretEnv:       cfg.SecretEnv,
		SecretMask:      cfg.SecretMask,
		NoMaskSecrets:   cfg.NoMaskSecrets,
		Env:             envMap,
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

func notifyGotify(cfg ServerConfig, p pushPayload, pipelineFile, branch, commit string, status jobResultStatus, detail, logName, runID string) error {
	if !shouldNotifyGotify(cfg, status) {
		return nil
	}
	if commit == "" {
		commit = "unknown"
	}
	if strings.TrimSpace(runID) == "" {
		runID = "n/a"
	}

	statusLabel := strings.ToUpper(string(status))

	payload := map[string]any{
		"title": fmt.Sprintf("pipe %s %s@%s", statusLabel, p.Repo, branch),
		"message": fmt.Sprintf(
			"status=%s\ndetail=%s\nrepo=%s\nbranch=%s\npipeline=%s\nref=%s\ncommit=%s\nrun=%s\nlog=%s",
			statusLabel, detail, p.Repo, branch, pipelineFile, p.Ref, commit, runID, logName,
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

func validateServerRuntime(cfg ServerConfig) error {
	mode, err := normalizeExecutorMode(cfg.Executor)
	if err != nil {
		return err
	}
	if _, err := normalizePullPolicy(cfg.PullPolicy); err != nil {
		return err
	}
	if mode == "host" {
		log.Printf("pipe: runtime preflight executor=host")
		return nil
	}

	rt, err := detectContainerRuntime(cfg.ContainerEngine, cfg.ContainerSocket)
	if err != nil {
		if mode == "container" {
			return err
		}
		log.Printf("pipe: runtime preflight auto mode could not detect container runtime: %v", err)
		return nil
	}

	socketLabel := "default context"
	if rt.SocketPath != "" {
		socketLabel = rt.SocketPath
	}
	log.Printf("pipe: runtime preflight executor=%s engine=%s socket=%s", mode, rt.Name, socketLabel)
	return nil
}

func parseLabelMap(raw []string) (map[string]string, error) {
	out := make(map[string]string, len(raw))
	for _, item := range raw {
		v := strings.TrimSpace(item)
		if v == "" {
			continue
		}
		parts := strings.SplitN(v, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("expected key=value, got %q", item)
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		if key == "" {
			return nil, fmt.Errorf("empty label key in %q", item)
		}
		if strings.ContainsAny(key, " \t\n\r=,") {
			return nil, fmt.Errorf("invalid label key %q", key)
		}
		out[key] = val
	}
	return out, nil
}

func pipelineMatchesLabels(required, available map[string]string) bool {
	if len(required) == 0 {
		return true
	}
	for key, expected := range required {
		got, ok := available[key]
		if !ok {
			return false
		}
		if strings.TrimSpace(expected) == "" || expected == "*" {
			continue
		}
		if got == "*" {
			continue
		}
		if got != expected {
			return false
		}
	}
	return true
}

func pruneServerLogs(logDir string, retentionDays, retentionCount int) (int, error) {
	if retentionDays <= 0 && retentionCount <= 0 {
		return 0, nil
	}
	entries, err := os.ReadDir(logDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	type logItem struct {
		path    string
		modTime time.Time
	}
	items := make([]logItem, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".log") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		items = append(items, logItem{
			path:    filepath.Join(logDir, name),
			modTime: info.ModTime(),
		})
	}

	removed := 0
	candidates := make([]logItem, 0, len(items))
	if retentionDays > 0 {
		cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)
		for _, item := range items {
			if item.modTime.Before(cutoff) {
				if err := os.Remove(item.path); err == nil {
					removed++
					continue
				}
			}
			candidates = append(candidates, item)
		}
	} else {
		candidates = append(candidates, items...)
	}

	if retentionCount > 0 && len(candidates) > retentionCount {
		sort.Slice(candidates, func(i, j int) bool {
			return candidates[i].modTime.Before(candidates[j].modTime)
		})
		toDelete := candidates[:len(candidates)-retentionCount]
		for _, item := range toDelete {
			if err := os.Remove(item.path); err == nil {
				removed++
			}
		}
	}

	return removed, nil
}

func resolveRunLogPath(logDir, logName string) (string, error) {
	raw := strings.TrimSpace(logName)
	if raw == "" {
		return "", fmt.Errorf("invalid log name")
	}
	if raw != filepath.Base(raw) {
		return "", fmt.Errorf("invalid log name")
	}
	name := filepath.Base(raw)
	if name == "" || name == "." || name == ".." {
		return "", fmt.Errorf("invalid log name")
	}
	if strings.ContainsAny(name, "/\\\x00") {
		return "", fmt.Errorf("invalid log name")
	}
	return filepath.Join(logDir, name), nil
}

func parseRunsListLimit(raw string) int {
	if strings.TrimSpace(raw) == "" {
		return defaultRunsListLimit
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return defaultRunsListLimit
	}
	if n > maxRunsListLimit {
		return maxRunsListLimit
	}
	return n
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("pipe: write json response failed: %v", err)
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
