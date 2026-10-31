// Package memstore is a pure in-memory implementation of the interfaces
// api.DataStore requires, satisfying the exact same method set as
// *dbstore.DB. It exists for two purposes only:
//
//  1. TEAHUB_IN_MEMORY_STORE=true, a local-dev convenience that lets
//     someone run `go run ./backend/cmd/server` without a Postgres
//     instance while hacking on something unrelated to persistence. It
//     must never be set in production — config.Load enforces that.
//  2. api package unit tests (see api/handlers_test.go), which exercise
//     HTTP routing, auth enforcement, and ownership checks without
//     needing a real Postgres connection. Correctness of the actual SQL
//     lives in dbstore's own integration tests instead.
//
// Data here does not survive a restart — that's the entire point of the
// real dbstore.DB implementation existing.
package memstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"teahub/backend/internal/dbstore"
)

type MemStore struct {
	mu sync.Mutex

	usersByID     map[string]*dbstore.User
	usersByGithub map[int64]*dbstore.User

	repos map[string]*dbstore.Repository

	sessions map[string]sessionRecord // jwtID -> record
}

type sessionRecord struct {
	userID    string
	expiresAt time.Time
	revoked   bool
}

func New() *MemStore {
	return &MemStore{
		usersByID:     map[string]*dbstore.User{},
		usersByGithub: map[int64]*dbstore.User{},
		repos:         map[string]*dbstore.Repository{},
		sessions:      map[string]sessionRecord{},
	}
}

func newUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b[0:4]) + "-" + hex.EncodeToString(b[4:6]) + "-" +
		hex.EncodeToString(b[6:8]) + "-" + hex.EncodeToString(b[8:10]) + "-" + hex.EncodeToString(b[10:16])
}

// PingContext satisfies api.Pinger; there's no real connection to check,
// so it always succeeds (in-memory mode's readiness is not meaningful).
func (m *MemStore) PingContext(ctx context.Context) error { return nil }

func (m *MemStore) UpsertGitHubUser(ctx context.Context, githubID int64, login, displayName, avatarURL, email string) (*dbstore.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	if u, ok := m.usersByGithub[githubID]; ok {
		u.GitHubLogin, u.DisplayName, u.AvatarURL, u.Email = login, displayName, avatarURL, email
		u.UpdatedAt = now
		return u, nil
	}
	u := &dbstore.User{
		ID: newUUID(), GitHubID: githubID, GitHubLogin: login,
		DisplayName: displayName, AvatarURL: avatarURL, Email: email,
		CreatedAt: now, UpdatedAt: now,
	}
	m.usersByGithub[githubID] = u
	m.usersByID[u.ID] = u
	return u, nil
}

func (m *MemStore) GetUserByID(ctx context.Context, id string) (*dbstore.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.usersByID[id]
	if !ok {
		return nil, dbstore.ErrNotFound
	}
	return u, nil
}

func (m *MemStore) CreateSession(ctx context.Context, userID, jwtID, userAgent, ipAddress string, expiresAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[jwtID] = sessionRecord{userID: userID, expiresAt: expiresAt}
	return nil
}

func (m *MemStore) IsSessionRevoked(ctx context.Context, jwtID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.sessions[jwtID]
	if !ok {
		return false, nil
	}
	return rec.revoked || time.Now().After(rec.expiresAt), nil
}

func (m *MemStore) RevokeSession(ctx context.Context, jwtID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rec, ok := m.sessions[jwtID]; ok {
		rec.revoked = true
		m.sessions[jwtID] = rec
	}
	return nil
}

func (m *MemStore) RevokeAllSessionsForUser(ctx context.Context, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, rec := range m.sessions {
		if rec.userID == userID {
			rec.revoked = true
			m.sessions[id] = rec
		}
	}
	return nil
}

func (m *MemStore) CreateRepository(ctx context.Context, ownerID, url string) (*dbstore.Repository, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.repos {
		if r.OwnerID == ownerID && r.URL == url {
			return nil, dbstore.ErrAlreadyExists
		}
	}
	now := time.Now()
	r := &dbstore.Repository{
		ID: newUUID(), OwnerID: ownerID, URL: url, Status: dbstore.StatusPending,
		ImportedAt: now, UpdatedAt: now,
	}
	m.repos[r.ID] = r
	return cloneRepo(r), nil
}

func (m *MemStore) UpdateRepositoryResult(ctx context.Context, id, localPath, status, errMsg string, fileCount int, tree any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.repos[id]
	if !ok {
		return dbstore.ErrNotFound
	}
	r.LocalPath, r.Status, r.ErrorMessage, r.FileCount = localPath, status, errMsg, fileCount
	r.UpdatedAt = time.Now()
	if tree != nil {
		if b, err := marshalTree(tree); err == nil {
			r.TreeJSON = b
		}
	}
	return nil
}

func (m *MemStore) UpdateRepositoryStatus(ctx context.Context, id, status string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.repos[id]
	if !ok {
		return dbstore.ErrNotFound
	}
	r.Status = status
	r.UpdatedAt = time.Now()
	return nil
}

func (m *MemStore) GetRepositoryForOwner(ctx context.Context, id, ownerID string) (*dbstore.Repository, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.repos[id]
	if !ok || r.OwnerID != ownerID {
		return nil, dbstore.ErrNotFound
	}
	return cloneRepo(r), nil
}

// ListRepositoriesForOwnerPage mirrors dbstore.DB's keyset-paginated
// query: same ordering (imported_at DESC, id DESC), same cursor encoding,
// so handler tests exercise the exact pagination behavior production
// sees, not an approximation of it.
func (m *MemStore) ListRepositoriesForOwnerPage(ctx context.Context, ownerID string, limit int, cursor string) ([]*dbstore.Repository, string, error) {
	m.mu.Lock()
	var all []*dbstore.Repository
	for _, r := range m.repos {
		if r.OwnerID == ownerID {
			all = append(all, cloneRepo(r))
		}
	}
	m.mu.Unlock()
	sortReposNewestFirst(all)

	start := 0
	if cursor != "" {
		importedAt, id, err := dbstore.DecodeRepoCursorForTests(cursor)
		if err != nil {
			return nil, "", dbstore.ErrInvalidCursor
		}
		for i, r := range all {
			if r.ImportedAt.Before(importedAt) || (r.ImportedAt.Equal(importedAt) && r.ID < id) {
				start = i
				break
			}
			start = i + 1
		}
	}

	end := start + limit
	var nextCursor string
	if end < len(all) {
		last := all[end-1]
		nextCursor = dbstore.EncodeRepoCursorForTests(last.ImportedAt, last.ID)
	} else {
		end = len(all)
	}
	if start > len(all) {
		start = len(all)
	}
	return all[start:end], nextCursor, nil
}

func sortReposNewestFirst(repos []*dbstore.Repository) {
	sort.Slice(repos, func(i, j int) bool {
		if repos[i].ImportedAt.Equal(repos[j].ImportedAt) {
			return repos[i].ID > repos[j].ID
		}
		return repos[i].ImportedAt.After(repos[j].ImportedAt)
	})
}

func (m *MemStore) ListAllReadyRepositories(ctx context.Context) ([]*dbstore.Repository, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*dbstore.Repository
	for _, r := range m.repos {
		if r.Status == dbstore.StatusReady {
			out = append(out, cloneRepo(r))
		}
	}
	return out, nil
}

func (m *MemStore) DeleteRepository(ctx context.Context, id, ownerID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.repos[id]
	if !ok || r.OwnerID != ownerID {
		return dbstore.ErrNotFound
	}
	delete(m.repos, id)
	return nil
}

func cloneRepo(r *dbstore.Repository) *dbstore.Repository {
	cp := *r
	return &cp
}

func marshalTree(tree any) ([]byte, error) {
	return json.Marshal(tree)
}
