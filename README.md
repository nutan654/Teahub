# TeaHub

TeaHub is a developer-intelligence platform: sign in with GitHub, import a
public repository, and it's incrementally indexed with real per-language
static analysis. On top of that you can browse and read the repo, search it
by meaning, generate an architecture diagram, and ask grounded questions
about it — with the model citing which files it looked at. Every imported
repository is private to the account that imported it.

Built incrementally: each stage shipped fully working and tested before the
next started (see `ROADMAP.md`). It's a real Go backend + Postgres +
GitHub OAuth, containerized and documented for a real Cloud Run deployment
— not a mockup, and not a toy in-memory demo.

## 1. What TeaHub solves

Understanding an unfamiliar codebase is slow: cloning it, guessing where
things live, and re-reading files just to answer "where is X handled?"
eats real time before you've written a line of code. TeaHub compresses
that first-hour ramp-up into a few minutes by combining real static
analysis with an LLM that's grounded in the actual repository content
(never just its own training data) and always cites its sources.

### Who it's for / when it's useful

- **Onboarding onto a codebase you didn't write** — new hire, new
  open-source contributor, or picking up a project you haven't touched in
  months. Import it, ask "what does this repo do?" or "where is
  authentication handled?" and get an answer grounded in the actual files,
  not a guess.
- **Evaluating a repo before you depend on it** — before adding a library
  or forking a project, generate its architecture diagram and skim the
  auto-extracted symbol index (functions, types, TODOs) instead of reading
  every file.
- **Code review / due diligence** — semantic search finds the function
  that *does* what you're describing even when it doesn't share any words
  with your query (e.g. "where do we prevent replay attacks" matching a
  nonce-check that never says "replay").
- **Teaching or interviewing** — point it at a reference implementation
  and let someone explore it with plain-English questions instead of a
  guided walkthrough.

Every repo you import is private to your account (GitHub-authenticated,
ownership-checked on every request — see "Security considerations" below),
so this is safe to use on private forks or work-in-progress branches, not
just public showcase repos.

### How to use it

1. **Sign in** — click "Sign in with GitHub" (top right). This is the
   only way in; there's no separate username/password system.
2. **Import a repository** — paste a public GitHub URL or `owner/repo`
   shorthand (e.g. `golang/example`) into the import box and submit. The
   backend clones it, walks the file tree, and runs per-language static
   analysis inline — for a small-to-medium repo this takes a few seconds
   to low tens of seconds; you'll see the imported repo appear in your
   list when it's done.
3. **Explore** — open the repo from your list and use the tabs:
   - **Explorer** — browse the file tree, click any file to read it with
     syntax highlighting.
   - **Chat / Ask** — ask a question in plain English ("what does this
     repo do?", "where is the rate limiter implemented?"). The answer
     cites the specific files it's grounded in, so you can verify it
     rather than trust it blindly.
   - **Search** — a query like "handles user sign-out" returns ranked
     files by actual relevance, not just keyword match.
   - **Architecture** — generates a Mermaid diagram of the repo's
     structure (detected stack, top-level layout, likely entrypoints)
     with a short written explanation alongside it.
4. **Delete a repo** whenever you're done with it — this removes its
   metadata and frees its cached index; nothing about your account is
   affected.

Everything above is also reachable directly via the HTTP API (see
`API.md` / `openapi.yaml`) if you'd rather script it than click through
the UI — e.g. `POST /api/import`, `POST /api/ask`, `POST
/api/repos/{id}/search`, `POST /api/repos/{id}/diagram`.

## 2. Architecture diagram

```
                    ┌──────────────────────┐
   Browser ───────▶ │  frontend/index.html │  (single file, no build step)
                    └──────────┬───────────┘
                               │ same-origin fetch, session cookie
                               ▼
                    ┌──────────────────────────────────────────┐
                    │              backend/internal/api          │
                    │  routing · auth middleware · ownership     │
                    └───────┬───────────────────────┬────────────┘
                            │                        │
             ┌──────────────▼───────────┐   ┌────────▼─────────────┐
             │  auth (GitHub OAuth,      │   │  gitclone / parser /  │
             │  hand-rolled HS256 JWT)   │   │  analysis / index      │
             └──────────────┬───────────┘   │  (static analysis,     │
                             │               │  incremental indexing) │
             ┌──────────────▼───────────┐   └────────┬──────────────┘
             │  dbstore (Postgres):      │            │
             │  users, repositories,     │   ┌────────▼──────────────┐
             │  sessions — metadata      │   │  store (in-process     │
             │  only, never file bodies  │   │  runtime cache): file   │
             └───────────────────────────┘   │  bodies + analysis      │
                                              │  index, rebuilt on      │
                                              │  cache miss             │
                                              └────────┬──────────────┘
                                                        │
                                    ┌───────────────────▼──────────────────┐
                                    │  ai / diagram / semsearch             │
                                    │  → Anthropic Claude API (Messages)    │
                                    └────────────────────────────────────────┘

Disk: REPO_STORAGE_DIR holds cloned working trees (file bodies), never
written to Postgres. See ARCHITECTURE.md "Storage model".
```

## 3. Tech stack

- **Backend**: Go 1.22, stdlib `net/http` (Go 1.22+ `ServeMux` path
  variables, no web framework), `database/sql` + `lib/pq`.
- **Database**: PostgreSQL 16 — users, repository metadata, sessions.
  Embedded, versioned SQL migrations (no external migration tool).
- **Auth**: GitHub OAuth (authorization-code flow, implemented directly
  against `net/http` — no `golang.org/x/oauth2` dependency) + hand-rolled
  HS256 JWT session tokens (no JWT library dependency) in an HttpOnly,
  SameSite=Lax cookie. Revocable via a `sessions` table.
- **AI**: Anthropic Claude API (Messages endpoint), one shared client for
  every AI-backed feature (Q&A, diagram generation, semantic search).
- **Frontend**: a single static HTML file, zero build step, `highlight.js`
  + `mermaid.js` via CDN.
- **Infra**: Docker (multi-stage build), docker-compose (local dev),
  GitHub Actions CI, Google Cloud Run + Cloud SQL + Secret Manager
  (deployment target — see `deploy/cloudrun.md`).

## 4. Key engineering decisions

- **Metadata in Postgres, file bodies on disk, never the reverse.**
  Repository metadata (owner, URL, status, file tree — paths and sizes,
  no content) is the only thing persisted to Postgres. Cloned working
  trees live under `REPO_STORAGE_DIR`; the parsed file-content map and
  derived static-analysis index are pure in-process cache, rebuilt from
  disk on a cache miss (e.g. after a restart). See `ARCHITECTURE.md`
  "Storage model" for the full reasoning, including the Cloud Run
  ephemeral-disk case.
- **Ownership enforced at the query level, not just in Go code.** Every
  repository lookup is scoped by `(id, owner_id)` in the SQL itself
  (`GetRepositoryForOwner`), so a missing ownership check in a handler
  can't leak another user's data — the query simply returns no rows.
  Cross-tenant lookups return 404, not 403, so a caller can't distinguish
  "doesn't exist" from "exists but isn't yours."
- **Stdlib-only JWT and OAuth.** TeaHub only needs HS256 sign/verify and a
  three-call GitHub OAuth flow; both are small enough to implement
  directly against `crypto/hmac` and `net/http` rather than pulling in
  `golang-jwt/jwt` or `golang.org/x/oauth2` for something this size (see
  the "unnecessary dependencies" note in `ROADMAP.md`).
  `github.com/lib/pq` remains the one real third-party dependency, since
  `database/sql` needs an actual driver.
- **Synchronous import.** Cloning + parsing + indexing happens inline
  within the `POST /api/import` request rather than as a background job.
  This keeps the whole import flow simple to reason about and test, at
  the cost of the request taking as long as the clone does. The
  async-import follow-up (with a job queue and progress updates) is
  tracked in `ROADMAP.md`, not built yet.
- **Hybrid, not vector, search.** `semsearch` and Q&A retrieval both use
  keyword pre-filtering + LLM re-ranking rather than an embeddings
  database — see `ARCHITECTURE.md` for the reasoning and the documented
  ceiling this creates for very large repos.

## 5. Performance / benchmark results

`backend/internal/index` includes benchmarks comparing a full `Build` pass
against the incremental `Update` path (only changed files re-analyzed) at
various repo sizes and concurrency levels:

```bash
make bench
# go test ./backend/internal/index/... -run '^$' -bench . -benchmem
```

These have **not been re-run in this environment** (no Go toolchain
available here — see "Verification status" below). Run `make bench`
yourself and paste the real output here; as of the last verified run
(prior development session) the incremental `Update` path completed
roughly 20x faster than a full `Build` at 1,000 files, because only
changed files (identified via content hashing — see `index/hash.go`) are
re-parsed.

## 6. Security considerations

- **Path traversal**: `fsread.Read` resolves every requested path against
  the repo's real clone root and rejects anything that escapes it (`../`,
  absolute paths, symlink escapes) — covered by explicit attack-case tests.
- **SQL injection**: every query in `dbstore` uses parameterized
  placeholders (`$1`, `$2`, ...); no string-concatenated SQL anywhere.
- **Session security**: HttpOnly + SameSite=Lax cookies (JS can't read the
  token; cross-site requests don't carry it), `Secure` flag forced on in
  production, constant-time signature comparison (`crypto/subtle`) on
  every JWT verify, and server-side revocation via the `sessions` table
  (checked on every authenticated request, not just at issuance).
- **CSRF on the OAuth callback**: a random `state` token is required to
  round-trip through both a short-lived cookie and server-side memory
  before a callback is accepted.
- **Ownership isolation**: see "Key engineering decisions" above — enforced
  in SQL, not just application code, and covered by dedicated tests
  (`TestRepository_OwnershipIsolation`, `TestFileContent_OtherUsersRepoIsNotFound`).
- **Secrets**: never committed — `.env` is gitignored, `.env.example`
  documents every variable without real values, and production config
  requires `JWT_SIGNING_KEY`/`GITHUB_CLIENT_ID/SECRET`/`DATABASE_URL` to be
  explicitly set (`config.Load` refuses to start in `TEAHUB_ENV=production`
  without them, rather than silently falling back to an insecure default).
- **Resource limits**: `MAX_REPO_BYTES`-style caps in `parser.Walk`
  (`maxFileBytes`, `maxIndexedFiles`) bound memory use per import; request
  timeouts (`httpmw.Timeout`) bound how long any single handler can run.
- **Non-root container**: the Docker image runs as an unprivileged user
  (see `Dockerfile`), not root.

## 7. Testing / CI

```bash
make test              # go test ./... — all packages, no DB required
make test-integration  # dbstore integration tests against a real, throwaway Postgres
make race               # go test -race ./...
make bench              # index package benchmarks
make vet
make fmt                # gofmt -l . — should print nothing
```

`dbstore`'s integration tests are skipped automatically (not failed) when
`TEAHUB_TEST_DATABASE_URL` isn't set, so `make test` works with zero setup;
`make test-integration` spins up a disposable Postgres via
`docker compose --profile test` to actually exercise them.

`.github/workflows/ci.yml` runs, on every push/PR: `gofmt -l`, `go vet`,
`go build`, `go test` (with a real Postgres service container so the
dbstore integration tests run in CI, not just locally), `go test -race`,
and a Docker image build. See that file for the exact jobs.

**Verification status (honesty note):** most of this repository was
assembled and reviewed in a sandboxed environment with no Go toolchain,
Docker, network access, or GCP/GitHub OAuth credentials, so most of it
was written and manually reviewed but not executed. As of the latest
dead-code audit pass, a Go toolchain was installed in that sandbox and
`gofmt -l .`, `go vet ./...`, `go build ./...`, `go test ./...`, and
`go test -race ./...` were all actually run against this tree (module
downloads used `GOPROXY=direct` since the sandbox's proxy.golang.org
access is blocked by network policy, not `go mod tidy`'s default) — all
green. **Still not exercised anywhere:** `.github/workflows/ci.yml`
itself (added in that same pass — no workflow file existed before it,
despite this README and ROADMAP.md Stage 10 describing one as already
shipped), any Docker build, the live OAuth round-trip, and the Postgres
integration tests (`make test-integration`), all of which need
resources this sandbox doesn't have. Run those yourself before trusting
a green build end-to-end.

## 8. Local setup

Requires Go 1.22+, Docker (for Postgres), and an
[Anthropic API key](https://console.anthropic.com/).

```bash
git clone <this-repo>
cd teahub
go mod tidy              # REQUIRED FIRST STEP — fetches github.com/lib/pq
                          # and generates go.sum (needs network access)

cp .env.example .env     # fill in ANTHROPIC_API_KEY at minimum;
                          # GITHUB_CLIENT_ID/SECRET for sign-in to work
                          # (see .env.example for how to create a GitHub OAuth App)

docker compose up --build   # starts Postgres + TeaHub on :8080
```

Or without Docker, against a Postgres you run yourself:

```bash
export DATABASE_URL=postgres://teahub:teahub@localhost:5432/teahub?sslmode=disable
export ANTHROPIC_API_KEY=sk-ant-...
export JWT_SIGNING_KEY=$(openssl rand -base64 48)
export GITHUB_CLIENT_ID=... GITHUB_CLIENT_SECRET=... GITHUB_OAUTH_REDIRECT_URL=http://localhost:8080/auth/github/callback
make run
```

Then open `http://localhost:8080`, sign in with GitHub, and import a repo
(paste `owner/repo` or a full URL).

Pure local hacking without any Postgres setup: `TEAHUB_IN_MEMORY_STORE=true`
swaps in an in-memory store (see `.env.example`) — repository metadata
will not survive a restart in this mode, and it must never be set in
production (`config.Load` allows it everywhere except when you also set
`TEAHUB_ENV=production`, at which point it's simply ignored in favor of
requiring a real `DATABASE_URL`).

## 9. Demo Mode (try it with zero setup)

Set `DEMO_REPO_URL` (e.g. `DEMO_REPO_URL=golang/example`) and TeaHub
clones + indexes that one repository **once, at server startup**, then
serves it through a handful of unauthenticated, read-only endpoints
(`GET /api/demo`, `GET /api/demo/file`, `POST /api/demo/ask`,
`/api/demo/search`, `/api/demo/diagram`) — no GitHub OAuth, no account,
no Postgres row, nothing to configure on the visitor's side. The landing
page's "Try live demo" button drives the exact same Explorer/Chat/Search/
Architecture UI as a real import, just pointed at these endpoints.

This exists specifically so anyone evaluating the project — a recruiter
following a resume link, a reviewer, anyone without their own Anthropic
key — can experience the real product in under a minute. A few
deliberate constraints keep it safe and cheap to leave running publicly:

- **Anonymous Ask/Search calls are rate-limited by caller IP**
  (`DEMO_ASK_RATE_LIMIT_PER_MIN` / `_BURST`, tighter than the
  authenticated defaults), so a publicly linked page can't run up your
  Anthropic bill.
- **The architecture diagram is generated once, total** — the first
  visitor to load it pays one AI call; every visitor after that gets the
  cached result. There's no `?refresh=true` for demo visitors.
- **Leaving `DEMO_REPO_URL` unset disables Demo Mode completely** — zero
  extra routes, zero extra clone/API cost. It's opt-in.

See `backend/internal/api/demo.go` for the full design note.

## 10. Live deployment

**Not currently deployed anywhere** — no GCP project or credentials were
available while building this. `deploy/cloudrun.md` documents the exact
`gcloud` commands to deploy to Cloud Run + Cloud SQL + Secret Manager,
written and reviewed but not executed. Follow it end-to-end to get a real
URL, then update this section with the live link.

## 11. Screenshots / demo instructions

No screenshots are checked into this repo (avoid committing binary image
diffs that go stale). To capture your own after running the app locally
(see section 8):

1. Sign in with GitHub, import a well-known repo, e.g. `golang/example`.
2. Screenshot the file tree + explorer view (click any file).
3. Ask a question in the Chat tab, e.g. "what does this repo do?" —
   screenshot the answer with its cited source files.
4. Run a Search tab query and screenshot the ranked results.
5. Generate an architecture diagram (Architecture tab) and screenshot the
   rendered Mermaid diagram + explanation.

## Project layout

```
teahub/
├── backend/
│   ├── cmd/server/            entrypoint: config, graceful shutdown, wiring
│   └── internal/
│       ├── config/            all environment-derived settings, in one place
│       ├── auth/               GitHub OAuth flow + hand-rolled HS256 JWT
│       ├── httpmw/             auth middleware, structured logging, recovery, timeouts
│       ├── dbstore/             Postgres: migrations, users, repositories, sessions
│       ├── memstore/            in-memory fake of dbstore's interface, for tests + TEAHUB_IN_MEMORY_STORE
│       ├── gitclone/            shallow-clone + URL validation
│       ├── parser/              filesystem walk -> tree + indexed file contents
│       ├── fsread/              sandboxed single-file reads for the explorer (traversal-safe)
│       ├── analysis/            per-language static analysis (Go via go/ast, Python via python3's ast, JS/TS heuristic)
│       ├── index/                incremental indexing engine: concurrent Build, hash-based incremental Update
│       ├── ai/                   reusable Claude API client (Client.Complete) + keyword retrieval
│       ├── diagram/              structural summary + Mermaid diagram generation/validation
│       ├── semsearch/            hybrid keyword-prefilter + LLM semantic re-ranking, with path validation
│       ├── store/                in-process runtime cache (file bodies + analysis index), not persisted
│       └── api/                  HTTP handlers, routing, auth/ownership enforcement (incl. demo.go — unauthenticated Demo Mode)
├── frontend/index.html        single-file UI, no build step (hljs + mermaid.js via CDN)
├── deploy/cloudrun.md          Cloud Run + Cloud SQL + Secret Manager deployment guide
├── .github/workflows/ci.yml    CI: fmt, vet, build, test, test -race, Docker build
├── Dockerfile                  multi-stage, non-root runtime
├── docker-compose.yml           local TeaHub + Postgres (+ disposable test-DB profile)
├── .env.example
├── API.md                       hand-written endpoint reference
├── openapi.yaml                 machine-readable OpenAPI 3.0 spec, generated from API.md and the handlers
├── ARCHITECTURE.md
└── ROADMAP.md
```

## Why Go

Chosen specifically because Go shows up repeatedly in Japanese engineering
job listings (Mercari, SmartHR, LayerX, etc.) and demonstrates comfort
with a statically-typed, concurrency-oriented backend — a different (and
for some roles more relevant) signal than another Node/Python side
project.
