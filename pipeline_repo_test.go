package main

import "testing"

func TestBundledPipelinesParse(t *testing.T) {
	pipelines := []string{
		".pipe.yml",
		".pipe/ci.yml",
		".pipe/release.yml",
	}

	for _, file := range pipelines {
		if _, err := LoadPipeline(".", file); err != nil {
			t.Fatalf("LoadPipeline(%q) failed: %v", file, err)
		}
	}
}
