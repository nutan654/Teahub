// Package store holds the *runtime* state of imported repositories:
// parsed file bodies, the static-analysis index, and the cached
// architecture diagram. None of this is persisted — see
// ARCHITECTURE.md "Storage model" for why (large file bodies don't
// belong in Postgres, and the analysis index is cheap to rebuild from
// the on-disk clone).
//
// Durable metadata (id, url, owner, status, file tree, timestamps) lives
// in Postgres via the dbstore package. This package is a pure in-process
// cache keyed by repository ID: on a cold cache miss (e.g. right after a
// restart) the api layer rebuilds an entry from the on-disk clone before
// serving the request — see api.Server.runtimeRepo.
package store

import (
	"sync"
	"time"

	"teahub/backend/internal/index"
)

// FileNode is a single node (file or directory) in a repo's tree.
type FileNode struct {
	Path     string      `json:"path"` // relative to repo root, e.g. "src/auth/jwt.go"
	IsDir    bool        `json:"isDir"`
	Size     int64       `json:"size,omitempty"`
	Children []*FileNode `json:"children,omitempty"`
}

// Repo is the full runtime view of one imported repository: durable
// fields mirrored from Postgres plus in-memory-only fields rebuilt on
// demand. JSON tags control what's exposed to API clients (repository
// list/detail responses).
type Repo struct {
	ID         string    `json:"id"`
	OwnerID    string    `json:"-"`
	URL        string    `json:"url"`
	LocalPath  string    `json:"-"` // filesystem path where it was cloned, not exposed to clients
	Status     string    `json:"status"`
	ImportedAt time.Time `json:"importedAt"`
	FileCount  int       `json:"fileCount"`
	Tree       *FileNode `json:"tree"`

	// Files maps relative path -> text content, for files we decided were
	// worth indexing (small enough, not binary). Used for keyword retrieval
	// when answering questions. In-memory only; rebuilt from disk on cache
	// miss.
	Files map[string]string `json:"-"`

	// DiagramMermaid/DiagramExplanation cache the last generated architecture
	// diagram so repeat views don't re-spend an API call. Not persisted:
	// a cold cache after restart just regenerates on next request.
	DiagramMermaid     string `json:"-"`
	DiagramExplanation string `json:"-"`

	// Index is the repo's static-analysis index (see the index package):
	// built once when the runtime cache entry is created, and never
	// re-walked afterward. Index is itself safe for concurrent use (it
	// has its own internal lock), so it doesn't need to go through the
	// Store's lock the way DiagramMermaid does via SetDiagram — callers
	// just read the pointer via Get and call its methods directly.
	Index *index.Index `json:"-"`
}

// Store is an in-process, concurrency-safe cache of Repo runtime state,
// keyed by repository ID (the Postgres UUID). It holds no metadata that
// isn't also in Postgres except the fields explicitly marked in-memory
// above.
type Store struct {
	mu    sync.RWMutex
	repos map[string]*Repo
}

func New() *Store {
	return &Store{repos: make(map[string]*Repo)}
}

func (s *Store) Put(r *Repo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repos[r.ID] = r
}

func (s *Store) Get(id string) (*Repo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.repos[id]
	return r, ok
}

// Delete evicts a repo from the runtime cache (called after the DB row is
// deleted, so a subsequent request can't keep serving cached content for
// a repo that no longer officially exists).
func (s *Store) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.repos, id)
}

// SetDiagram caches a generated diagram on the repo. Goes through the store's
// own lock (rather than a separate per-Repo lock) since this is the only
// mutation path for a Repo after it's first stored — keeping locking in one
// place avoids the two-locks-one-object bugs that creep in otherwise.
func (s *Store) SetDiagram(id, mermaid, explanation string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.repos[id]
	if !ok {
		return false
	}
	r.DiagramMermaid = mermaid
	r.DiagramExplanation = explanation
	return true
}
