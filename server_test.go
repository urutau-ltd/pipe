package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSanitizeRepo(t *testing.T) {
	tests := []struct {
		name    string
		repo    string
		wantErr bool
	}{
		{name: "valid", repo: "my-app", wantErr: false},
		{name: "path traversal", repo: "../my-app", wantErr: true},
		{name: "slash", repo: "org/my-app", wantErr: true},
		{name: "empty", repo: "", wantErr: true},
		{name: "spaces", repo: " my-app ", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := sanitizeRepo(tc.repo)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for %q", tc.repo)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.repo, err)
			}
		})
	}
}

func TestValidateRef(t *testing.T) {
	tests := []struct {
		name    string
		ref     string
		wantErr bool
	}{
		{name: "valid main", ref: "refs/heads/main", wantErr: false},
		{name: "valid feature", ref: "refs/heads/feature/x", wantErr: false},
		{name: "tag not allowed", ref: "refs/tags/v1.0.0", wantErr: true},
		{name: "double dot", ref: "refs/heads/feat..x", wantErr: true},
		{name: "space", ref: "refs/heads/feat x", wantErr: true},
		{name: "empty branch", ref: "refs/heads/", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRef(tc.ref)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for %q", tc.ref)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.ref, err)
			}
		})
	}
}

func TestShouldNotifyGotify(t *testing.T) {
	cfg := ServerConfig{GotifyEndpoint: "https://gotify.local/message"}

	if !shouldNotifyGotify(cfg, jobStatusOK) {
		t.Fatal("expected ok status to notify in default mode")
	}
	if !shouldNotifyGotify(cfg, jobStatusFail) {
		t.Fatal("expected fail status to notify in default mode")
	}

	cfg.GotifyOn = "fail"

	if !shouldNotifyGotify(cfg, jobStatusFail) {
		t.Fatal("expected fail status to notify in fail mode")
	}
	if shouldNotifyGotify(cfg, jobStatusOK) {
		t.Fatal("did not expect ok status to notify in fail mode")
	}

	cfg.GotifyOn = "all"
	if !shouldNotifyGotify(cfg, jobStatusOK) {
		t.Fatal("expected ok status to notify in all mode")
	}

	cfg.GotifyEndpoint = ""
	if shouldNotifyGotify(cfg, jobStatusFail) {
		t.Fatal("did not expect notify when endpoint is empty")
	}
}

func TestNotifyGotify(t *testing.T) {
	type gotifyRequest struct {
		Title    string `json:"title"`
		Message  string `json:"message"`
		Priority int    `json:"priority"`
	}

	var reqBody gotifyRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Fatalf("unexpected content type: %q", ct)
		}
		if key := r.Header.Get("X-Gotify-Key"); key != "" {
			t.Fatalf("did not expect auth header, got: %q", key)
		}
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := notifyGotify(ServerConfig{
		GotifyEndpoint: srv.URL,
		GotifyPriority: 7,
		GotifyOn:       "all",
	}, pushPayload{
		Repo: "pipe",
		Ref:  "refs/heads/main",
	}, "main", "abc123", jobStatusOK, "pipeline passed", "pipe-123.log")
	if err != nil {
		t.Fatalf("notifyGotify returned error: %v", err)
	}

	if reqBody.Priority != 7 {
		t.Fatalf("unexpected priority: %d", reqBody.Priority)
	}
	if !strings.Contains(reqBody.Title, "pipe OK") {
		t.Fatalf("unexpected title: %q", reqBody.Title)
	}
	if !strings.Contains(reqBody.Message, "repo=pipe") {
		t.Fatalf("unexpected message: %q", reqBody.Message)
	}
}

func TestNotifyGotifyWithToken(t *testing.T) {
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Gotify-Key")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := notifyGotify(ServerConfig{
		GotifyEndpoint: srv.URL,
		GotifyToken:    "secret-token",
		GotifyPriority: 5,
		GotifyOn:       "all",
	}, pushPayload{Repo: "pipe", Ref: "refs/heads/main"}, "main", "abc123", jobStatusOK, "pipeline passed", "pipe-123.log")
	if err != nil {
		t.Fatalf("notifyGotify returned error: %v", err)
	}
	if gotKey != "secret-token" {
		t.Fatalf("unexpected gotify key header: %q", gotKey)
	}
}

func TestNotifyGotifyErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := notifyGotify(ServerConfig{
		GotifyEndpoint: srv.URL,
		GotifyPriority: 5,
		GotifyOn:       "all",
	}, pushPayload{Repo: "pipe", Ref: "refs/heads/main"}, "main", "abc123", jobStatusFail, "pipeline failed", "pipe-123.log")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "gotify returned") {
		t.Fatalf("unexpected error: %v", err)
	}
}
