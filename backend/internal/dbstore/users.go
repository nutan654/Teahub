package dbstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound is returned by any lookup method when no row matches.
var ErrNotFound = errors.New("dbstore: not found")

// User mirrors the users table. It's the row shape, not an API/DTO type —
// api.publicUser (see api/auth_handlers.go) controls what actually gets
// serialized to clients.
type User struct {
	ID          string
	GitHubID    int64
	GitHubLogin string
	DisplayName string
	AvatarURL   string
	Email       string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	LastLoginAt sql.NullTime
}

// UpsertGitHubUser creates a user on first login or updates their cached
// profile fields (name/avatar/email can change on GitHub's side) on every
// subsequent login, and always stamps last_login_at. One round trip via
// ON CONFLICT rather than a separate SELECT-then-INSERT/UPDATE.
func (db *DB) UpsertGitHubUser(ctx context.Context, githubID int64, login, displayName, avatarURL, email string) (*User, error) {
	const q = `
		INSERT INTO users (github_id, github_login, display_name, avatar_url, email, last_login_at)
		VALUES ($1, $2, $3, $4, $5, now())
		ON CONFLICT (github_id) DO UPDATE SET
			github_login = EXCLUDED.github_login,
			display_name = EXCLUDED.display_name,
			avatar_url   = EXCLUDED.avatar_url,
			email        = EXCLUDED.email,
			last_login_at = now(),
			updated_at   = now()
		RETURNING id, github_id, github_login, display_name, avatar_url, email, created_at, updated_at, last_login_at`

	u := &User{}
	err := db.QueryRowContext(ctx, q, githubID, login, displayName, avatarURL, email).Scan(
		&u.ID, &u.GitHubID, &u.GitHubLogin, &u.DisplayName, &u.AvatarURL, &u.Email,
		&u.CreatedAt, &u.UpdatedAt, &u.LastLoginAt,
	)
	if err != nil {
		return nil, fmt.Errorf("upserting github user: %w", err)
	}
	return u, nil
}

// GetUserByID looks up a user by their internal UUID (used to hydrate the
// authenticated user on every request from the JWT's subject claim).
func (db *DB) GetUserByID(ctx context.Context, id string) (*User, error) {
	const q = `SELECT id, github_id, github_login, display_name, avatar_url, email, created_at, updated_at, last_login_at
		FROM users WHERE id = $1`
	u := &User{}
	err := db.QueryRowContext(ctx, q, id).Scan(
		&u.ID, &u.GitHubID, &u.GitHubLogin, &u.DisplayName, &u.AvatarURL, &u.Email,
		&u.CreatedAt, &u.UpdatedAt, &u.LastLoginAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("getting user by id: %w", err)
	}
	return u, nil
}

// CreateSession records an issued JWT so it can later be looked up for
// revocation (logout / "sign out everywhere"). Ordinary request auth never
// queries this table — only the logout and revocation-check paths do —
// keeping the hot path a pure signature/expiry check.
func (db *DB) CreateSession(ctx context.Context, userID, jwtID, userAgent, ipAddress string, expiresAt time.Time) error {
	const q = `INSERT INTO sessions (user_id, jwt_id, user_agent, ip_address, expires_at)
		VALUES ($1, $2, $3, $4, $5)`
	_, err := db.ExecContext(ctx, q, userID, jwtID, userAgent, ipAddress, expiresAt)
	if err != nil {
		return fmt.Errorf("creating session: %w", err)
	}
	return nil
}

// IsSessionRevoked reports whether the given JWT ID was explicitly revoked
// (logout) or its DB record is gone/expired. Used by RequireAuth as a
// defense-in-depth check in addition to the JWT's own expiry claim.
func (db *DB) IsSessionRevoked(ctx context.Context, jwtID string) (bool, error) {
	const q = `SELECT revoked_at IS NOT NULL OR expires_at < now() FROM sessions WHERE jwt_id = $1`
	var revoked bool
	err := db.QueryRowContext(ctx, q, jwtID).Scan(&revoked)
	if errors.Is(err, sql.ErrNoRows) {
		// No session row (e.g. pre-session-table token) — treat as valid;
		// the JWT signature/expiry check is still authoritative.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking session revocation: %w", err)
	}
	return revoked, nil
}

// RevokeSession marks a session's JWT ID revoked (logout).
func (db *DB) RevokeSession(ctx context.Context, jwtID string) error {
	const q = `UPDATE sessions SET revoked_at = now() WHERE jwt_id = $1 AND revoked_at IS NULL`
	_, err := db.ExecContext(ctx, q, jwtID)
	if err != nil {
		return fmt.Errorf("revoking session: %w", err)
	}
	return nil
}

// RevokeAllSessionsForUser implements "sign out everywhere".
func (db *DB) RevokeAllSessionsForUser(ctx context.Context, userID string) error {
	const q = `UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`
	_, err := db.ExecContext(ctx, q, userID)
	if err != nil {
		return fmt.Errorf("revoking sessions for user: %w", err)
	}
	return nil
}
