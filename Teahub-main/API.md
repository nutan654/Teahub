# TeaHub API Reference

A machine-readable version of this document is in `openapi.yaml` at the
repo root (OpenAPI 3.0); it also documents the `429` rate-limit response
on `/api/import`, `/api/ask`, and `/api/repos/{id}/search`, which isn't
called out below.

Base URL: same origin as the frontend by default (e.g. `http://localhost:8080`).
All request/response bodies are JSON unless noted. All endpoints except
`/api/health`, `/api/ready`, `/metrics`, `/auth/github/login`, and
`/auth/github/callback` require a valid session cookie (see
"Authentication" below) — omit it and you get `401 {"error": "..."}`.

## Authentication

### `GET /auth/github/login`
Redirects the browser to GitHub's OAuth authorize page. No body/params.
Sets a short-lived CSRF-state cookie.

### `GET /auth/github/callback?code=...&state=...`
GitHub redirects here after the user approves. On success, sets the
session cookie and redirects to `FRONTEND_ORIGIN` (or `/`). On failure,
returns `400`/`502` with a JSON error body.

### `POST /auth/logout`
Requires auth. Revokes the current session server-side and clears the
cookie.

**Response** `200`: `{"status": "signed out"}`

### `GET /api/me`
Requires auth. Returns the current user's identity.

**Response** `200`:
```json
{"id": "uuid", "githubLogin": "octocat"}
```

## Health

### `GET /api/health`
No auth required. Liveness probe — never touches the database.

**Response** `200`: `{"status": "ok", "version": "..."}`

### `GET /api/ready`
No auth required. Readiness probe — pings the database.

**Response** `200`: `{"status": "ready"}`
**Response** `503`: `{"status": "not ready", "error": "database unreachable"}`

### `GET /metrics`
No auth required (exposes only aggregate counts/latency, no per-user or
per-repo data). Basic request/error metrics collected in-memory since
process start.

**Response** `200`:
```json
{
  "totalRequests": 1204,
  "errors5xx": 3,
  "requestsByStatusClass": {"2xx": 1180, "4xx": 21, "5xx": 3},
  "latencyP50Ms": 42.1,
  "latencyP99Ms": 890.4,
  "latencySampleCount": 1204
}
```
Latency percentiles are computed over the most recent 2000 requests
(a bounded in-memory window, not the full process lifetime).

## Repositories

### `POST /api/import`
Requires auth. Clones, parses, and indexes a public GitHub repository,
scoped to the calling user. Synchronous — the request blocks until
cloning + indexing finish (see ARCHITECTURE.md "Synchronous import").

**Request**:
```json
{"url": "owner/repo"}
```
(also accepts a full `https://github.com/owner/repo` URL)

**Response** `200`: the created repository object (id, url, status, tree, fileCount, importedAt).
**Response** `409`: you already imported this exact URL.
**Response** `400`/`502`: invalid URL / clone failed.

### `GET /api/repos?limit=20&cursor=...`
Requires auth. Lists repositories owned by the calling user only, most
recently imported first, keyset-paginated.

**Query params**: `limit` (optional, default 20, max 100). `cursor`
(optional, opaque — omit for the first page; pass back the previous
response's `nextCursor` to get the next page).

**Response** `200`: `{"repos": [...], "nextCursor": "..."}` — `repos` is
an array of repository objects; `nextCursor` is `""` once there are no
more pages.
**Response** `400`: invalid `limit` or `cursor`.

### `DELETE /api/repos/{id}`
Requires auth + ownership. Deletes the repository's metadata and its
on-disk clone.

**Response** `200`: `{"status": "deleted"}`
**Response** `404`: unknown repo, or it isn't yours.

### `GET /api/repos/{id}/file?path=relative/path`
Requires auth + ownership + the repo must be `ready`. Returns one file's
content, syntax-language-detected. Path traversal is rejected.

**Response** `200`: `{"path": "...", "language": "go", "content": "...", "truncated": false}`
**Response** `400`: missing `path`, path escapes the repo, or path is a directory.
**Response** `404`: unknown repo/file.
**Response** `409`: repo exists but is still importing.

### `POST /api/ask`
Requires auth + ownership. Grounded Q&A over an imported repo.

**Request**:
```json
{"repoId": "uuid", "question": "where is auth handled?"}
```

**Response** `200`: `{"answer": "...", "sourceFiles": ["path/a.go", "path/b.go"]}`
**Response** `502`: upstream AI call failed (e.g. `GEMINI_API_KEY` unset).

### `POST /api/repos/{id}/search`
Requires auth + ownership. Hybrid semantic code search.

**Request**: `{"query": "where do we validate incoming webhooks"}`

**Response** `200`: `{"results": [{"path": "...", "reason": "..."}]}`
**Response** `400`: empty query.

### `POST /api/repos/{id}/diagram?refresh=true`
Requires auth + ownership. Generates (or returns a cached) architecture
diagram. Omit `?refresh=true` to get the cached version if one exists.

**Response** `200`: `{"mermaid": "...", "explanation": "...", "cached": true}`

## Error format

Every non-2xx response is `{"error": "human-readable message"}`. Ownership
failures and not-found failures are both `404` (never `403`) so a caller
can't distinguish "doesn't exist" from "isn't yours" — see
ARCHITECTURE.md "Ownership enforcement".
