package api

import (
	"context"
	"time"

	"teahub/backend/internal/dbstore"
)

// UserStore is the subset of dbstore.DB needed for authentication. A
// separate interface (rather than one giant DataStore) so a test that
// only exercises repo endpoints doesn't need to fake user methods too.
type UserStore interface {
	UpsertGitHubUser(ctx context.Context, githubID int64, login, displayName, avatarURL, email string) (*dbstore.User, error)
	GetUserByID(ctx context.Context, id string) (*dbstore.User, error)
	CreateSession(ctx context.Context, userID, jwtID, userAgent, ipAddress string, expiresAt time.Time) error
	IsSessionRevoked(ctx context.Context, jwtID string) (bool, error)
	RevokeSession(ctx context.Context, jwtID string) error
	RevokeAllSessionsForUser(ctx context.Context, userID string) error
}

// RepoStore is the subset of dbstore.DB needed for repository metadata
// persistence.
type RepoStore interface {
	CreateRepository(ctx context.Context, ownerID, url string) (*dbstore.Repository, error)
	UpdateRepositoryResult(ctx context.Context, id, localPath, status, errMsg string, fileCount int, tree any) error
	UpdateRepositoryStatus(ctx context.Context, id, status string) error
	GetRepositoryForOwner(ctx context.Context, id, ownerID string) (*dbstore.Repository, error)
	ListRepositoriesForOwnerPage(ctx context.Context, ownerID string, limit int, cursor string) ([]*dbstore.Repository, string, error)
	ListAllReadyRepositories(ctx context.Context) ([]*dbstore.Repository, error)
	DeleteRepository(ctx context.Context, id, ownerID string) error
}

// DataStore is everything the api package needs from persistence.
// *dbstore.DB satisfies this directly; tests use an in-memory fake (see
// handlers_test.go) so HTTP/auth/ownership logic can be tested without a
// real Postgres instance. Real DB-query correctness is covered
// separately by dbstore's own integration tests (dbstore/*_integration_test.go),
// which do talk to a real Postgres and are skipped unless
// TEAHUB_TEST_DATABASE_URL is set.
type DataStore interface {
	UserStore
	RepoStore
	Pinger
}

// Pinger lets the readiness endpoint check the DB is actually reachable,
// not just that a *sql.DB handle exists.
type Pinger interface {
	PingContext(ctx context.Context) error
}
