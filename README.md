# TeaHub

TeaHub is a developer tool for understanding unfamiliar GitHub repositories. Import a repository and TeaHub clones it, parses it, runs static analysis, and lets you browse files, search by intent, ask questions in natural language, and generate an architecture diagram. AI answers are built from the repository's real files and cite the files they used.


- **Backend (Render):** https://teahub-tqmf.onrender.com

> The deployment runs on free tiers. The first request after a period of inactivity can be slow because the instance wakes up, and repositories are re-cloned on demand after a restart (see [Storage model](#storage-model)).

## Features

- GitHub OAuth sign-in with JWT sessions in HttpOnly cookies and server-side session revocation
- Import a repository by GitHub URL or `owner/repo` shorthand (shallow clone, GitHub only)
- Static analysis of Go (real AST), Python (via `python3`'s `ast`), and JavaScript/TypeScript (regex heuristics, marked partial)
- Hybrid semantic search: keyword pre-filter, then Gemini re-ranking, with every returned path validated against real files
- Repository Q&A using keyword retrieval and Gemini, with source file citations
- Architecture diagram generation (Mermaid) with caching and a deterministic fallback diagram
- File explorer with syntax highlighting and a sandboxed file reader
- Per-user repository isolation enforced in SQL
- Per-user rate limiting on cost-bearing endpoints
- Optional unauthenticated Demo Mode backed by one pre-indexed repository
- Health, readiness, and in-process metrics endpoints
- Docker image (non-root), Docker Compose stack, GitHub Actions CI

## Architecture

```text
Browser (frontend/index.html, vanilla JS, Highlight.js + Mermaid via CDN)
        |  fetch()
        |  On Vercel, /api/* and /auth/* are rewritten (proxied) to the Render backend,
        |  so the browser sees a single origin and session cookies work without CORS.
        v
Go HTTP server (net/http, Go 1.22 ServeMux patterns)
   middleware: metrics > request id > panic recovery > access log > timeout > CORS
   per route:  auth > rate limit > handler
        |                  |                      |
        v                  v                      v
   PostgreSQL        Disk (clones)       In-memory runtime cache
   users             REPO_STORAGE_DIR    file contents, analysis index,
   repositories                          cached diagram
   sessions                                      |
                                                 v
                                          Gemini API (Q&A, search re-rank, diagram)
```

### Import pipeline

```text
POST /api/import
  -> validate URL (github.com only)
  -> INSERT repository row (status: pending)
  -> git clone --depth 1            (status: cloning)
  -> parser.Walk: tree + file text  (status: indexing)
  -> index.Build: concurrent static analysis (worker pool)
  -> persist metadata to Postgres   (status: ready)
  -> store files + index in the in-memory cache
```

Status values: `pending`, `cloning`, `indexing`, `ready`, `failed`.

### Storage model

| State | Location | Reason |
|---|---|---|
| Users, repository metadata (owner, URL, status, file tree), sessions | PostgreSQL | Small, structured, must survive restarts |
| Cloned working trees | Disk under `REPO_STORAGE_DIR` | Source blobs do not belong in a relational database |
| Parsed file contents, analysis index, cached diagram | In-process cache | Rebuildable from the clone |

If a repository's metadata exists in Postgres but its cache entry or clone is missing (restart, new instance, ephemeral disk), the server re-clones from the stored URL, re-parses, and rebuilds the index before serving the request. On startup it also tries to warm the cache for repositories that are already `ready`.

### Incremental indexing

`index.Build` analyzes every file once, using a bounded concurrent worker pool with context cancellation. `index.Update` re-analyzes only files whose content hash changed. A benchmark on a 1,000-file synthetic repository measured roughly 55 ms for a full build versus 2.7 ms for a no-change update (about 20x), measured on a single-core machine, so the gain comes from skipping unchanged files rather than from parallelism.

**Current status:** `/api/import` calls `Build` only. `Update` is implemented, tested (including a call-count test proving unchanged files are skipped), and benchmarked, but nothing in the running application triggers it yet.

## API

All endpoints return JSON. Authenticated endpoints use the `teahub_session` cookie. Full contract: [`API.md`](API.md) and [`openapi.yaml`](openapi.yaml).

| Method | Path | Auth | Description |
|---|---|---|---|
| GET | `/api/health` | no | Liveness (never touches the database), includes build version |
| GET | `/api/ready` | no | Readiness (pings the database, 503 if unreachable) |
| GET | `/metrics` | no | In-process request, error, and latency snapshot |
| GET | `/auth/github/login` | no | Start GitHub OAuth |
| GET | `/auth/github/callback` | no | Finish OAuth, set session cookie |
| POST | `/auth/logout` | yes | Revoke session, clear cookie |
| GET | `/api/me` | yes | Current user |
| POST | `/api/import` | yes, rate limited | Clone, parse, and index a repository |
| GET | `/api/repos?limit=&cursor=` | yes | List your repositories (keyset pagination, default 20, max 100) |
| DELETE | `/api/repos/{id}` | yes | Delete a repository and its clone |
| POST | `/api/ask` | yes, rate limited | Ask a question about a repository |
| GET | `/api/repos/{id}/file?path=` | yes | Read one file (sandboxed, 512 KB cap) |
| POST | `/api/repos/{id}/search` | yes, rate limited | Semantic search |
| POST | `/api/repos/{id}/diagram?refresh=true` | yes | Architecture diagram (cached unless `refresh=true`) |

Demo Mode routes (only registered when `DEMO_REPO_URL` is set): `GET /api/demo`, `GET /api/demo/file`, `POST /api/demo/ask`, `POST /api/demo/search`, `POST /api/demo/diagram`. The `ask` and `search` routes are rate limited by client IP.

Common status codes: `400` invalid input, `401` not signed in, `404` unknown repository **or not owned by you**, `409` already imported or still importing, `429` rate limited, `502` upstream failure (git or Gemini), `503` not ready or timed out.

## Security

- **Auth:** GitHub OAuth (authorization-code flow) with a random `state` checked against both a short-lived cookie and server memory. Sessions are HS256 JWTs (`sub`, `jti`, `iat`, `exp`) signed with a key of at least 32 bytes in production, verified with a constant-time comparison. The cookie is `HttpOnly`, `SameSite=Lax`, and `Secure` in production.
- **Revocation:** each JWT's `jti` is stored in the `sessions` table. The auth middleware checks revocation and loads the user from the database on every authenticated request, so logout takes effect immediately at the cost of two queries per request.
- **Tenant isolation:** repository queries filter on `id` and `owner_id` in SQL. Other users' repositories return `404`, not `403`, so existence is not revealed.
- **SQL:** parameterized queries only.
- **Filesystem:** file reads are confined to the repository's clone directory. Absolute paths, `..` traversal, and symlink escapes are rejected.
- **Cloning:** only `https://github.com/owner/repo` URLs are accepted, and `git` is invoked with an argument list (no shell) and a 90-second timeout.
- **Request limits:** body size cap, question and query length caps, handler timeout (default 60s), and `ReadHeaderTimeout`.
- **CORS:** default deny. The `Origin` is echoed only if it exactly equals `FRONTEND_ORIGIN`.
- **AI output:** returned search paths are validated against the candidate set, and diagram output must parse as a Mermaid diagram. Mermaid renders with `securityLevel: 'strict'`.
- **Config:** in production the server refuses to start without `DATABASE_URL` and a sufficiently long `JWT_SIGNING_KEY`.
- **Container:** multi-stage build, non-root runtime user.

## Tech stack

| Layer | Technology |
|---|---|
| Backend | Go 1.22, `net/http` |
| Database | PostgreSQL 16 via `database/sql` + `lib/pq` (the only third-party Go dependency) |
| Auth | GitHub OAuth, hand-rolled HS256 JWT (stdlib `crypto/hmac`) |
| AI | Google Gemini (OpenAI-compatible chat completions endpoint) with retries and model fallback |
| Frontend | Single HTML file, vanilla JS, Highlight.js, Mermaid.js |
| Containers | Docker, Docker Compose |
| CI | GitHub Actions |
| Hosting | Render (backend + Postgres), Vercel (frontend). Cloud Run + Cloud SQL guide included |

## Getting started

### Requirements

- Go 1.22+
- Docker and Docker Compose (recommended)
- A Gemini API key (for Q&A, search, and diagrams)
- A GitHub OAuth app (for sign-in)
- `git` on the machine running the server (already included in the Docker image)
- `python3` on the server `PATH` if you want full Python analysis (see [Known limitations](#known-limitations))

### Run with Docker Compose

```bash
git clone https://github.com/nutan654/Teahub.git
cd Teahub
cp .env.example .env     # fill in values
docker compose up --build
```

The app is served at http://localhost:8080 (Postgres on port 5432).

### Run without a database (development only)

```bash
export TEAHUB_IN_MEMORY_STORE=true
export JWT_SIGNING_KEY="$(openssl rand -base64 48)"
export GEMINI_API_KEY=...
go run ./backend/cmd/server
```

Metadata is lost on restart in this mode. Never use it in production.

### Configuration

Required for the full product: `DATABASE_URL`, `JWT_SIGNING_KEY`, `GITHUB_CLIENT_ID`, `GITHUB_CLIENT_SECRET`, `GITHUB_OAUTH_REDIRECT_URL`, `GEMINI_API_KEY`.

Without `GEMINI_API_KEY`, import still works but ask, search, and diagram fail. Without GitHub credentials, sign-in is disabled. Without `JWT_SIGNING_KEY` outside production, a random key is generated per process and sessions do not survive a restart.

| Variable | Default | Purpose |
|---|---|---|
| `PORT` | `8080` | Listen port |
| `TEAHUB_ENV` | `development` | `production` enables strict config validation and secure cookies |
| `DATABASE_URL` | none | Postgres DSN |
| `TEAHUB_IN_MEMORY_STORE` | `false` | Dev-only in-memory metadata store |
| `DB_MAX_OPEN_CONNS` / `DB_MAX_IDLE_CONNS` / `DB_CONN_MAX_LIFETIME` | `10` / `5` / `30m` | Connection pool |
| `SESSION_TTL` | `168h` | Session lifetime |
| `SESSION_COOKIE_NAME` | `teahub_session` | Cookie name |
| `SECURE_COOKIES` | true in production | `Secure` cookie flag |
| `GEMINI_MODEL` | see `.env.example` | Primary model (fallbacks are tried automatically) |
| `REPO_STORAGE_DIR` | `./data/repos` | Where clones live |
| `FRONTEND_ORIGIN` | empty | Only needed if the frontend is on a different origin than the API |
| `REQUEST_TIMEOUT` | `60s` | Per-request handler timeout |
| `SHUTDOWN_GRACE_PERIOD` | `15s` | Graceful shutdown window |
| `MAX_REQUEST_BODY_BYTES` | `1048576` | Request body cap |
| `MAX_QUESTION_LENGTH` / `MAX_SEARCH_QUERY_LENGTH` | `4000` / `500` | Input caps |
| `IMPORT_RATE_LIMIT_PER_MIN` / `_BURST` | `5` / `3` | Import limiter |
| `ASK_RATE_LIMIT_PER_MIN` / `_BURST` | `20` / `10` | Ask and search limiter |
| `DEMO_REPO_URL` | empty | Enables Demo Mode (for example `golang/example`) |
| `DEMO_ASK_RATE_LIMIT_PER_MIN` / `_BURST` | `6` / `3` | Per-IP demo limiter |

`backend/internal/config/config.go` is the authoritative list.

### Demo Mode

Set `DEMO_REPO_URL` and the server clones and indexes that repository in the background after startup. Until it finishes, `/api/demo/*` returns `503`. Demo endpoints are read-only and need no sign-in. Leave the variable unset and no demo routes exist.

## Development

```bash
make build              # compile
make test               # unit tests
make race               # tests with the race detector
make bench              # indexing benchmarks
make vet                # go vet
make fmt                # list files that are not gofmt'd
make test-integration   # DB tests against a throwaway Postgres (needs Docker)
```

Unit tests use an in-memory fake that satisfies the same `DataStore` interface as the Postgres store, so they run without a database. `dbstore` integration tests run against real Postgres when `TEAHUB_TEST_DATABASE_URL` is set.

### CI

GitHub Actions runs on every push to `main` and every pull request: `gofmt`, `go vet`, `staticcheck`, build, `go test`, `go test -race`, `dbstore` integration tests against a Postgres 16 service container, and a Docker image build (not pushed).

### Database migrations

SQL files in `backend/internal/dbstore/migrations` are embedded in the binary, applied in filename order at startup, each in its own transaction, and recorded in `schema_migrations`. Migrations are forward-only.

## Deployment

- **Render:** `render.yaml` is a Blueprint that deploys the Dockerfile with a managed Postgres. Secrets (`GITHUB_*`, `GEMINI_API_KEY`) are entered in the dashboard. See [`deploy/render.md`](deploy/render.md).
- **Vercel:** serves `frontend/` and rewrites `/api/*` and `/auth/*` to the backend (configure the destination in `vercel.json`).
- **Cloud Run + Cloud SQL:** step-by-step guide in [`deploy/cloudrun.md`](deploy/cloudrun.md) (written but not executed).

The platform health check uses `/api/health`. `/api/ready` additionally checks the database.

## Project structure

```text
backend/
  cmd/server/            entrypoint, startup, graceful shutdown
  internal/
    api/                 routes, handlers, demo mode, cache warm-up
    auth/                GitHub OAuth, JWT
    config/              environment configuration and validation
    httpmw/              auth, logging, recovery, timeout, rate limiting, client IP
    dbstore/             Postgres access and migrations
    memstore/            in-memory store for tests and dev
    store/               runtime cache (files, index, diagram)
    gitclone/            URL validation and shallow clone
    parser/              filesystem walk, tree and text extraction
    fsread/              sandboxed file reads
    analysis/            Go, Python, JavaScript static analysis
    index/               incremental index and worker pool
    ai/                  Gemini client, keyword retrieval, context building
    semsearch/           hybrid semantic search
    diagram/             Mermaid diagram generation and fallback
    metrics/             in-process metrics
frontend/index.html      single-page UI
deploy/                  Render and Cloud Run guides
Dockerfile, docker-compose.yml, render.yaml, vercel.json, Makefile
API.md, openapi.yaml, ARCHITECTURE.md, ROADMAP.md
```

## Known limitations

- **Import is synchronous.** Clone, parse, and index happen inside one request. The clone timeout is 90 seconds but the default request timeout is 60 seconds, so very large repositories can time out. Asynchronous jobs are planned.
- **A failed import leaves its row in `failed` state,** and importing the same URL again returns `409` until the repository is deleted.
- **Retrieval is keyword-based.** Q&A and the search candidate pool start from keyword overlap. Semantic search re-ranks by meaning, but a relevant file sharing no words with the query can still be missed in large repositories. Embeddings are the planned replacement.
- **JavaScript/TypeScript analysis is heuristic** and always marked partial.
- **Python analysis needs `python3` on `PATH`.** The provided Docker image does not install it, so Python files are marked partial there unless you add it to the image.
- **Single-instance assumptions.** The rate limiters, OAuth state store, and runtime cache are in process memory. Running several instances multiplies the effective rate limits and can break a login whose callback lands on a different instance. Redis or Postgres would be needed for a shared implementation.
- **Demo rate limiting uses `X-Forwarded-For`,** which clients can spoof. It protects the AI budget, not a security boundary.
- **`/metrics` is unauthenticated** and resets on restart.
- **Ephemeral disk on free hosting.** Clones are lost on restart and re-created on first access.
- **Only public GitHub repositories** can be imported.

## License

See [`LICENSE`](LICENSE).

## Author

Nutan Bisandre, Electronics & Communication Engineering, IIITDM Jabalpur. [GitHub](https://github.com/nutan654)
