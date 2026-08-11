package dbstore

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Repository statuses. "ready" is the only state from which file content
// operations (browse, ask, search, diagram) are served.
const (
	StatusPending  = "pending"
	StatusCloning  = "cloning"
	StatusIndexing = "indexing"
	StatusReady    = "ready"
	StatusFailed   = "failed"
)

// Repository mirrors the repositories table.
type Repository struct {
	ID           string
	OwnerID      string
	URL          string
	LocalPath    string
	Status       string
	ErrorMessage string
	FileCount    int
	TreeJSON     []byte // raw JSON tree, decoded by the api layer into *store.FileNode
	ImportedAt   time.Time
	UpdatedAt    time.Time
}

// CreateRepository inserts a new repository row in "pending" status,
// scoped to ownerID. Returns ErrAlreadyExists if this owner already
// imported this exact URL (the unique (owner_id, url) constraint).
var ErrAlreadyExists = errors.New("dbstore: repository already exists for this owner")

func (db *DB) CreateRepository(ctx context.Context, ownerID, url string) (*Repository, error) {
	const q = `INSERT INTO repositories (owner_id, url, status)
		VALUES ($1, $2, $3)
		RETURNING id, owner_id, url, local_path, status, error_message, file_count, tree_json, imported_at, updated_at`
	r := &Repository{}
	err := db.QueryRowContext(ctx, q, ownerID, url, StatusPending).Scan(
		&r.ID, &r.OwnerID, &r.URL, &r.LocalPath, &r.Status, &r.ErrorMessage,
		&r.FileCount, &r.TreeJSON, &r.ImportedAt, &r.UpdatedAt,
	)
	if isUniqueViolation(err) {
		return nil, ErrAlreadyExists
	}
	if err != nil {
		return nil, fmt.Errorf("creating repository: %w", err)
	}
	return r, nil
}

// UpdateRepositoryResult is called once import/indexing finishes (success
// or failure) to persist the outcome: local path, status, file count, and
// the file tree (small: paths + sizes, never content — see
// ARCHITECTURE.md "Storage model").
func (db *DB) UpdateRepositoryResult(ctx context.Context, id, localPath, status, errMsg string, fileCount int, tree any) error {
	treeJSON, err := json.Marshal(tree)
	if err != nil {
		return fmt.Errorf("marshaling tree: %w", err)
	}
	const q = `UPDATE repositories
		SET local_path = $2, status = $3, error_message = $4, file_count = $5, tree_json = $6, updated_at = now()
		WHERE id = $1`
	_, err = db.ExecContext(ctx, q, id, localPath, status, errMsg, fileCount, treeJSON)
	if err != nil {
		return fmt.Errorf("updating repository result: %w", err)
	}
	return nil
}

// UpdateRepositoryStatus does a lightweight status-only update (e.g.
// pending -> cloning -> indexing) without touching the other fields.
func (db *DB) UpdateRepositoryStatus(ctx context.Context, id, status string) error {
	const q = `UPDATE repositories SET status = $2, updated_at = now() WHERE id = $1`
	_, err := db.ExecContext(ctx, q, id, status)
	if err != nil {
		return fmt.Errorf("updating repository status: %w", err)
	}
	return nil
}

// GetRepositoryForOwner enforces ownership at the query level: a repo
// belonging to another user simply doesn't match, so callers can't
// accidentally leak cross-tenant data by forgetting a check in Go code.
func (db *DB) GetRepositoryForOwner(ctx context.Context, id, ownerID string) (*Repository, error) {
	const q = `SELECT id, owner_id, url, local_path, status, error_message, file_count, tree_json, imported_at, updated_at
		FROM repositories WHERE id = $1 AND owner_id = $2`
	r := &Repository{}
	err := db.QueryRowContext(ctx, q, id, ownerID).Scan(
		&r.ID, &r.OwnerID, &r.URL, &r.LocalPath, &r.Status, &r.ErrorMessage,
		&r.FileCount, &r.TreeJSON, &r.ImportedAt, &r.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("getting repository: %w", err)
	}
	return r, nil
}

// ErrInvalidCursor is returned by ListRepositoriesForOwnerPage when the
// caller-supplied cursor doesn't decode to a valid (imported_at, id) pair
// — either it's malformed, or it was generated for a different owner /
// query shape. Handlers should treat this as a 400, not a 500.
var ErrInvalidCursor = errors.New("dbstore: invalid pagination cursor")

// ListRepositoriesForOwnerPage returns up to limit repositories owned by
// ownerID, most recently imported first (ties broken by id, descending,
// so the ordering is stable even for repositories imported in the same
// instant). Pass cursor="" for the first page; the returned nextCursor is
// "" once there are no more pages, and otherwise should be echoed back
// verbatim as the next call's cursor.
//
// This uses keyset ("seek") pagination rather than OFFSET: OFFSET
// pagination re-scans and discards all preceding rows on every page,
// which gets linearly slower the deeper a caller pages, and can skip or
// duplicate rows if a repository is imported/deleted between page
// fetches. Keyset pagination has neither problem and is the standard
// choice for anything indexed and append-mostly like this table.
func (db *DB) ListRepositoriesForOwnerPage(ctx context.Context, ownerID string, limit int, cursor string) (repos []*Repository, nextCursor string, err error) {
	args := []any{ownerID}
	q := `SELECT id, owner_id, url, local_path, status, error_message, file_count, tree_json, imported_at, updated_at
		FROM repositories WHERE owner_id = $1`

	if cursor != "" {
		importedAt, id, decErr := decodeRepoCursor(cursor)
		if decErr != nil {
			return nil, "", ErrInvalidCursor
		}
		args = append(args, importedAt, id)
		q += ` AND (imported_at, id) < ($2, $3)`
	}
	// Fetch one extra row beyond the page size so we know whether a next
	// page exists without a second round trip.
	args = append(args, limit+1)
	q += fmt.Sprintf(` ORDER BY imported_at DESC, id DESC LIMIT $%d`, len(args))

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", fmt.Errorf("listing repositories page: %w", err)
	}
	defer rows.Close()

	var out []*Repository
	for rows.Next() {
		r := &Repository{}
		if err := rows.Scan(&r.ID, &r.OwnerID, &r.URL, &r.LocalPath, &r.Status, &r.ErrorMessage,
			&r.FileCount, &r.TreeJSON, &r.ImportedAt, &r.UpdatedAt); err != nil {
			return nil, "", fmt.Errorf("scanning repository row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("iterating repository rows: %w", err)
	}

	if len(out) > limit {
		last := out[limit-1]
		nextCursor = encodeRepoCursor(last.ImportedAt, last.ID)
		out = out[:limit]
	}
	return out, nextCursor, nil
}

func encodeRepoCursor(importedAt time.Time, id string) string {
	raw := importedAt.UTC().Format(time.RFC3339Nano) + "|" + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeRepoCursor(cursor string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", err
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, "", fmt.Errorf("malformed cursor")
	}
	importedAt, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, "", err
	}
	return importedAt, parts[1], nil
}

// EncodeRepoCursorForTests and DecodeRepoCursorForTests expose the cursor
// codec to other packages (specifically memstore, so its pagination
// mirrors DB's cursor format exactly for handler tests) without making
// the encoding part of the public API surface real callers should depend
// on — the cursor is meant to be opaque to everyone but this package.
func EncodeRepoCursorForTests(importedAt time.Time, id string) string {
	return encodeRepoCursor(importedAt, id)
}
func DecodeRepoCursorForTests(cursor string) (time.Time, string, error) {
	return decodeRepoCursor(cursor)
}

// ListAllReadyRepositories is used once at startup to rehydrate the
// in-memory runtime cache (Files/Index — see store.RuntimeCache) for
// repos that were "ready" before the last restart, so a restart doesn't
// silently orphan repos whose on-disk clone is still present.
func (db *DB) ListAllReadyRepositories(ctx context.Context) ([]*Repository, error) {
	const q = `SELECT id, owner_id, url, local_path, status, error_message, file_count, tree_json, imported_at, updated_at
		FROM repositories WHERE status = $1`
	rows, err := db.QueryContext(ctx, q, StatusReady)
	if err != nil {
		return nil, fmt.Errorf("listing ready repositories: %w", err)
	}
	defer rows.Close()

	var out []*Repository
	for rows.Next() {
		r := &Repository{}
		if err := rows.Scan(&r.ID, &r.OwnerID, &r.URL, &r.LocalPath, &r.Status, &r.ErrorMessage,
			&r.FileCount, &r.TreeJSON, &r.ImportedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scanning repository row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteRepository removes a repository row, scoped to ownerID. The
// caller is responsible for removing the on-disk clone (see
// gitclone.Remove) — that's a filesystem concern, not a DB concern.
func (db *DB) DeleteRepository(ctx context.Context, id, ownerID string) error {
	const q = `DELETE FROM repositories WHERE id = $1 AND owner_id = $2`
	res, err := db.ExecContext(ctx, q, id, ownerID)
	if err != nil {
		return fmt.Errorf("deleting repository: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking delete result: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// isUniqueViolation checks for Postgres error code 23505 (unique_violation).
// String-matching the driver error message is a deliberate compromise to
// avoid importing lib/pq's concrete error type into every call site; if
// TeaHub later swaps drivers this is the one place to update.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "23505") || strings.Contains(msg, "duplicate key value violates unique constraint")
}
