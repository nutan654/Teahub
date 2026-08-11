package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"teahub/backend/internal/ai"
	"teahub/backend/internal/auth"
	"teahub/backend/internal/config"
	"teahub/backend/internal/dbstore"
	"teahub/backend/internal/memstore"
	"teahub/backend/internal/store"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 1})) // silence logs
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	return &config.Config{
		Env:                 "test",
		JWTSigningKey:       []byte("test-signing-key-at-least-32-bytes!!"),
		SessionCookieName:   "teahub_session",
		SessionTTL:          time.Hour,
		SecureCookies:       false,
		RepoStorageDir:      dir,
		RequestTimeout:      10 * time.Second,
		ShutdownGracePeriod: time.Second,

		// Matches config.Load()'s defaults; tests exercising the size/length
		// limits or rate limiters directly should override these.
		MaxRequestBodyBytes:   1 << 20,
		MaxQuestionLength:     4000,
		MaxSearchQueryLength:  500,
		ImportRateLimitPerMin: 5,
		ImportRateLimitBurst:  3,
		AskRateLimitPerMin:    20,
		AskRateLimitBurst:     10,
	}
}

// testHarness bundles everything a handler test needs: the server, the
// backing memstore (so tests can seed data directly), and the runtime
// cache (so tests can seed Files/Index without a real git clone).
type testHarness struct {
	srv   *Server
	data  *memstore.MemStore
	cache *store.Store
	cfg   *config.Config
}

func newHarness(t *testing.T, aiClient ...*ai.Client) *testHarness {
	t.Helper()
	return newHarnessWithConfig(t, testConfig(t), aiClient...)
}

func newHarnessWithConfig(t *testing.T, cfg *config.Config, aiClient ...*ai.Client) *testHarness {
	t.Helper()
	data := memstore.New()
	cache := store.New()
	srv := New(cfg, data, cache, testLogger(), aiClient...)
	return &testHarness{srv: srv, data: data, cache: cache, cfg: cfg}
}

// createUserAndCookie creates a user directly in the store and returns a
// valid session cookie for them, bypassing the real GitHub OAuth flow
// (which needs network access) so handler tests can focus on
// authorization logic.
func (h *testHarness) createUserAndCookie(t *testing.T, githubLogin string) (*dbstore.User, *http.Cookie) {
	t.Helper()
	u, err := h.data.UpsertGitHubUser(context.Background(), int64(len(githubLogin)+1), githubLogin, githubLogin, "", "")
	if err != nil {
		t.Fatalf("creating test user: %v", err)
	}
	jti := "test-jti-" + githubLogin
	expiresAt := time.Now().Add(time.Hour)
	if err := h.data.CreateSession(context.Background(), u.ID, jti, "test-agent", "127.0.0.1", expiresAt); err != nil {
		t.Fatalf("creating test session: %v", err)
	}
	token, err := auth.SignJWT(h.cfg.JWTSigningKey, auth.Claims{
		Subject: u.ID, JWTID: jti, IssuedAt: time.Now().Unix(), ExpiresAt: expiresAt.Unix(),
	})
	if err != nil {
		t.Fatalf("signing test jwt: %v", err)
	}
	return u, &http.Cookie{Name: h.cfg.SessionCookieName, Value: token}
}

// seedReadyRepo creates a "ready" repository owned by owner, in both the
// metadata store and the runtime cache, without doing a real git clone —
// mirroring what a completed handleImport would have produced.
func (h *testHarness) seedReadyRepo(t *testing.T, owner *dbstore.User, id string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "main.go"), []byte("package main\nfunc main(){}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(root), "secret-outside.txt"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(filepath.Join(filepath.Dir(root), "secret-outside.txt")) })

	dbRepo, err := h.data.CreateRepository(context.Background(), owner.ID, "https://github.com/example/"+id)
	if err != nil {
		t.Fatalf("creating test repo: %v", err)
	}
	tree := &store.FileNode{Children: []*store.FileNode{
		{Path: "src", IsDir: true, Children: []*store.FileNode{{Path: "src/main.go"}}},
	}}
	if err := h.data.UpdateRepositoryResult(context.Background(), dbRepo.ID, root, dbstore.StatusReady, "", 1, tree); err != nil {
		t.Fatalf("updating test repo result: %v", err)
	}
	h.cache.Put(&store.Repo{
		ID: dbRepo.ID, OwnerID: owner.ID, URL: dbRepo.URL, LocalPath: root,
		Status: dbstore.StatusReady, FileCount: 1, Tree: tree,
		Files: map[string]string{"src/main.go": "package main\nfunc main(){}\n"},
	})
	return dbRepo.ID
}

func doReq(t *testing.T, srv *Server, method, path string, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	return doReqWithOrigin(t, srv, method, path, body, cookie, "")
}

func doReqWithOrigin(t *testing.T, srv *Server, method, path string, body string, cookie *http.Cookie, origin string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	return w
}

func TestFileContent_RequiresAuth(t *testing.T) {
	h := newHarness(t)
	w := doReq(t, h.srv, http.MethodGet, "/api/repos/anything/file?path=src/main.go", "", nil)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without a session cookie, got %d: %s", w.Code, w.Body.String())
	}
}

func TestFileContent_Success(t *testing.T) {
	h := newHarness(t)
	owner, cookie := h.createUserAndCookie(t, "alice")
	repoID := h.seedReadyRepo(t, owner, "repo1")

	w := doReq(t, h.srv, http.MethodGet, "/api/repos/"+repoID+"/file?path=src/main.go", "", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var fc map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &fc); err != nil {
		t.Fatal(err)
	}
	if fc["language"] != "go" {
		t.Errorf("expected language 'go', got %v", fc["language"])
	}
}

func TestFileContent_OtherUsersRepoIsNotFound(t *testing.T) {
	h := newHarness(t)
	owner, _ := h.createUserAndCookie(t, "alice")
	repoID := h.seedReadyRepo(t, owner, "repo1")

	_, bobCookie := h.createUserAndCookie(t, "bob")
	w := doReq(t, h.srv, http.MethodGet, "/api/repos/"+repoID+"/file?path=src/main.go", "", bobCookie)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for another user's repo (not 403, to avoid confirming existence), got %d", w.Code)
	}
}

func TestFileContent_UnknownRepo(t *testing.T) {
	h := newHarness(t)
	_, cookie := h.createUserAndCookie(t, "alice")
	w := doReq(t, h.srv, http.MethodGet, "/api/repos/does-not-exist/file?path=src/main.go", "", cookie)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestFileContent_TraversalBlocked(t *testing.T) {
	h := newHarness(t)
	owner, cookie := h.createUserAndCookie(t, "alice")
	repoID := h.seedReadyRepo(t, owner, "repo1")

	w := doReq(t, h.srv, http.MethodGet, "/api/repos/"+repoID+"/file?path=../secret-outside.txt", "", cookie)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for traversal attempt, got %d: %s", w.Code, w.Body.String())
	}
}

func TestFileContent_MissingPathParam(t *testing.T) {
	h := newHarness(t)
	owner, cookie := h.createUserAndCookie(t, "alice")
	repoID := h.seedReadyRepo(t, owner, "repo1")

	w := doReq(t, h.srv, http.MethodGet, "/api/repos/"+repoID+"/file", "", cookie)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing path param, got %d", w.Code)
	}
}

func TestDiagram_ReturnsCachedWithoutNetworkCall(t *testing.T) {
	h := newHarness(t)
	owner, cookie := h.createUserAndCookie(t, "alice")
	repoID := h.seedReadyRepo(t, owner, "repo1")
	h.cache.SetDiagram(repoID, "flowchart TD\n  A --> B", "A simple flow.")

	os.Unsetenv("ANTHROPIC_API_KEY")

	w := doReq(t, h.srv, http.MethodPost, "/api/repos/"+repoID+"/diagram", "", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["cached"] != true {
		t.Errorf("expected cached=true, got %v", body)
	}
	if body["mermaid"] != "flowchart TD\n  A --> B" {
		t.Errorf("unexpected mermaid content: %v", body["mermaid"])
	}
}

func TestDiagram_RefreshWithoutAPIKeyFailsCleanly(t *testing.T) {
	h := newHarness(t)
	owner, cookie := h.createUserAndCookie(t, "alice")
	repoID := h.seedReadyRepo(t, owner, "repo1")
	h.cache.SetDiagram(repoID, "flowchart TD\n  A --> B", "cached")
	os.Unsetenv("ANTHROPIC_API_KEY")

	w := doReq(t, h.srv, http.MethodPost, "/api/repos/"+repoID+"/diagram?refresh=true", "", cookie)
	if w.Code != http.StatusBadGateway {
		t.Errorf("expected 502 when refreshing without an API key, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDiagram_UnknownRepo(t *testing.T) {
	h := newHarness(t)
	_, cookie := h.createUserAndCookie(t, "alice")
	w := doReq(t, h.srv, http.MethodPost, "/api/repos/nope/diagram", "", cookie)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestSearch_Success(t *testing.T) {
	mockAI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := `[{"path":"src/main.go","reason":"entrypoint matching the query"}]`
		json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]string{{"type": "text", "text": resp}},
		})
	}))
	defer mockAI.Close()

	h := newHarness(t, &ai.Client{APIKey: "k", BaseURL: mockAI.URL, HTTPClient: mockAI.Client()})
	owner, cookie := h.createUserAndCookie(t, "alice")
	repoID := h.seedReadyRepo(t, owner, "repo1")

	w := doReq(t, h.srv, http.MethodPost, "/api/repos/"+repoID+"/search", `{"query":"where does the program start"}`, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	results := out["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d: %v", len(results), results)
	}
}

func TestSearch_EmptyQueryRejected(t *testing.T) {
	h := newHarness(t)
	owner, cookie := h.createUserAndCookie(t, "alice")
	repoID := h.seedReadyRepo(t, owner, "repo1")

	w := doReq(t, h.srv, http.MethodPost, "/api/repos/"+repoID+"/search", `{"query":""}`, cookie)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestSearch_UnknownRepo(t *testing.T) {
	h := newHarness(t)
	_, cookie := h.createUserAndCookie(t, "alice")
	w := doReq(t, h.srv, http.MethodPost, "/api/repos/nope/search", `{"query":"anything"}`, cookie)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// listReposResponse mirrors handleListRepos's JSON envelope.
type listReposResponse struct {
	Repos      []map[string]any `json:"repos"`
	NextCursor string           `json:"nextCursor"`
}

func TestListRepos_OnlyReturnsOwnRepos(t *testing.T) {
	h := newHarness(t)
	alice, aliceCookie := h.createUserAndCookie(t, "alice")
	h.seedReadyRepo(t, alice, "repo1")
	bob, bobCookie := h.createUserAndCookie(t, "bob")
	h.seedReadyRepo(t, bob, "repo2")

	w := doReq(t, h.srv, http.MethodGet, "/api/repos", "", aliceCookie)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var out listReposResponse
	json.Unmarshal(w.Body.Bytes(), &out)
	if len(out.Repos) != 1 {
		t.Fatalf("expected alice to see exactly 1 repo, got %d", len(out.Repos))
	}

	w = doReq(t, h.srv, http.MethodGet, "/api/repos", "", bobCookie)
	json.Unmarshal(w.Body.Bytes(), &out)
	if len(out.Repos) != 1 {
		t.Fatalf("expected bob to see exactly 1 repo, got %d", len(out.Repos))
	}
}

func TestListRepos_Pagination(t *testing.T) {
	h := newHarness(t)
	alice, cookie := h.createUserAndCookie(t, "alice")
	for i := 0; i < 5; i++ {
		h.seedReadyRepo(t, alice, "repo"+strings.Repeat("x", i+1))
	}

	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		path := "/api/repos?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		w := doReq(t, h.srv, http.MethodGet, path, "", cookie)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var out listReposResponse
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decoding response: %v", err)
		}
		if len(out.Repos) == 0 {
			t.Fatalf("page %d returned zero repos", pages)
		}
		if len(out.Repos) > 2 {
			t.Fatalf("page %d returned %d repos, expected <= limit 2", pages, len(out.Repos))
		}
		for _, r := range out.Repos {
			id, _ := r["id"].(string)
			if seen[id] {
				t.Fatalf("repo %s returned on more than one page", id)
			}
			seen[id] = true
		}
		pages++
		if out.NextCursor == "" {
			break
		}
		cursor = out.NextCursor
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}

	if len(seen) != 5 {
		t.Errorf("expected all 5 repos seen across pages, got %d", len(seen))
	}
	if pages != 3 { // 2 + 2 + 1
		t.Errorf("expected 3 pages for 5 repos at limit=2, got %d", pages)
	}
}

func TestListRepos_InvalidCursorRejected(t *testing.T) {
	h := newHarness(t)
	_, cookie := h.createUserAndCookie(t, "alice")
	w := doReq(t, h.srv, http.MethodGet, "/api/repos?cursor=not-valid-base64!!", "", cookie)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid cursor, got %d", w.Code)
	}
}

func TestListRepos_LimitClampedToMax(t *testing.T) {
	h := newHarness(t)
	alice, cookie := h.createUserAndCookie(t, "alice")
	h.seedReadyRepo(t, alice, "repo1")

	w := doReq(t, h.srv, http.MethodGet, "/api/repos?limit=99999", "", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	// Just confirms an oversized limit doesn't error; the clamp itself is
	// exercised implicitly (a real assertion would need >100 seeded repos).
}

func TestMe_RequiresAuth(t *testing.T) {
	h := newHarness(t)
	w := doReq(t, h.srv, http.MethodGet, "/api/me", "", nil)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestMe_ReturnsIdentity(t *testing.T) {
	h := newHarness(t)
	_, cookie := h.createUserAndCookie(t, "alice")
	w := doReq(t, h.srv, http.MethodGet, "/api/me", "", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["githubLogin"] != "alice" {
		t.Errorf("expected githubLogin=alice, got %v", body)
	}
}

func TestHealth_NoAuthRequired(t *testing.T) {
	h := newHarness(t)
	w := doReq(t, h.srv, http.MethodGet, "/api/health", "", nil)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestReady_ChecksDatabase(t *testing.T) {
	h := newHarness(t)
	w := doReq(t, h.srv, http.MethodGet, "/api/ready", "", nil)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 (memstore PingContext always succeeds), got %d", w.Code)
	}
}

func TestLogout_RevokesSessionSoCookieNoLongerWorks(t *testing.T) {
	h := newHarness(t)
	owner, cookie := h.createUserAndCookie(t, "alice")
	h.seedReadyRepo(t, owner, "repo1")

	w := doReq(t, h.srv, http.MethodPost, "/auth/logout", "", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from logout, got %d: %s", w.Code, w.Body.String())
	}

	w = doReq(t, h.srv, http.MethodGet, "/api/me", "", cookie)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 with a revoked session cookie, got %d", w.Code)
	}
}

func TestCORS_ConfiguredOriginIsReflected(t *testing.T) {
	cfg := testConfig(t)
	cfg.FrontendOrigin = "https://teahub.example.com"
	h := newHarnessWithConfig(t, cfg)

	w := doReqWithOrigin(t, h.srv, http.MethodGet, "/api/health", "", nil, "https://teahub.example.com")
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://teahub.example.com" {
		t.Errorf("expected configured origin reflected, got %q", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("expected Allow-Credentials: true, got %q", got)
	}
}

func TestCORS_UnconfiguredOriginIsNotReflected(t *testing.T) {
	cfg := testConfig(t)
	cfg.FrontendOrigin = "https://teahub.example.com"
	h := newHarnessWithConfig(t, cfg)

	// A request from an *arbitrary* origin must never get CORS headers,
	// even though FRONTEND_ORIGIN is configured for a different site —
	// this is the "no reflect-any-origin" regression test.
	w := doReqWithOrigin(t, h.srv, http.MethodGet, "/api/health", "", nil, "https://evil.example.com")
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("expected no CORS header for unconfigured origin, got %q", got)
	}
}

func TestCORS_NoFrontendOriginConfigured_NoHeadersEmitted(t *testing.T) {
	cfg := testConfig(t) // FrontendOrigin left empty, as in local dev without a configured frontend
	h := newHarnessWithConfig(t, cfg)

	w := doReqWithOrigin(t, h.srv, http.MethodGet, "/api/health", "", nil, "https://anything.example.com")
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("expected no CORS header when FRONTEND_ORIGIN unset, got %q", got)
	}
}

func TestAsk_QuestionTooLongRejected(t *testing.T) {
	cfg := testConfig(t)
	cfg.MaxQuestionLength = 10
	h := newHarnessWithConfig(t, cfg)
	owner, cookie := h.createUserAndCookie(t, "alice")
	repoID := h.seedReadyRepo(t, owner, "repo1")

	body := `{"repoId":"` + repoID + `","question":"this question is way longer than ten characters"}`
	w := doReq(t, h.srv, http.MethodPost, "/api/ask", body, cookie)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for over-length question, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSearch_QueryTooLongRejected(t *testing.T) {
	cfg := testConfig(t)
	cfg.MaxSearchQueryLength = 10
	h := newHarnessWithConfig(t, cfg)
	owner, cookie := h.createUserAndCookie(t, "alice")
	repoID := h.seedReadyRepo(t, owner, "repo1")

	w := doReq(t, h.srv, http.MethodPost, "/api/repos/"+repoID+"/search", `{"query":"this query is way longer than ten characters"}`, cookie)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for over-length query, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAsk_OversizedBodyRejected(t *testing.T) {
	cfg := testConfig(t)
	cfg.MaxRequestBodyBytes = 20 // smaller than even a minimal valid request body
	h := newHarnessWithConfig(t, cfg)
	owner, cookie := h.createUserAndCookie(t, "alice")
	repoID := h.seedReadyRepo(t, owner, "repo1")

	body := `{"repoId":"` + repoID + `","question":"where does main live?"}`
	w := doReq(t, h.srv, http.MethodPost, "/api/ask", body, cookie)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for oversized body, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAsk_RateLimitExceededReturns429(t *testing.T) {
	mockAI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]string{{"type": "text", "text": "answer"}},
		})
	}))
	defer mockAI.Close()

	cfg := testConfig(t)
	cfg.AskRateLimitBurst = 2
	cfg.AskRateLimitPerMin = 0.001 // effectively no refill during the test
	h := newHarnessWithConfig(t, cfg, &ai.Client{APIKey: "k", BaseURL: mockAI.URL, HTTPClient: mockAI.Client()})
	owner, cookie := h.createUserAndCookie(t, "alice")
	repoID := h.seedReadyRepo(t, owner, "repo1")

	body := `{"repoId":"` + repoID + `","question":"where does main live?"}`
	var lastCode int
	for i := 0; i < 3; i++ {
		lastCode = doReq(t, h.srv, http.MethodPost, "/api/ask", body, cookie).Code
	}
	if lastCode != http.StatusTooManyRequests {
		t.Errorf("expected the 3rd request (burst=2) to be rate limited with 429, got %d", lastCode)
	}
}

func TestImport_RateLimitExceededReturns429(t *testing.T) {
	cfg := testConfig(t)
	cfg.ImportRateLimitBurst = 1
	cfg.ImportRateLimitPerMin = 0.001
	h := newHarnessWithConfig(t, cfg)
	_, cookie := h.createUserAndCookie(t, "alice")

	// Both requests use invalid URLs, so they fail fast at validation —
	// what's under test is that the *second* request never reaches the
	// handler at all once the single-token burst is spent.
	first := doReq(t, h.srv, http.MethodPost, "/api/import", `{"url":"not-a-valid-url"}`, cookie)
	if first.Code == http.StatusTooManyRequests {
		t.Fatalf("first request should not be rate limited, got 429")
	}
	second := doReq(t, h.srv, http.MethodPost, "/api/import", `{"url":"also-not-valid"}`, cookie)
	if second.Code != http.StatusTooManyRequests {
		t.Errorf("expected second request (burst=1) to be rate limited with 429, got %d: %s", second.Code, second.Body.String())
	}
}
