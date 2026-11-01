// Demo Mode: a handful of unauthenticated, read-only endpoints backed by
// exactly one repository, so someone evaluating TeaHub (a recruiter, a
// reviewer, anyone following a shared link) can use every real feature —
// explorer, Q&A, semantic search, architecture diagram — without GitHub
// OAuth, Postgres, or a Gemini API key of their own.
//
// Design choices, and why:
//
//   - The demo repo is cloned and indexed exactly once, at server
//     startup (see ImportDemoRepo, called from main.go), not per visitor
//     and not on first request. This matches the "pre-indexed, not
//     cloned on click" requirement: every visitor hits an already-warm
//     in-process cache entry, so the demo feels instant and never pays a
//     clone cost on someone else's behalf.
//   - It's stored in the same store.Store the authenticated flow uses,
//     under the fixed ID demoRepoID — but it is NEVER written to
//     Postgres and has no owner. This deliberately keeps Demo Mode
//     outside the ownership/ACL system entirely rather than inventing a
//     synthetic "demo user" account, since the demo repo isn't
//     private data that needs ownership scoping in the first place.
//   - Every demo handler is unauthenticated (mounted directly on the
//     mux, not behind requireAuth) but still rate-limited — by caller IP
//     via httpmw.ClientIP, since there's no user ID to key on — for the
//     two calls that spend real Gemini API budget (ask, search). The
//     diagram endpoint is deliberately NOT separately rate-limited
//     beyond that: it's generated once total (see handleDemoDiagram)
//     and served from cache to every subsequent visitor, so it can't be
//     abused into repeat API spend no matter how many people click it.
//   - If DemoRepoURL is unset, none of this is wired up at all (see
//     Server.routes) — Demo Mode has zero cost/surface for a deployment
//     that doesn't opt in.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"teahub/backend/internal/ai"
	"teahub/backend/internal/diagram"
	"teahub/backend/internal/fsread"
	"teahub/backend/internal/gitclone"
	"teahub/backend/internal/httpmw"
	"teahub/backend/internal/index"
	"teahub/backend/internal/parser"
	"teahub/backend/internal/semsearch"
	"teahub/backend/internal/store"
)

// demoRepoID is the fixed store.Store key for the demo repository. It is
// intentionally not a UUID (unlike every real, database-backed repo ID)
// so it can never collide with one, and so its presence in a log line is
// self-explanatory.
const demoRepoID = "demo"

// ImportDemoRepo clones and indexes cfg.DemoRepoURL once and stores the
// result under demoRepoID, exactly the way handleImport does for a real
// user's repo — same clone/parse/index pipeline, just skipping the
// Postgres write and ownership fields, since the demo repo belongs to no
// one and is world-readable by design.
//
// Called once from main.go after the server is constructed. A failure
// here (bad URL, network blip, git not installed) is logged and Demo
// Mode simply stays unavailable (every /api/demo/* route returns 503)
// rather than crashing the whole server over a non-essential feature.
func (s *Server) ImportDemoRepo(ctx context.Context) error {
	if s.cfg.DemoRepoURL == "" {
		return nil
	}

	repoURL, err := gitclone.Normalize(s.cfg.DemoRepoURL)
	if err != nil {
		return fmt.Errorf("normalizing DEMO_REPO_URL: %w", err)
	}

	localPath, err := gitclone.CloneInto(ctx, repoURL, s.cfg.RepoStorageDir, demoRepoID)
	if err != nil {
		return fmt.Errorf("cloning demo repo: %w", err)
	}

	tree, indexed, fileCount, err := parser.Walk(localPath)
	if err != nil {
		return fmt.Errorf("parsing demo repo: %w", err)
	}

	symbolIndex, idxErr := index.Build(ctx, indexed, index.Options{})
	if idxErr != nil {
		s.logger.Warn("demo_index_build_warnings", "error", idxErr)
	}
	fileCountIndexed, symbolCount := symbolIndex.Stats()

	s.cache.Put(&store.Repo{
		ID:        demoRepoID,
		URL:       repoURL,
		LocalPath: localPath,
		Status:    "ready",
		FileCount: fileCount,
		Tree:      tree,
		Files:     indexed,
		Index:     symbolIndex,
	})

	s.logger.Info("demo_repo_ready", "url", repoURL, "files_analyzed", fileCountIndexed, "symbols", symbolCount)
	return nil
}

// demoRepo fetches the pre-indexed demo repo from the runtime cache, or
// writes a 503 and returns ok=false if Demo Mode isn't configured or
// hasn't finished importing yet (e.g. briefly during startup).
func (s *Server) demoRepo(w http.ResponseWriter) (*store.Repo, bool) {
	repo, ok := s.cache.Get(demoRepoID)
	if !ok {
		writeErr(w, http.StatusServiceUnavailable, "demo mode is not available right now")
		return nil, false
	}
	return repo, true
}

// demoRateLimited mirrors Server.rateLimited but keys the limiter by
// caller IP instead of an authenticated user ID, since demo visitors
// never sign in.
func (s *Server) demoRateLimited(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.demoLimiter.Allow(httpmw.ClientIP(r)) {
			writeErr(w, http.StatusTooManyRequests, "too many demo requests, please slow down")
			return
		}
		next(w, r)
	}
}

// handleDemoInfo returns a summary of the demo repository — enough for
// the frontend to render it exactly like a freshly imported repo (same
// shape as the POST /api/import response), plus a couple of
// index-derived stats a recruiter-facing landing page wants to show at a
// glance (files analyzed, symbols found).
func (s *Server) handleDemoInfo(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.demoRepo(w)
	if !ok {
		return
	}
	filesAnalyzed, symbolCount := 0, 0
	if repo.Index != nil {
		filesAnalyzed, symbolCount = repo.Index.Stats()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            repo.ID,
		"url":           repo.URL,
		"status":        repo.Status,
		"fileCount":     repo.FileCount,
		"tree":          repo.Tree,
		"filesAnalyzed": filesAnalyzed,
		"symbolCount":   symbolCount,
		"demo":          true,
	})
}

// handleDemoFile mirrors handleFileContent: sandboxed single-file reads
// via fsread, just without an ownership check (the demo repo has no
// owner) and without requireAuth in front of it.
func (s *Server) handleDemoFile(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.demoRepo(w)
	if !ok {
		return
	}
	relPath := r.URL.Query().Get("path")
	if relPath == "" {
		writeErr(w, http.StatusBadRequest, "missing required query param: path")
		return
	}
	fc, err := fsread.Read(repo.LocalPath, relPath)
	if err != nil {
		switch {
		case errors.Is(err, fsread.ErrOutsideRepo):
			writeErr(w, http.StatusBadRequest, "path escapes repository root")
		case errors.Is(err, fsread.ErrNotFound):
			writeErr(w, http.StatusNotFound, "file not found")
		case errors.Is(err, fsread.ErrIsDir):
			writeErr(w, http.StatusBadRequest, "path is a directory, not a file")
		default:
			writeErr(w, http.StatusInternalServerError, "reading file: "+err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, fc)
}

// handleDemoAsk mirrors handleAsk against the fixed demo repo. The
// request body's repoId (if any) is ignored — there is exactly one demo
// repo, so there's nothing to disambiguate.
func (s *Server) handleDemoAsk(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.demoRepo(w)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxRequestBodyBytes)
	var req askRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json body, or body exceeds the size limit")
		return
	}
	if utf8.RuneCountInString(req.Question) > s.cfg.MaxQuestionLength {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("question exceeds the %d character limit", s.cfg.MaxQuestionLength))
		return
	}
	if strings.TrimSpace(req.Question) == "" {
		writeErr(w, http.StatusBadRequest, "question must not be empty")
		return
	}

	paths := ai.Retrieve(repo.Files, req.Question, 8)
	repoContext := ai.BuildContext(topLevelSummary(repo.Tree), repo.Files, paths)

	sys := "You are TeaHub, an AI engineering assistant analyzing a specific codebase for a developer. " +
		"Answer using only the repository context provided below. Cite file paths for any claim about " +
		"where something is implemented. If the context doesn't contain the answer, say so plainly " +
		"instead of guessing."
	answer, err := s.aiClient().Complete(r.Context(), sys, repoContext+"\n\nQUESTION: "+req.Question)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"answer":      answer,
		"sourceFiles": paths,
	})
}

// handleDemoSearch mirrors handleSearch against the fixed demo repo.
func (s *Server) handleDemoSearch(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.demoRepo(w)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxRequestBodyBytes)
	var req searchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json body, or body exceeds the size limit")
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeErr(w, http.StatusBadRequest, "query must not be empty")
		return
	}
	if utf8.RuneCountInString(req.Query) > s.cfg.MaxSearchQueryLength {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("query exceeds the %d character limit", s.cfg.MaxSearchQueryLength))
		return
	}
	results, err := semsearch.Search(r.Context(), s.aiClient(), repo, req.Query)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// handleDemoDiagram mirrors handleDiagram, with one deliberate
// simplification: there is no ?refresh=true escape hatch. The demo
// diagram is generated once (by whichever visitor happens to be first)
// and served from cache to everyone after that — an unauthenticated,
// publicly linked endpoint has no legitimate reason to force-regenerate
// (and re-spend an API call) on every visitor's whim the way a repo
// owner refreshing their own diagram does.
func (s *Server) handleDemoDiagram(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.demoRepo(w)
	if !ok {
		return
	}
	if repo.DiagramMermaid != "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"mermaid":     repo.DiagramMermaid,
			"explanation": repo.DiagramExplanation,
<<<<<<< HEAD
=======
			"fallback":    diagram.FallbackMermaid(repo),
>>>>>>> 3edc2f6 (Update TeaHub)
			"cached":      true,
		})
		return
	}
	result, err := diagram.Generate(r.Context(), s.aiClient(), repo)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	s.cache.SetDiagram(demoRepoID, result.Mermaid, result.Explanation)
	writeJSON(w, http.StatusOK, map[string]any{
		"mermaid":     result.Mermaid,
		"explanation": result.Explanation,
<<<<<<< HEAD
=======
		"fallback":    diagram.FallbackMermaid(repo),
		"source":      result.Source,
>>>>>>> 3edc2f6 (Update TeaHub)
		"cached":      false,
	})
}
