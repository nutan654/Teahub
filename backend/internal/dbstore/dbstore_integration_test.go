// These tests exercise dbstore against a real PostgreSQL instance. They
// are skipped unless TEAHUB_TEST_DATABASE_URL is set, so `go test ./...`
// works out of the box without a database; CI and local verification
// set the env var (see docker-compose.yml's "test" profile and
// .github/workflows/ci.yml) to actually run them.
package dbstore

import (
	"context"
	"os"
	"testing"
	"time"
)

func testDB(t *testing.T) *DB {
	t.Helper()
	dsn := os.Getenv("TEAHUB_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEAHUB_TEST_DATABASE_URL not set; skipping dbstore integration tests (see README.md 'Running the DB integration tests')")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	db, err := Open(ctx, dsn, 5, 2, 5*time.Minute)
	if err != nil {
		t.Fatalf("connecting to test database: %v", err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrating test database: %v", err)
	}
	t.Cleanup(func() {
		// Truncate rather than drop so repeated test runs don't need
		// external setup beyond an empty database existing once.
		db.ExecContext(context.Background(), `TRUNCATE sessions, repositories, users CASCADE`)
		db.Close()
	})
	return db
}

func TestUpsertGitHubUser_CreateThenUpdate(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	u1, err := db.UpsertGitHubUser(ctx, 12345, "octocat", "The Octocat", "https://example.com/a.png", "octo@example.com")
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if u1.GitHubLogin != "octocat" {
		t.Errorf("expected login octocat, got %s", u1.GitHubLogin)
	}

	u2, err := db.UpsertGitHubUser(ctx, 12345, "octocat-renamed", "The Octocat", "https://example.com/b.png", "octo@example.com")
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if u2.ID != u1.ID {
		t.Errorf("expected same user id on re-login, got %s vs %s", u2.ID, u1.ID)
	}
	if u2.GitHubLogin != "octocat-renamed" {
		t.Errorf("expected updated login, got %s", u2.GitHubLogin)
	}
}

func TestRepository_OwnershipIsolation(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	alice, _ := db.UpsertGitHubUser(ctx, 1, "alice", "Alice", "", "")
	bob, _ := db.UpsertGitHubUser(ctx, 2, "bob", "Bob", "", "")

	repo, err := db.CreateRepository(ctx, alice.ID, "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("creating repository: %v", err)
	}

	if _, err := db.GetRepositoryForOwner(ctx, repo.ID, bob.ID); err != ErrNotFound {
		t.Errorf("expected ErrNotFound when bob looks up alice's repo, got %v", err)
	}
	if _, err := db.GetRepositoryForOwner(ctx, repo.ID, alice.ID); err != nil {
		t.Errorf("expected alice to be able to load her own repo, got %v", err)
	}

	if err := db.DeleteRepository(ctx, repo.ID, bob.ID); err != ErrNotFound {
		t.Errorf("expected ErrNotFound when bob tries to delete alice's repo, got %v", err)
	}
}

func TestCreateRepository_DuplicateURLForSameOwnerRejected(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	alice, _ := db.UpsertGitHubUser(ctx, 1, "alice", "Alice", "", "")
	if _, err := db.CreateRepository(ctx, alice.ID, "https://github.com/example/repo"); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := db.CreateRepository(ctx, alice.ID, "https://github.com/example/repo"); err != ErrAlreadyExists {
		t.Errorf("expected ErrAlreadyExists on duplicate import, got %v", err)
	}
}

func TestMigrate_IsIdempotent(t *testing.T) {
	db := testDB(t)
	if err := db.Migrate(context.Background()); err != nil {
		t.Errorf("second Migrate call should be a no-op, got error: %v", err)
	}
}

func TestSessionRevocation(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	alice, _ := db.UpsertGitHubUser(ctx, 1, "alice", "Alice", "", "")
	if err := db.CreateSession(ctx, alice.ID, "jti-1", "test-agent", "127.0.0.1", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("creating session: %v", err)
	}

	revoked, err := db.IsSessionRevoked(ctx, "jti-1")
	if err != nil || revoked {
		t.Fatalf("expected fresh session to be valid, revoked=%v err=%v", revoked, err)
	}

	if err := db.RevokeSession(ctx, "jti-1"); err != nil {
		t.Fatalf("revoking session: %v", err)
	}
	revoked, err = db.IsSessionRevoked(ctx, "jti-1")
	if err != nil || !revoked {
		t.Fatalf("expected revoked session to report revoked=true, got revoked=%v err=%v", revoked, err)
	}
}
