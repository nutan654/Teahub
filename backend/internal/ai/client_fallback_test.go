package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func okBody(content string) apiResponse {
	return apiResponse{Choices: []choice{{Message: message{Role: "assistant", Content: content}}}}
}

// A retired model (404) must fall through to the next candidate model.
func TestComplete_FallsBackWhenModelRetired(t *testing.T) {
	var tried []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req apiRequest
		json.NewDecoder(r.Body).Decode(&req)
		tried = append(tried, req.Model)
		if req.Model == "retired-test-model" {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`[{"error":{"code":404,"message":"models/retired-test-model is not found","status":"NOT_FOUND"}}]`))
			return
		}
		json.NewEncoder(w).Encode(okBody("hello"))
	}))
	defer srv.Close()

	c := &Client{APIKey: "k", Model: "retired-test-model", BaseURL: srv.URL, HTTPClient: srv.Client()}
	got, err := c.Complete(context.Background(), "sys", "hi")
	if err != nil {
		t.Fatalf("unexpected error: %v (tried %v)", err, tried)
	}
	if got != "hello" {
		t.Errorf("got %q", got)
	}
	if len(tried) < 2 || tried[0] != "retired-test-model" || tried[1] != DefaultModel {
		t.Errorf("expected retired model then %s, tried %v", DefaultModel, tried)
	}
}

// Empty content (thinking ate the budget) is retried once with more tokens.
func TestComplete_RetriesEmptyAnswerWithLargerBudget(t *testing.T) {
	var budgets []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req apiRequest
		json.NewDecoder(r.Body).Decode(&req)
		budgets = append(budgets, req.MaxTokens)
		if len(budgets) == 1 {
			json.NewEncoder(w).Encode(okBody(""))
			return
		}
		json.NewEncoder(w).Encode(okBody("finally"))
	}))
	defer srv.Close()

	c := &Client{APIKey: "k", Model: "empty-retry-test-model", BaseURL: srv.URL, HTTPClient: srv.Client()}
	got, err := c.Complete(context.Background(), "sys", "hi")
	if err != nil || got != "finally" {
		t.Fatalf("got %q, err %v", got, err)
	}
	if len(budgets) != 2 || budgets[1] <= budgets[0] {
		t.Errorf("expected a second, larger request, got budgets %v", budgets)
	}
}

// Token floor: tiny budgets are raised so thinking models still answer.
func TestComplete_AppliesTokenFloor(t *testing.T) {
	var got int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req apiRequest
		json.NewDecoder(r.Body).Decode(&req)
		got = req.MaxTokens
		json.NewEncoder(w).Encode(okBody("ok"))
	}))
	defer srv.Close()
	c := &Client{APIKey: "k", Model: "floor-test-model", BaseURL: srv.URL, HTTPClient: srv.Client()}
	if _, err := c.Complete(context.Background(), "s", "u", CompleteOpts{MaxTokens: 1024}); err != nil {
		t.Fatal(err)
	}
	if got < minMaxTokens {
		t.Errorf("max_tokens %d below floor %d", got, minMaxTokens)
	}
}

// A bad API key must NOT trigger model fallback; it's reported as-is.
func TestComplete_AuthErrorDoesNotFallBack(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`[{"error":{"code":401,"message":"API key not valid","status":"UNAUTHENTICATED"}}]`))
	}))
	defer srv.Close()
	c := &Client{APIKey: "bad", Model: "auth-test-model", BaseURL: srv.URL, HTTPClient: srv.Client()}
	_, err := c.Complete(context.Background(), "s", "u")
	if err == nil || !strings.Contains(err.Error(), "UNAUTHENTICATED") {
		t.Fatalf("expected UNAUTHENTICATED error, got %v", err)
	}
	if calls != 1 {
		t.Errorf("expected exactly 1 call, got %d", calls)
	}
}
