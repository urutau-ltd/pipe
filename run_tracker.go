package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

const defaultRunHistoryLimit = 256

type runRecord struct {
	ID         string    `json:"id"`
	Repo       string    `json:"repo"`
	Ref        string    `json:"ref"`
	Branch     string    `json:"branch"`
	Pipeline   string    `json:"pipeline"`
	Log        string    `json:"log"`
	Status     string    `json:"status"`
	Detail     string    `json:"detail,omitempty"`
	Commit     string    `json:"commit,omitempty"`
	QueuedAt   time.Time `json:"queued_at"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

type runsResponse struct {
	Queued  int         `json:"queued"`
	Running int         `json:"running"`
	Items   []runRecord `json:"items"`
}

type runTracker struct {
	mu    sync.Mutex
	limit int
	seq   uint64
	order []string
	byID  map[string]runRecord
}

func newRunTracker(limit int) *runTracker {
	if limit <= 0 {
		limit = defaultRunHistoryLimit
	}
	return &runTracker{
		limit: limit,
		byID:  make(map[string]runRecord, limit),
	}
}

func (t *runTracker) enqueue(p pushPayload, pipelineFile, logName string) string {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.seq++
	now := time.Now().UTC()
	id := fmt.Sprintf("run-%d-%d", now.Unix(), t.seq)
	rec := runRecord{
		ID:       id,
		Repo:     p.Repo,
		Ref:      p.Ref,
		Branch:   stripBranch(p.Ref),
		Pipeline: pipelineFile,
		Log:      logName,
		Status:   string(jobStatusQueued),
		QueuedAt: now,
	}

	t.byID[id] = rec
	t.order = append(t.order, id)
	t.trimLocked()
	return id
}

func (t *runTracker) drop(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	trimmed := strings.TrimSpace(id)
	delete(t.byID, trimmed)
	for i, entry := range t.order {
		if entry == trimmed {
			t.order = append(t.order[:i], t.order[i+1:]...)
			break
		}
	}
}

func (t *runTracker) markRunning(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	rec, ok := t.byID[strings.TrimSpace(id)]
	if !ok {
		return
	}
	now := time.Now().UTC()
	rec.Status = string(jobStatusRunning)
	rec.StartedAt = now
	t.byID[rec.ID] = rec
}

func (t *runTracker) finish(id string, status jobResultStatus, detail, commit string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	rec, ok := t.byID[strings.TrimSpace(id)]
	if !ok {
		return
	}
	now := time.Now().UTC()
	if rec.StartedAt.IsZero() {
		rec.StartedAt = now
	}
	rec.Status = string(status)
	rec.Detail = strings.TrimSpace(detail)
	rec.Commit = strings.TrimSpace(commit)
	rec.FinishedAt = now
	t.byID[rec.ID] = rec
}

func (t *runTracker) get(id string) (runRecord, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	rec, ok := t.byID[strings.TrimSpace(id)]
	return rec, ok
}

func (t *runTracker) snapshot(limit int) runsResponse {
	t.mu.Lock()
	defer t.mu.Unlock()

	if limit <= 0 || limit > t.limit {
		limit = t.limit
	}
	resp := runsResponse{
		Items: make([]runRecord, 0, limit),
	}

	for _, id := range t.order {
		rec, ok := t.byID[id]
		if !ok {
			continue
		}
		switch rec.Status {
		case string(jobStatusQueued):
			resp.Queued++
		case string(jobStatusRunning):
			resp.Running++
		}
	}

	for i := len(t.order) - 1; i >= 0 && len(resp.Items) < limit; i-- {
		id := t.order[i]
		rec, ok := t.byID[id]
		if !ok {
			continue
		}
		resp.Items = append(resp.Items, rec)
	}
	return resp
}

func (t *runTracker) trimLocked() {
	if t.limit <= 0 {
		return
	}
	for len(t.order) > t.limit {
		oldest := t.order[0]
		t.order = t.order[1:]
		delete(t.byID, oldest)
	}
}
