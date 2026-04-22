package main

import "testing"

func TestStepDependencyNames(t *testing.T) {
	s := Step{
		Needs:     []string{"build"},
		DependsOn: []string{"build", "lint"},
	}
	got := s.DependencyNames()
	if len(got) != 2 {
		t.Fatalf("unexpected dependencies: %#v", got)
	}
	if got[0] != "build" || got[1] != "lint" {
		t.Fatalf("unexpected dependency order/content: %#v", got)
	}
}

func TestStepFailureIgnored(t *testing.T) {
	if !(Step{Failure: "ignore"}).FailureIgnored() {
		t.Fatal("expected failure: ignore to be enabled")
	}
	if (Step{Failure: "fail"}).FailureIgnored() {
		t.Fatal("did not expect failure ignore for fail mode")
	}
}

func TestStepShouldRunForPipelineStatus(t *testing.T) {
	if !(Step{}).ShouldRunForPipelineStatus(false) {
		t.Fatal("default step should run when pipeline is healthy")
	}
	if (Step{}).ShouldRunForPipelineStatus(true) {
		t.Fatal("default step should not run when pipeline failed")
	}
	if !(Step{RunsOn: []string{"failure"}}).ShouldRunForPipelineStatus(true) {
		t.Fatal("failure step should run when pipeline failed")
	}
	if (Step{RunsOn: []string{"failure"}}).ShouldRunForPipelineStatus(false) {
		t.Fatal("failure step should not run on success")
	}
	if !(Step{RunsOn: []string{"always"}}).ShouldRunForPipelineStatus(true) {
		t.Fatal("always step should run on failure")
	}
}
