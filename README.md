# TeaHub

A developer intelligence platform for understanding unfamiliar GitHub repositories through static analysis, semantic search, AI-assisted code exploration, and automatically generated architecture diagrams.

TeaHub turns a repository into an explorable knowledge system: import a GitHub repository, analyze its structure and source code, browse files, search by intent, ask questions in natural language, and generate an architecture overview with source-grounded answers.


- ⚙️ **Render:** https://teahub-tqmf.onrender.com

## Core Capabilities

* GitHub OAuth authentication with server-side session revocation
* Repository import through GitHub URLs or `owner/repository` shorthand
* Automatic repository cloning, parsing, and indexing
* Per-language static analysis for Go, Python, and JavaScript
* Incremental indexing using content hashes to skip unchanged files
* Semantic code search using keyword pre-filtering and LLM re-ranking
* AI-assisted repository Q&A powered by Google Gemini
* Source-grounded answers with file-level citations
* Automatic Mermaid architecture diagram generation
* Syntax-highlighted source exploration
* Per-user repository isolation enforced at the SQL layer
* Path-traversal-safe file access
* Request rate limiting for expensive AI and repository operations
* Dockerized, non-root production deployment
* Automated CI with tests, race detection, static analysis, and Docker builds

## Problem

Understanding an unfamiliar codebase often requires manually cloning the repository, navigating its directory tree, locating relevant files, and building a mental model of its architecture.

TeaHub compresses that workflow into a single interface.

Instead of asking:

> "Where is authentication implemented?"

a developer can ask the repository directly and receive an answer grounded in the actual source files.

## Architecture

```text
                         ┌──────────────────────────┐
                         │      Browser / UI         │
                         │   Static HTML + JS        │
                         │  Highlight.js + Mermaid   │
                         └────────────┬─────────────┘
                                      │
                              HTTP / Session Cookie
                                      │
                                      ▼
                         ┌──────────────────────────┐
                         │       Go HTTP API        │
                         │                          │
                         │ Routing · Auth · API     │
                         │ Ownership · Rate Limits  │
                         └───────┬─────────┬────────┘
                                 │         │
                    ┌────────────┘         └─────────────┐
                    ▼                                    ▼
          ┌───────────────────┐                ┌───────────────────┐
          │    PostgreSQL     │                │ Repository Engine │
          │                   │                │                   │
          │ Users             │                │ Git Clone         │
          │ Repositories      │                │ Parser            │
          │ Sessions          │                │ Static Analysis   │
          │ Metadata          │                │ Incremental Index │
          └───────────────────┘                └─────────┬─────────┘
                                                         │
                                                         ▼
                                               ┌───────────────────┐
                                               │ AI / Search Layer │
                                               │                   │
                                               │ Gemini            │
                                               │ Semantic Search   │
                                               │ Architecture      │
                                               └───────────────────┘
```

PostgreSQL stores application metadata such as users, repositories, and sessions. Repository working trees remain on disk, while parsed source content and analysis indexes are maintained as runtime caches and rebuilt when necessary.

## Repository Intelligence Pipeline

```text
GitHub Repository
       │
       ▼
   Git Clone
       │
       ▼
Filesystem Parser
       │
       ├───────────────┐
       ▼               ▼
  File Tree       Source Contents
                       │
                       ▼
               Static Analysis
                       │
                       ▼
              Incremental Index
                       │
             ┌─────────┴─────────┐
             ▼                   ▼
        Code Search          AI Retrieval
             │                   │
             └─────────┬─────────┘
                       ▼
                 Gemini API
                       │
              ┌────────┼─────────┐
              ▼        ▼         ▼
             Q&A    Search    Architecture
```

## Incremental Indexing

TeaHub does not blindly re-analyze every file on every update.

Each indexed file is associated with its content hash. During an incremental update, files whose content has not changed are skipped entirely.

This reduces unnecessary analysis work and makes repeated repository updates significantly cheaper.

A benchmark on a 1,000-file synthetic repository reported approximately:

```text
Full Build       ~55 ms/op
No-change Update  ~2.7 ms/op
Improvement       ~20x
```

The indexing engine also uses a concurrent worker pool with cancellation support and is covered by race testing.

## AI-Assisted Code Understanding

TeaHub uses Google Gemini for three major capabilities:

### Repository Q&A

Users can ask questions about the imported codebase in natural language.

Answers are grounded in repository files rather than relying solely on model knowledge.

### Semantic Search

Search combines:

```text
Keyword pre-filter
        +
LLM re-ranking
        +
Path validation
```

This avoids requiring a separate vector database while still supporting intent-based code discovery.

Returned paths are validated against the actual repository file set before being shown to the user.

### Architecture Generation

TeaHub analyzes the repository structure and generates:

* Mermaid architecture diagrams
* Structural summaries
* Written explanations of major components and relationships

## Security

Security is treated as part of the application architecture rather than an afterthought.

### Authentication

* GitHub OAuth
* HS256 JWT authentication
* HttpOnly cookies
* SameSite=Lax
* Secure cookies in production
* Server-side session revocation
* OAuth CSRF protection using a state token

### Multi-Tenant Isolation

Repository ownership is enforced directly in SQL.

Repository queries are scoped by both repository ID and owner ID:

```text
WHERE id = ? AND owner_id = ?
```

This prevents accidental cross-user access even if a handler-level authorization check is missed.

Unauthorized repositories return `404`, preventing information leakage about resources belonging to another user.

### File-System Security

File reads are sandboxed against the repository's actual clone root.

The implementation rejects:

* `../` traversal
* Absolute paths
* Symlink escapes

### API Security

* Parameterized SQL queries
* Default-deny CORS
* Per-user rate limiting
* Stricter IP-based limits for anonymous Demo Mode
* No hardcoded secrets
* Production configuration validation
* Non-root Docker runtime

## Demo Mode

TeaHub includes an optional unauthenticated Demo Mode for evaluating the product without configuring GitHub OAuth or PostgreSQL.

Set:

```bash
DEMO_REPO_URL=golang/example
```

and TeaHub clones and indexes the repository once at startup.

The demo exposes read-only repository exploration, search, Q&A, and architecture generation while applying tighter rate limits to protect AI usage.

The architecture diagram is generated once and cached for subsequent visitors.

Demo Mode is completely opt-in and disappears when `DEMO_REPO_URL` is not configured.

## Technology Stack

| Layer                | Technology                             |
| -------------------- | --------------------------------------- |
| Backend              | Go 1.22                                |
| HTTP                 | Go `net/http` / `ServeMux`             |
| Database             | PostgreSQL 16                          |
| Database Access      | `database/sql` + `lib/pq`              |
| Authentication       | GitHub OAuth + HS256 JWT               |
| AI                   | Google Gemini API                      |
| Frontend             | HTML, CSS, JavaScript                  |
| Code Highlighting    | Highlight.js                           |
| Diagrams             | Mermaid.js                             |
| Containerization     | Docker                                 |
| Local Infrastructure | Docker Compose                         |
| CI                   | GitHub Actions                         |
| Production           | Render / Cloud Run                     |
| Database Hosting     | PostgreSQL / Cloud SQL                 |
| Secrets              | Environment variables / Secret Manager |

The backend deliberately keeps dependencies minimal: `lib/pq` is the only third-party Go dependency; routing, authentication, JWT handling, and OAuth are implemented using the Go standard library.

## Testing & Quality

The project includes multiple levels of validation:

```bash
make test
make test-integration
make race
make bench
make vet
make fmt
```

CI automatically runs:

```text
gofmt
go vet
go build
go test
go test -race
Docker build
```

Integration tests use a real PostgreSQL service in CI, while the standard unit test suite can run without a database.

## Local Development

### Requirements

* Go 1.22+
* Docker
* PostgreSQL, or Docker Compose
* Google Gemini API key
* GitHub OAuth application for authenticated development

### Setup

```bash
git clone https://github.com/nutan654/Teahub.git
cd Teahub

go mod tidy
cp .env.example .env
```

Configure:

```text
DATABASE_URL
GEMINI_API_KEY
JWT_SIGNING_KEY
GITHUB_CLIENT_ID
GITHUB_CLIENT_SECRET
GITHUB_OAUTH_REDIRECT_URL
```

Start the complete stack:

```bash
docker compose up --build
```

The application runs on:

```text
http://localhost:8080
```

For development without PostgreSQL, TeaHub also supports an in-memory store through:

```text
TEAHUB_IN_MEMORY_STORE=true
```

This mode is intended only for development and testing, not production.

## Project Structure

```text
Teahub/
├── backend/
│   ├── cmd/server/
│   └── internal/
│       ├── api/              HTTP handlers and routing
│       ├── auth/             GitHub OAuth and JWT
│       ├── config/           Environment configuration
│       ├── httpmw/           Middleware and rate limiting
│       ├── dbstore/          PostgreSQL persistence
│       ├── memstore/         In-memory store
│       ├── gitclone/         Repository cloning and validation
│       ├── parser/            Filesystem parsing
│       ├── fsread/            Secure file access
│       ├── analysis/          Go/Python/JavaScript analysis
│       ├── index/             Incremental indexing
│       ├── ai/                Gemini integration
│       ├── diagram/           Architecture generation
│       ├── semsearch/         Semantic search
│       └── store/             Runtime cache
│
├── frontend/
│   └── index.html             Single-page frontend
│
├── deploy/
│   ├── render.md
│   └── cloudrun.md
│
├── .github/workflows/
│   └── ci.yml
│
├── Dockerfile
├── docker-compose.yml
├── render.yaml
├── API.md
├── openapi.yaml
├── ARCHITECTURE.md
├── ROADMAP.md
├── Makefile
└── go.mod
```

## API

Core API operations include:

```text
POST /api/import
POST /api/ask
POST /api/repos/{id}/search
POST /api/repos/{id}/diagram
```

The complete API contract is documented in:

```text
API.md
openapi.yaml
```

## Deployment

TeaHub is containerized and deployment-ready for:

```text
Render
Cloud Run
Cloud SQL
```

The repository includes:

* `Dockerfile`
* `docker-compose.yml`
* `render.yaml`
* Render deployment documentation
* Cloud Run deployment documentation
* PostgreSQL configuration
* CI/CD workflow

For production deployments, repository storage must account for ephemeral container filesystems. PostgreSQL stores persistent metadata while cloned repositories and runtime indexes are treated as rebuildable state.

## Engineering Decisions

### Why Go?

The backend uses Go's standard library extensively to keep the service small, explicit, and easy to reason about.

### Why PostgreSQL?

PostgreSQL provides durable storage for users, repository metadata, and sessions while allowing ownership constraints to be enforced directly through SQL.

### Why Hybrid Search?

A keyword pre-filter followed by LLM re-ranking provides semantic retrieval without introducing an embeddings database or vector infrastructure.

### Why Synchronous Repository Import?

The initial implementation keeps clone → parse → analyze → index within a single request. This makes the system easier to test and reason about. Asynchronous job processing is planned as a future scaling improvement.

## Future Work

Planned improvements include:

* Asynchronous repository imports
* Background indexing jobs
* Improved incremental analysis
* Expanded language support
* More advanced repository dependency analysis
* Production-scale caching
* Persistent external repository storage
* Additional AI-powered code intelligence features

## Author

**Nutan Bisandre**

Electronics & Communication Engineering
IIITDM Jabalpur

[GitHub](https://github.com/nutan654)
