// Package api is TeaHub's HTTP layer: routing, request/response
// marshaling, authentication enforcement, and repository-ownership
// checks. Every handler that touches a specific repository re-verifies
// ownership against the database on every call (see repoForCaller) —
// ownership is never trusted from a cached in-memory object alone.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"teahub/backend/internal/ai"
	"teahub/backend/internal/auth"
	"teahub/backend/internal/config"
	"teahub/backend/internal/httpmw"
	"teahub/backend/internal/metrics"
	"teahub/backend/internal/store"
)

// Server wires together persistence, the runtime cache, the AI client,
// and GitHub OAuth into one http.Handler.
type Server struct {
	cfg    *config.Config
	data   DataStore
	cache  *store.Store
	logger *slog.Logger
	oauth  *auth.GitHubOAuth
	mux    *http.ServeMux

	// Version is the build version string (set by main from a linker flag;
	// see Dockerfile), surfaced on /api/health for deploy verification. A
	// plain exported field rather than a constructor arg since it's purely
	// diagnostic and optional — tests never need to set it.
	Version string

	// ai is only set when a test explicitly injects a Client (e.g. pointed
	// at an httptest mock server). In normal operation it stays nil and
	// aiClient() builds a fresh Client from the environment on every call,
	// so ANTHROPIC_API_KEY can be set/rotated without restarting the process.
	ai *ai.Client

	// oauthStates tracks in-flight CSRF state tokens issued by
	// /auth/github/login, so /auth/github/callback can reject a
	// forged/replayed callback. Entries expire after 10 minutes; a real
	// multi-instance deployment would put this in Postgres or Redis
	// instead of process memory, but a login flow that must complete
	// within 10 minutes on the *same* instance is an acceptable
	// limitation for Cloud Run's default single-revision routing and is
	// called out in ARCHITECTURE.md "Known limitations".
	statesMu sync.Mutex
	states   map[string]time.Time

	// importLimiter / askLimiter enforce per-user request-rate ceilings on
	// the cost-bearing endpoints (see ratelimit.go's doc comment on why
	// this is in-memory/per-instance rather than distributed).
	importLimiter *httpmw.UserRateLimiter
	askLimiter    *httpmw.UserRateLimiter

	// metrics collects basic request/error counts and latency percentiles,
	// served at GET /metrics. See internal/metrics's package doc for why
	// this is a small in-memory counter rather than a full metrics client.
	metrics *metrics.Collector
}

// New wires a Server. aiClient lets tests inject a Client pointed at a
// mock server instead of the real Anthropic API.
func New(cfg *config.Config, data DataStore, cache *store.Store, logger *slog.Logger, aiClient ...*ai.Client) *Server {
	srv := &Server{
		cfg:           cfg,
		data:          data,
		cache:         cache,
		logger:        logger,
		mux:           http.NewServeMux(),
		states:        make(map[string]time.Time),
		importLimiter: httpmw.NewUserRateLimiter(cfg.ImportRateLimitPerMin/60, cfg.ImportRateLimitBurst),
		askLimiter:    httpmw.NewUserRateLimiter(cfg.AskRateLimitPerMin/60, cfg.AskRateLimitBurst),
		metrics:       metrics.New(),
	}
	srv.importLimiter.StartCleanup(10*time.Minute, time.Hour)
	srv.askLimiter.StartCleanup(10*time.Minute, time.Hour)
	if cfg.GitHubClientID != "" {
		srv.oauth = auth.NewGitHubOAuth(cfg.GitHubClientID, cfg.GitHubClientSecret, cfg.GitHubRedirectURL)
	}
	if len(aiClient) > 0 && aiClient[0] != nil {
		srv.ai = aiClient[0]
	}
	srv.routes()
	return srv
}

func (s *Server) aiClient() *ai.Client {
	if s.ai != nil {
		return s.ai
	}
	return ai.NewClientFromEnv()
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler().ServeHTTP(w, r)
}

// handler builds the fully-wrapped handler chain once per call rather
// than once at construction so that logger/config swaps in tests are
// picked up; the cost is negligible (a handful of closures) next to an
// HTTP round trip.
func (s *Server) handler() http.Handler {
	var h http.Handler = s.mux
	h = withCORS(s.cfg.FrontendOrigin)(h)
	h = httpmw.Timeout(s.cfg.RequestTimeout, `{"error":"request timed out"}`)(h)
	h = httpmw.AccessLog(s.logger)(h)
	h = httpmw.Recover(s.logger)(h)
	h = httpmw.RequestID(h)
	// metrics wraps everything, including panic recovery, so a recovered
	// panic's resulting 500 is counted rather than silently dropped (see
	// Collector.Middleware's doc comment).
	h = s.metrics.Middleware(h)
	return h
}

func (s *Server) requireAuth(next http.HandlerFunc) http.Handler {
	return httpmw.RequireAuth(s.cfg.JWTSigningKey, s.cfg.SessionCookieName, s.data)(next)
}

// rateLimited wraps next with a per-user token-bucket check. Must sit
// inside requireAuth (i.e. requireAuth(rateLimited(...))) so the user is
// already attached to the request context by the time it runs.
func (s *Server) rateLimited(limiter *httpmw.UserRateLimiter, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := httpmw.UserFromContext(r.Context())
		if !ok {
			writeErr(w, http.StatusUnauthorized, "not signed in")
			return
		}
		if !limiter.Allow(user.ID) {
			writeErr(w, http.StatusTooManyRequests, "too many requests, please slow down")
			return
		}
		next(w, r)
	}
}

func (s *Server) routes() {
	s.mux.HandleFunc("/api/health", s.handleHealth)
	s.mux.HandleFunc("/api/ready", s.handleReady)
	// Unauthenticated, like /api/health and /api/ready: it exposes only
	// aggregate counts/latency, no per-user or per-repo data, matching
	// what Cloud Run's own scraping/dashboards can already reach without
	// a credential.
	s.mux.HandleFunc("GET /metrics", s.handleMetrics)

	s.mux.HandleFunc("GET /auth/github/login", s.handleGitHubLogin)
	s.mux.HandleFunc("GET /auth/github/callback", s.handleGitHubCallback)
	s.mux.Handle("POST /auth/logout", s.requireAuth(s.handleLogout))
	s.mux.Handle("GET /api/me", s.requireAuth(s.handleMe))

	s.mux.Handle("POST /api/import", s.requireAuth(s.rateLimited(s.importLimiter, s.handleImport)))
	s.mux.Handle("GET /api/repos", s.requireAuth(s.handleListRepos))
	s.mux.Handle("DELETE /api/repos/{id}", s.requireAuth(s.handleDeleteRepo))
	s.mux.Handle("POST /api/ask", s.requireAuth(s.rateLimited(s.askLimiter, s.handleAsk)))
	s.mux.Handle("GET /api/repos/{id}/file", s.requireAuth(s.handleFileContent))
	s.mux.Handle("POST /api/repos/{id}/diagram", s.requireAuth(s.handleDiagram))
	// Shares askLimiter with /api/ask: search is equally AI-cost-bearing
	// (see semsearch.Search), even though the original gap list only
	// called out import/ask explicitly.
	s.mux.Handle("POST /api/repos/{id}/search", s.requireAuth(s.rateLimited(s.askLimiter, s.handleSearch)))

	s.mux.Handle("/", http.FileServer(http.Dir("./frontend")))
}

// withCORS enforces a default-deny cross-origin policy: credentialed
// requests are only allowed from the single, explicitly configured
// FRONTEND_ORIGIN. It never reflects an arbitrary request Origin back to
// the client — doing so combined with Allow-Credentials: true would let
// any website read authenticated responses from a logged-in user's
// browser. When FRONTEND_ORIGIN is unset (e.g. local dev with no
// frontend config), CORS headers are simply omitted rather than opened
// up, so browsers fall back to same-origin-only behavior.
func withCORS(frontendOrigin string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if frontendOrigin != "" && origin == frontendOrigin {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				w.Header().Set("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusOK)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// handleHealth is Cloud Run's liveness probe target: it must answer
// immediately and never depend on the database, so a DB outage doesn't
// get the container killed on top of the outage already in progress.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	version := s.Version
	if version == "" {
		version = "dev"
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": version})
}

// handleReady is Cloud Run's readiness probe target: unlike /api/health,
// this does check the database, so a Cloud Run instance that can't reach
// Cloud SQL is taken out of rotation instead of receiving traffic it
// can't serve.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	if err := s.data.PingContext(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "not ready",
			"error":  "database unreachable",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// handleMetrics serves a point-in-time snapshot of request/error counts
// and latency percentiles collected by s.metrics. See internal/metrics's
// package doc for scope and why this isn't a Prometheus-format endpoint.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.metrics.Snapshot())
}
