package main

import "testing"

func TestRunTrackerLifecycle(t *testing.T) {
	tracker := newRunTracker(8)
	payload := pushPayload{
		Repo: "pipe",
		Ref:  "refs/heads/main",
	}

	runID := tracker.enqueue(payload, ".pipe.yml", "pipe.log")
	if runID == "" {
		t.Fatal("expected run id")
	}

	snap := tracker.snapshot(10)
	if snap.Queued != 1 || snap.Running != 0 {
		t.Fatalf("unexpected queue/running counters: %#v", snap)
	}
	if len(snap.Items) != 1 {
		t.Fatalf("expected one item, got %d", len(snap.Items))
	}
	if snap.Items[0].Status != string(jobStatusQueued) {
		t.Fatalf("unexpected initial status: %q", snap.Items[0].Status)
	}

	tracker.markRunning(runID)
	snap = tracker.snapshot(10)
	if snap.Queued != 0 || snap.Running != 1 {
		t.Fatalf("unexpected queue/running counters after running: %#v", snap)
	}
	if snap.Items[0].Status != string(jobStatusRunning) {
		t.Fatalf("unexpected running status: %q", snap.Items[0].Status)
	}

	tracker.finish(runID, jobStatusOK, "pipeline passed", "abc123")
	rec, ok := tracker.get(runID)
	if !ok {
		t.Fatal("expected run to exist")
	}
	if rec.Status != string(jobStatusOK) {
		t.Fatalf("unexpected final status: %q", rec.Status)
	}
	if rec.Detail != "pipeline passed" || rec.Commit != "abc123" {
		t.Fatalf("unexpected final detail/commit: %#v", rec)
	}
	if rec.StartedAt.IsZero() || rec.FinishedAt.IsZero() {
		t.Fatalf("expected timestamps to be set: %#v", rec)
	}
}

func TestRunTrackerTrim(t *testing.T) {
	tracker := newRunTracker(2)
	payload := pushPayload{
		Repo: "pipe",
		Ref:  "refs/heads/main",
	}

	first := tracker.enqueue(payload, ".pipe/first.yml", "first.log")
	second := tracker.enqueue(payload, ".pipe/second.yml", "second.log")
	third := tracker.enqueue(payload, ".pipe/third.yml", "third.log")

	if _, ok := tracker.get(first); ok {
		t.Fatalf("expected oldest run %q to be trimmed", first)
	}
	if _, ok := tracker.get(second); !ok {
		t.Fatalf("expected second run %q to exist", second)
	}
	if _, ok := tracker.get(third); !ok {
		t.Fatalf("expected third run %q to exist", third)
	}

	snap := tracker.snapshot(10)
	if len(snap.Items) != 2 {
		t.Fatalf("expected 2 runs in snapshot, got %d", len(snap.Items))
	}
	if snap.Items[0].ID != third || snap.Items[1].ID != second {
		t.Fatalf("unexpected run order: %#v", snap.Items)
	}
}
