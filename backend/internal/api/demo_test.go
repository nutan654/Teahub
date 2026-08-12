package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"teahub/backend/internal/ai"
	"teahub/backend/internal/config"
	"teahub/backend/internal/store"
)

// demoTestConfig returns a config with Demo Mode "on" (DemoRepoURL set)
// so routes() registers the /api/demo/* handlers, without ever actually
// cloning anything — tests seed the demo repo into the cache directly,
// the same way seedReadyRepo does for authenticated-repo tests.
func demoTestConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := testConfig(t)
	cfg.DemoRepoURL = "https://github.com/example/demo-repo"
	cfg.DemoAskRateLimitPerMin = 120 // generous by default; specific tests override to exercise the limiter
	cfg.DemoAskRateLimitBurst = 60
	return cfg
}

// seedDemoRepo mirrors testHarness.seedReadyRepo but stores under the
// fixed demoRepoID and skips everything ownership-related, matching what
// ImportDemoRepo produces for real.
func (h *testHarness) seedDemoRepo(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "main.go"), []byte("package main\nfunc main(){}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tree := &store.FileNode{Children: []*store.FileNode{
		{Path: "src", IsDir: true, Children: []*store.FileNode{{Path: "src/main.go"}}},
	}}
	h.cache.Put(&store.Repo{
		ID: demoRepoID, URL: "https://github.com/example/demo-repo", LocalPath: root,
		Status: "ready", FileCount: 1, Tree: tree,
		Files: map[string]string{"src/main.go": "package main\nfunc main(){}\n"},
	})
}

func TestDemoRoutes_NotRegisteredWhenDisabled(t *testing.T) {
	h := newHarness(t) // testConfig leaves DemoRepoURL empty
	w := doReq(t, h.srv, http.MethodGet, "/api/demo", "", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for /api/demo when Demo Mode is unconfigured, got %d", w.Code)
	}
}

func TestDemoInfo_UnavailableBeforeImport(t *testing.T) {
	h := newHarnessWithConfig(t, demoTestConfig(t))
	// Demo Mode is "on" (DemoRepoURL set, routes registered) but
	// ImportDemoRepo was never called, so the cache has no entry yet.
	w := doReq(t, h.srv, http.MethodGet, "/api/demo", "", nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 before the demo repo is imported, got %d", w.Code)
	}
}

func TestDemoInfo_Success_NoAuthRequired(t *testing.T) {
	h := newHarnessWithConfig(t, demoTestConfig(t))
	h.seedDemoRepo(t)

	// Deliberately no cookie: this is the whole point of Demo Mode.
	w := doReq(t, h.srv, http.MethodGet, "/api/demo", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if out["demo"] != true {
		t.Errorf(`expected "demo": true in response, got %v`, out["demo"])
	}
	if out["id"] != demoRepoID {
		t.Errorf("expected id %q, got %v", demoRepoID, out["id"])
	}
}

func TestDemoFile_Success(t *testing.T) {
	h := newHarnessWithConfig(t, demoTestConfig(t))
	h.seedDemoRepo(t)

	w := doReq(t, h.srv, http.MethodGet, "/api/demo/file?path=src/main.go", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// TestDemoFile_TraversalBlocked proves the demo file endpoint gets the
// exact same fsread sandboxing as the authenticated one — dropping the
// auth requirement must never widen what paths are readable.
func TestDemoFile_TraversalBlocked(t *testing.T) {
	h := newHarnessWithConfig(t, demoTestConfig(t))
	h.seedDemoRepo(t)

	w := doReq(t, h.srv, http.MethodGet, "/api/demo/file?path=../../etc/passwd", "", nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for a path-traversal attempt, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDemoAsk_Success_NoAuthRequired(t *testing.T) {
	mockAI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]string{{"type": "text", "text": "This repo has one file, src/main.go."}},
		})
	}))
	defer mockAI.Close()

	h := newHarnessWithConfig(t, demoTestConfig(t), &ai.Client{APIKey: "k", BaseURL: mockAI.URL, HTTPClient: mockAI.Client()})
	h.seedDemoRepo(t)

	w := doReq(t, h.srv, http.MethodPost, "/api/demo/ask", `{"question":"what does this repo do?"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDemoAsk_EmptyQuestionRejected(t *testing.T) {
	h := newHarnessWithConfig(t, demoTestConfig(t))
	h.seedDemoRepo(t)

	w := doReq(t, h.srv, http.MethodPost, "/api/demo/ask", `{"question":""}`, nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

// TestDemoAsk_RateLimitedByIP proves anonymous demo traffic is bounded
// even with no user ID to key on — the actual reason demoLimiter exists
// (protecting the Anthropic API budget from a publicly linked demo).
func TestDemoAsk_RateLimitedByIP(t *testing.T) {
	mockAI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]string{{"type": "text", "text": "answer"}},
		})
	}))
	defer mockAI.Close()

	cfg := demoTestConfig(t)
	cfg.DemoAskRateLimitPerMin = 60 // 1/sec sustained
	cfg.DemoAskRateLimitBurst = 2   // ...but only 2 allowed instantaneously

	h := newHarnessWithConfig(t, cfg, &ai.Client{APIKey: "k", BaseURL: mockAI.URL, HTTPClient: mockAI.Client()})
	h.seedDemoRepo(t)

	var codes []int
	for i := 0; i < 3; i++ {
		w := doReq(t, h.srv, http.MethodPost, "/api/demo/ask", `{"question":"what does this repo do?"}`, nil)
		codes = append(codes, w.Code)
	}
	if codes[0] != http.StatusOK || codes[1] != http.StatusOK {
		t.Fatalf("expected the first 2 requests (within burst) to succeed, got %v", codes)
	}
	if codes[2] != http.StatusTooManyRequests {
		t.Errorf("expected the 3rd request to be rate-limited (429), got %d", codes[2])
	}
}

func TestDemoDiagram_CachedAfterFirstGeneration(t *testing.T) {
	calls := 0
	mockAI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		resp := "```mermaid\nflowchart TD\n  A[main.go] --> B[main]\n```"
		json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]string{{"type": "text", "text": resp}},
		})
	}))
	defer mockAI.Close()

	h := newHarnessWithConfig(t, demoTestConfig(t), &ai.Client{APIKey: "k", BaseURL: mockAI.URL, HTTPClient: mockAI.Client()})
	h.seedDemoRepo(t)

	w1 := doReq(t, h.srv, http.MethodPost, "/api/demo/diagram", "", nil)
	if w1.Code != http.StatusOK {
		t.Fatalf("expected 200 on first generation, got %d: %s", w1.Code, w1.Body.String())
	}
	w2 := doReq(t, h.srv, http.MethodPost, "/api/demo/diagram", "", nil)
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200 on cached fetch, got %d: %s", w2.Code, w2.Body.String())
	}
	if calls != 1 {
		t.Errorf("expected exactly 1 AI call (2nd request should be served from cache), got %d", calls)
	}
}
