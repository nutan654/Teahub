package semsearch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"teahub/backend/internal/ai"
	"teahub/backend/internal/store"
)

func TestBuildCandidates_KeywordHitsFirst(t *testing.T) {
	files := map[string]string{
		"auth/jwt.go":      "func VerifyToken() {}",
		"handlers/user.go": "func GetUser() {}",
		"README.md":        "docs",
	}
	got := buildCandidates(files, "jwt token verification")
	if len(got) == 0 || got[0] != "auth/jwt.go" {
		t.Errorf("expected auth/jwt.go first, got %v", got)
	}
	// every file should still appear somewhere (small repo, budget not exceeded)
	if len(got) != 3 {
		t.Errorf("expected all 3 files as candidates for a small repo, got %v", got)
	}
}

func TestBuildCandidates_FallsBackWhenNoKeywordOverlap(t *testing.T) {
	files := map[string]string{
		"nonce/check.go": "func ValidateNonce(seen map[string]bool) bool { return true }",
	}
	// Query shares zero literal words with the file, but should still appear
	// as a candidate for the model to judge semantically.
	got := buildCandidates(files, "how do we prevent replay attacks")
	if len(got) != 1 || got[0] != "nonce/check.go" {
		t.Errorf("expected fallback to include nonce/check.go, got %v", got)
	}
}

func TestBuildCandidates_RespectsMaxBudget(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < 200; i++ {
		files[fmt.Sprintf("file%03d.go", i)] = "package main"
	}
	got := buildCandidates(files, "something")
	if len(got) != maxCandidates {
		t.Errorf("expected exactly %d candidates, got %d", maxCandidates, len(got))
	}
}

func TestExtractJSONArray_PlainArray(t *testing.T) {
	got, err := ExtractJSONArray(`[{"path":"a.go","reason":"x"}]`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != `[{"path":"a.go","reason":"x"}]` {
		t.Errorf("unexpected extraction: %q", got)
	}
}

func TestExtractJSONArray_WithCodeFenceAndProse(t *testing.T) {
	raw := "Here are the results:\n```json\n[{\"path\":\"a.go\",\"reason\":\"x\"}]\n```\nHope that helps."
	got, err := ExtractJSONArray(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed []Result
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("extracted text isn't valid JSON: %v (%q)", err, got)
	}
}

func TestExtractJSONArray_Empty(t *testing.T) {
	got, err := ExtractJSONArray("[]")
	if err != nil || got != "[]" {
		t.Errorf("expected empty array to round-trip, got %q, err %v", got, err)
	}
}

func TestExtractJSONArray_NoArrayFound(t *testing.T) {
	_, err := ExtractJSONArray("I cannot find any relevant files.")
	if err == nil {
		t.Error("expected error when no array present")
	}
}

func TestSearch_ValidatesAgainstHallucinatedPaths(t *testing.T) {
	files := map[string]string{
		"real/file.go": "package real",
	}
	repo := &store.Repo{Files: files}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Model hallucinates a path that was never in the candidate list.
		resp := `[{"path":"real/file.go","reason":"matches"},{"path":"made/up/path.go","reason":"fake"}]`
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": resp}}},
		})
	}))
	defer srv.Close()

	client := &ai.Client{APIKey: "k", BaseURL: srv.URL, HTTPClient: srv.Client()}
	results, err := Search(context.Background(), client, repo, "something relevant")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 || results[0].Path != "real/file.go" {
		t.Errorf("expected only the real path to survive validation, got %+v", results)
	}
}

func TestSearch_EmptyQuery(t *testing.T) {
	repo := &store.Repo{Files: map[string]string{"a.go": "x"}}
	client := &ai.Client{APIKey: "k"}
	_, err := Search(context.Background(), client, repo, "   ")
	if err == nil {
		t.Error("expected error for empty query")
	}
}

func TestSearch_EmptyRepoReturnsEmptyNotError(t *testing.T) {
	repo := &store.Repo{Files: map[string]string{}}
	client := &ai.Client{APIKey: "k"}
	results, err := Search(context.Background(), client, repo, "anything")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected empty results, got %+v", results)
	}
}
