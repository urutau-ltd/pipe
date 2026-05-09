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

func TestPipelineMatchesRef(t *testing.T) {
	p := Pipeline{Branches: []string{"main", "release/*"}}
	ref, err := parseGitRef("refs/heads/main")
	if err != nil {
		t.Fatalf("parseGitRef failed: %v", err)
	}
	if !p.MatchesRef(ref) {
		t.Fatal("expected branch pipeline to match main")
	}

	ref, err = parseGitRef("refs/heads/release/v2")
	if err != nil {
		t.Fatalf("parseGitRef failed: %v", err)
	}
	if !p.MatchesRef(ref) {
		t.Fatal("expected wildcard branch pattern to match")
	}

	ref, err = parseGitRef("refs/tags/v2.1.10")
	if err != nil {
		t.Fatalf("parseGitRef failed: %v", err)
	}
	if p.MatchesRef(ref) {
		t.Fatal("did not expect branch-only pipeline to match tag")
	}

	p = Pipeline{Tags: []string{"v2.*", "stable-*"}}
	if !p.MatchesRef(ref) {
		t.Fatal("expected tag pipeline to match v2 tag")
	}
}
