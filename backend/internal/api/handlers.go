package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"teahub/backend/internal/ai"
	"teahub/backend/internal/dbstore"
	"teahub/backend/internal/diagram"
	"teahub/backend/internal/fsread"
	"teahub/backend/internal/gitclone"
	"teahub/backend/internal/httpmw"
	"teahub/backend/internal/index"
	"teahub/backend/internal/parser"
	"teahub/backend/internal/semsearch"
	"teahub/backend/internal/store"
)

type importRequest struct {
	URL string `json:"url"`
}

// handleImport clones and indexes a repository, persists its metadata to
// Postgres scoped to the authenticated caller, and populates the runtime
// cache. Cloning/indexing happens synchronously within the request for
// the same reason the original MVP did it synchronously: it keeps the
// import flow simple to reason about and test, at the cost of the HTTP
// request taking as long as the clone+index does. See ROADMAP.md for the
// documented async-import follow-up.
func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	user, ok := httpmw.UserFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "not signed in")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxRequestBodyBytes)
	var req importRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json body, or body exceeds the size limit")
		return
	}

	repoURL, err := gitclone.Normalize(req.URL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	dbRepo, err := s.data.CreateRepository(r.Context(), user.ID, repoURL)
	if err != nil {
		if errors.Is(err, dbstore.ErrAlreadyExists) {
			writeErr(w, http.StatusConflict, "you already imported this repository")
			return
		}
		s.logger.Error("create_repository_failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "recording repository failed")
		return
	}

	s.logger.Info("importing_repo", "repo_id", dbRepo.ID, "url", repoURL, "owner_id", user.ID)
	_ = s.data.UpdateRepositoryStatus(r.Context(), dbRepo.ID, dbstore.StatusCloning)

	localPath, err := gitclone.CloneInto(r.Context(), repoURL, s.cfg.RepoStorageDir, dbRepo.ID)
	if err != nil {
		s.markImportFailed(r.Context(), dbRepo.ID, "clone failed: "+err.Error())
		writeErr(w, http.StatusBadGateway, "clone failed: "+err.Error())
		return
	}

	_ = s.data.UpdateRepositoryStatus(r.Context(), dbRepo.ID, dbstore.StatusIndexing)

	tree, indexed, fileCount, err := parser.Walk(localPath)
	if err != nil {
		s.markImportFailed(r.Context(), dbRepo.ID, "parse failed: "+err.Error())
		writeErr(w, http.StatusInternalServerError, "parse failed: "+err.Error())
		return
	}

	// Build the static-analysis index once, here, at import time. This is
	// the only place a repo's files are ever walked/analyzed for indexing
	// purposes — Search, the Explorer, and any future feature that reuses
	// this index all read from the already-built result, never re-walk.
	symbolIndex, idxErr := index.Build(r.Context(), indexed, index.Options{})
	if idxErr != nil {
		s.logger.Warn("index_build_warnings", "repo_id", dbRepo.ID, "error", idxErr)
	}
	fileCountIndexed, symbolCount := symbolIndex.Stats()
	s.logger.Info("indexed_repo", "repo_id", dbRepo.ID, "files_analyzed", fileCountIndexed, "symbols", symbolCount)

	if err := s.data.UpdateRepositoryResult(r.Context(), dbRepo.ID, localPath, dbstore.StatusReady, "", fileCount, tree); err != nil {
		s.logger.Error("persist_repository_result_failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "saving repository result failed")
		return
	}

	repo := &store.Repo{
		ID:         dbRepo.ID,
		OwnerID:    user.ID,
		URL:        repoURL,
		LocalPath:  localPath,
		Status:     dbstore.StatusReady,
		ImportedAt: dbRepo.ImportedAt,
		FileCount:  fileCount,
		Tree:       tree,
		Files:      indexed,
		Index:      symbolIndex,
	}
	s.cache.Put(repo)

	writeJSON(w, http.StatusOK, repo)
}

func (s *Server) markImportFailed(ctx context.Context, id, msg string) {
	if err := s.data.UpdateRepositoryResult(ctx, id, "", dbstore.StatusFailed, msg, 0, nil); err != nil {
		s.logger.Error("mark_import_failed_persist_error", "error", err)
	}
}

// defaultRepoListLimit / maxRepoListLimit bound the ?limit= query param
// on GET /api/repos. A default keeps the response small even when the
// caller doesn't ask for pagination explicitly; the max stops a caller
// from requesting an unbounded page (defeating the point of pagination)
// via a huge limit value.
const (
	defaultRepoListLimit = 20
	maxRepoListLimit     = 100
)

func (s *Server) handleListRepos(w http.ResponseWriter, r *http.Request) {
	user, ok := httpmw.UserFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "not signed in")
		return
	}

	limit := defaultRepoListLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeErr(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = n
	}
	if limit > maxRepoListLimit {
		limit = maxRepoListLimit
	}
	cursor := r.URL.Query().Get("cursor")

	rows, nextCursor, err := s.data.ListRepositoriesForOwnerPage(r.Context(), user.ID, limit, cursor)
	if errors.Is(err, dbstore.ErrInvalidCursor) {
		writeErr(w, http.StatusBadRequest, "invalid cursor")
		return
	}
	if err != nil {
		s.logger.Error("list_repositories_failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "listing repositories failed")
		return
	}
	out := make([]*store.Repo, 0, len(rows))
	for _, row := range rows {
		out = append(out, dbRowToAPIRepo(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"repos":      out,
		"nextCursor": nextCursor,
	})
}

func (s *Server) handleDeleteRepo(w http.ResponseWriter, r *http.Request) {
	user, ok := httpmw.UserFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "not signed in")
		return
	}
	id := r.PathValue("id")

	dbRepo, err := s.data.GetRepositoryForOwner(r.Context(), id, user.ID)
	if errors.Is(err, dbstore.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "unknown repo id")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "looking up repository failed")
		return
	}

	if err := s.data.DeleteRepository(r.Context(), id, user.ID); err != nil {
		s.logger.Error("delete_repository_failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "deleting repository failed")
		return
	}
	s.cache.Delete(id)
	if err := gitclone.Remove(dbRepo.LocalPath); err != nil {
		// Non-fatal: the DB row (source of truth for "does this repo
		// exist") is already gone. A leftover directory just wastes disk
		// until the next cleanup pass; log it so it's visible in metrics.
		s.logger.Warn("repo_directory_cleanup_failed", "repo_id", id, "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// repoForCaller loads a repository's runtime state (Files/Index/Tree),
// enforcing ownership via the database on every call — never trusting
// the in-memory cache's presence as a proxy for "this caller may access
// this repo". On a cache miss it rehydrates by re-parsing the on-disk
// clone (re-cloning first if that path is gone, e.g. after a restart on
// ephemeral storage — see gitclone.CloneInto's doc comment).
func (s *Server) repoForCaller(ctx context.Context, ownerID, id string) (*store.Repo, error) {
	dbRepo, err := s.data.GetRepositoryForOwner(ctx, id, ownerID)
	if err != nil {
		return nil, err
	}
	if dbRepo.Status != dbstore.StatusReady {
		return nil, errRepoNotReady
	}

	if cached, ok := s.cache.Get(id); ok {
		return cached, nil
	}

	return s.rehydrate(ctx, dbRepo)
}

var errRepoNotReady = errors.New("repository is not ready yet")

// rehydrate rebuilds a runtime cache entry from disk (re-cloning first if
// necessary) for a repo whose metadata exists in Postgres but whose
// in-memory Files/Index were lost, e.g. because this is a fresh instance
// that never imported it. This is what makes "repository metadata
// survives server restarts" true end-to-end rather than just true for
// the tree/status fields.
func (s *Server) rehydrate(ctx context.Context, dbRepo *dbstore.Repository) (*store.Repo, error) {
	localPath := dbRepo.LocalPath
	if _, err := os.Stat(localPath); err != nil {
		s.logger.Info("rehydrate_recloning", "repo_id", dbRepo.ID, "reason", "local clone missing")
		newPath, err := gitclone.CloneInto(ctx, dbRepo.URL, s.cfg.RepoStorageDir, dbRepo.ID)
		if err != nil {
			return nil, err
		}
		localPath = newPath
	}

	tree, indexed, fileCount, err := parser.Walk(localPath)
	if err != nil {
		return nil, err
	}
	symbolIndex, idxErr := index.Build(ctx, indexed, index.Options{})
	if idxErr != nil {
		s.logger.Warn("rehydrate_index_warnings", "repo_id", dbRepo.ID, "error", idxErr)
	}

	repo := &store.Repo{
		ID:         dbRepo.ID,
		OwnerID:    dbRepo.OwnerID,
		URL:        dbRepo.URL,
		LocalPath:  localPath,
		Status:     dbstore.StatusReady,
		ImportedAt: dbRepo.ImportedAt,
		FileCount:  fileCount,
		Tree:       tree,
		Files:      indexed,
		Index:      symbolIndex,
	}
	s.cache.Put(repo)

	if localPath != dbRepo.LocalPath {
		_ = s.data.UpdateRepositoryResult(ctx, dbRepo.ID, localPath, dbstore.StatusReady, "", fileCount, tree)
	}
	return repo, nil
}

// dbRowToAPIRepo converts a persisted metadata row into the API's Repo
// shape for list responses, without touching the runtime cache — list
// views don't need file bodies or the analysis index, just status/tree.
func dbRowToAPIRepo(row *dbstore.Repository) *store.Repo {
	r := &store.Repo{
		ID:         row.ID,
		OwnerID:    row.OwnerID,
		URL:        row.URL,
		Status:     row.Status,
		ImportedAt: row.ImportedAt,
		FileCount:  row.FileCount,
	}
	if len(row.TreeJSON) > 0 {
		var tree store.FileNode
		if err := json.Unmarshal(row.TreeJSON, &tree); err == nil {
			r.Tree = &tree
		}
	}
	return r
}

type askRequest struct {
	RepoID   string `json:"repoId"`
	Question string `json:"question"`
}

func topLevelSummary(root *store.FileNode) string {
	var b strings.Builder
	var walk func(n *store.FileNode, depth int)
	walk = func(n *store.FileNode, depth int) {
		if depth > 2 {
			return
		}
		for _, c := range n.Children {
			b.WriteString(strings.Repeat("  ", depth))
			if c.IsDir {
				b.WriteString(c.Path[strings.LastIndex(c.Path, "/")+1:] + "/\n")
				walk(c, depth+1)
			} else {
				b.WriteString(c.Path[strings.LastIndex(c.Path, "/")+1:] + "\n")
			}
		}
	}
	walk(root, 0)
	return b.String()
}

func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	user, ok := httpmw.UserFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "not signed in")
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

	repo, err := s.repoForCaller(r.Context(), user.ID, req.RepoID)
	if repoLookupError(w, err) {
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

// repoLookupError centralizes the "repo, err := ...; if err != nil" ->
// status code mapping shared by every {id}-scoped handler, so ownership
// failures and not-found failures always produce the same status/message
// (a 404, not a 403, for someone else's repo — see ARCHITECTURE.md
// "Security considerations" for why: a 403 would confirm the repo ID
// exists, leaking its existence to a caller who doesn't own it).
func repoLookupError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, dbstore.ErrNotFound):
		writeErr(w, http.StatusNotFound, "unknown repo id, or it doesn't belong to you")
	case errors.Is(err, errRepoNotReady):
		writeErr(w, http.StatusConflict, "repository is still importing")
	default:
		writeErr(w, http.StatusInternalServerError, "loading repository failed")
	}
	return true
}

// handleFileContent serves the raw content of one file within an imported
// repo, for the Interactive Repository Explorer. Path traversal is fully
// delegated to (and blocked by) the fsread package.
func (s *Server) handleFileContent(w http.ResponseWriter, r *http.Request) {
	user, ok := httpmw.UserFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "not signed in")
		return
	}
	id := r.PathValue("id")
	repo, err := s.repoForCaller(r.Context(), user.ID, id)
	if repoLookupError(w, err) {
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

// handleDiagram generates (or returns a cached) architecture diagram for a
// repo. Pass ?refresh=true to force regeneration even if a cached diagram
// exists — regeneration costs a real API call, so callers opt in explicitly.
func (s *Server) handleDiagram(w http.ResponseWriter, r *http.Request) {
	user, ok := httpmw.UserFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "not signed in")
		return
	}
	id := r.PathValue("id")
	repo, err := s.repoForCaller(r.Context(), user.ID, id)
	if repoLookupError(w, err) {
		return
	}

	refresh := r.URL.Query().Get("refresh") == "true"
	if !refresh && repo.DiagramMermaid != "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"mermaid":     repo.DiagramMermaid,
			"explanation": repo.DiagramExplanation,
			"fallback":    diagram.FallbackMermaid(repo),
			"cached":      true,
		})
		return
	}

	result, err := diagram.Generate(r.Context(), s.aiClient(), repo)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}

	s.cache.SetDiagram(id, result.Mermaid, result.Explanation)
	writeJSON(w, http.StatusOK, map[string]any{
		"mermaid":     result.Mermaid,
		"explanation": result.Explanation,
		"fallback":    diagram.FallbackMermaid(repo),
		"source":      result.Source,
		"cached":      false,
	})
}

type searchRequest struct {
	Query string `json:"query"`
}

// handleSearch runs hybrid semantic code search (see semsearch package for
// the full explanation of why it's keyword-prefilter + LLM re-rank rather
// than embeddings) and returns validated, real-path-only results.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	user, ok := httpmw.UserFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "not signed in")
		return
	}
	id := r.PathValue("id")
	repo, err := s.repoForCaller(r.Context(), user.ID, id)
	if repoLookupError(w, err) {
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
