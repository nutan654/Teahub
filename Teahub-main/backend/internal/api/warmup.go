package api

import (
	"context"
	"os"

	"teahub/backend/internal/dbstore"
	"teahub/backend/internal/index"
	"teahub/backend/internal/parser"
	"teahub/backend/internal/store"
)

// WarmCache rehydrates the in-memory runtime cache (Files/Index/Tree) for
// every repository that was "ready" before this process started, so a
// restart doesn't force every repo through a cold-cache re-clone+re-parse
// on its next request. Called once from main.go's run() after the server
// is constructed.
//
// For repos whose local clone is still present on disk (the common case
// on a persistent volume, and possible even on Cloud Run's ephemeral disk
// if this is a rolling restart of the same instance), this eagerly
// parses+indexes and populates the cache — cheap, since it skips the
// network clone. For repos whose local clone is gone, warming synchronously
// would mean startup time scales with fleet size and network conditions;
// instead we log which repos will pay the cold-cache re-clone lazily on
// their first request, same as before this method existed.
func (s *Server) WarmCache(ctx context.Context) {
	repos, err := s.data.ListAllReadyRepositories(ctx)
	if err != nil {
		s.logger.Error("warm_cache_list_failed", "error", err)
		return
	}

	var warmed, deferred int
	for _, dbRepo := range repos {
		if _, statErr := os.Stat(dbRepo.LocalPath); statErr != nil {
			deferred++
			s.logger.Info("warm_cache_deferred", "repo_id", dbRepo.ID, "reason", "local clone missing; will re-clone lazily on first access")
			continue
		}

		if err := s.warmOne(ctx, dbRepo); err != nil {
			deferred++
			s.logger.Warn("warm_cache_repo_failed", "repo_id", dbRepo.ID, "error", err, "detail", "will retry lazily on first access")
			continue
		}
		warmed++
	}

	s.logger.Info("warm_cache_complete", "warmed", warmed, "deferred", deferred, "total", len(repos))
}

// warmOne parses+indexes a repo whose clone already exists on disk and
// puts it in the runtime cache, without touching Postgres (status/tree
// there are already correct from the last successful import).
func (s *Server) warmOne(ctx context.Context, dbRepo *dbstore.Repository) error {
	tree, indexed, fileCount, err := parser.Walk(dbRepo.LocalPath)
	if err != nil {
		return err
	}
	symbolIndex, idxErr := index.Build(ctx, indexed, index.Options{})
	if idxErr != nil {
		s.logger.Warn("warm_cache_index_warnings", "repo_id", dbRepo.ID, "error", idxErr)
	}

	s.cache.Put(&store.Repo{
		ID:         dbRepo.ID,
		OwnerID:    dbRepo.OwnerID,
		URL:        dbRepo.URL,
		LocalPath:  dbRepo.LocalPath,
		Status:     dbstore.StatusReady,
		ImportedAt: dbRepo.ImportedAt,
		FileCount:  fileCount,
		Tree:       tree,
		Files:      indexed,
		Index:      symbolIndex,
	})
	return nil
}
