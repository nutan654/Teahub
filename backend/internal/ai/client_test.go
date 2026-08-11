package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestComplete_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-key" {
			t.Errorf("missing/wrong x-api-key header: %q", r.Header.Get("x-api-key"))
		}
		var req apiRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decoding request: %v", err)
		}
		if req.Model == "" || len(req.Messages) != 1 {
			t.Errorf("unexpected request shape: %+v", req)
		}
		json.NewEncoder(w).Encode(apiResponse{
			Content: []contentBlock{{Type: "text", Text: "42"}},
		})
	}))
	defer srv.Close()

	c := &Client{APIKey: "test-key", Model: "claude-sonnet-5", BaseURL: srv.URL, HTTPClient: srv.Client()}
	got, err := c.Complete(context.Background(), "system", "what is the answer?")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "42" {
		t.Errorf("got %q, want %q", got, "42")
	}
}

func TestComplete_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"type": "authentication_error", "message": "invalid x-api-key"},
		})
	}))
	defer srv.Close()

	c := &Client{APIKey: "bad-key", BaseURL: srv.URL, HTTPClient: srv.Client()}
	_, err := c.Complete(context.Background(), "sys", "hello")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "authentication_error") {
		t.Errorf("expected authentication_error in message, got: %v", err)
	}
}

func TestComplete_NoAPIKey(t *testing.T) {
	c := &Client{APIKey: ""}
	_, err := c.Complete(context.Background(), "sys", "hello")
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("expected missing-key error, got: %v", err)
	}
}

func TestComplete_MaxTokensOverride(t *testing.T) {
	var gotMaxTokens int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req apiRequest
		json.NewDecoder(r.Body).Decode(&req)
		gotMaxTokens = req.MaxTokens
		json.NewEncoder(w).Encode(apiResponse{Content: []contentBlock{{Type: "text", Text: "ok"}}})
	}))
	defer srv.Close()

	c := &Client{APIKey: "k", BaseURL: srv.URL, HTTPClient: srv.Client()}
	_, err := c.Complete(context.Background(), "sys", "hi", CompleteOpts{MaxTokens: 4096})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotMaxTokens != 4096 {
		t.Errorf("expected max_tokens=4096, got %d", gotMaxTokens)
	}
}
