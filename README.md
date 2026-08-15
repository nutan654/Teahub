# 🍵 TeaHub

**Understand an unfamiliar codebase in minutes, not hours.**

Sign in with GitHub, paste a repo URL, and TeaHub clones it, runs real
per-language static analysis, and lets you explore it — browse the file
tree, ask plain-English questions, search by meaning, and generate an
architecture diagram. Every answer cites the exact files it's grounded
in, so you can verify it instead of trusting it blindly.

Built incrementally: each stage shipped fully working and tested before the
next started (see `ROADMAP.md`). It's a real Go backend + Postgres +
GitHub OAuth, containerized and documented for both Render and Cloud Run
deployment — not a mockup, and not a toy in-memory demo.

**[▶ Try the live demo — no sign-in required](#try-it-now-zero-setup)**
&nbsp;·&nbsp; [API reference](API.md) &nbsp;·&nbsp; [Architecture deep-dive](ARCHITECTURE.md)

---

## Why this exists

Dropping into a codebase you didn't write is slow: clone it, guess where
things live, click through folders just to answer "where's auth
handled?" — before you've written a line of code. TeaHub compresses that
first hour into a few minutes by pairing real static analysis with an
LLM that only ever answers from the repository's actual contents, never
from its own training data, and always shows its work.

**Good for:**
- **Onboarding** onto a new codebase — new hire, new OSS contributor, or a project you haven't touched in months
- **Evaluating a library** before you depend on it — architecture diagram + symbol index instead of reading every file
- **Code review** — semantic search finds the function that *does* the thing you're describing, even if it doesn't share a single word with your query
- **Teaching / interviewing** — hand someone a reference repo and let them explore it by asking questions

Every imported repo is private to the account that imported it — safe for private forks and work-in-progress branches, not just public showcase repos.

---

## 🎮 Try it now (zero setup)

**Not deployed to a public URL yet** — but you don't need Docker,
Postgres, or a GitHub OAuth app to see it running. This is the actual
mode I used to verify every feature end-to-end:

```bash
git clone https://github.com/nutan654/Teahub.git && cd Teahub

TEAHUB_ENV=development \
TEAHUB_IN_MEMORY_STORE=true \
DEMO_REPO_URL=golang/example \
JWT_SIGNING_KEY=$(openssl rand -base64 48) \
go run ./backend/cmd/server
```

Open `http://localhost:8080` and click **"Try live demo."** TeaHub
clones `golang/example` once at startup and serves it read-only —
Explorer, Search, and Architecture work immediately. Ask/Chat also needs
`ANTHROPIC_API_KEY` set in the same command to get real answers instead
of a graceful "AI unavailable" response.

Swap `DEMO_REPO_URL` for any public `owner/repo` to demo a different
codebase. Once a permanent Cloud Run URL exists, it replaces this
section — the exact deploy steps are already written in
[`deploy/cloudrun.md`](deploy/cloudrun.md).

<details>
<summary>How Demo Mode actually works under the hood</summary>

<br>

Setting `DEMO_REPO_URL` clones + indexes that one repository **once, at
server startup**, and serves it through a handful of unauthenticated,
read-only endpoints (`GET /api/demo`, `/api/demo/file`, `POST
/api/demo/ask|search|diagram`) — no OAuth, no account, no Postgres row.
The landing page's "Try live demo" button drives the *exact same*
Explorer/Chat/Search/Architecture UI as a real signed-in import, just
pointed at these endpoints instead.

Guardrails, since this is meant to be safe to leave running publicly:
- **Anonymous Ask/Search calls are rate-limited by caller IP**
  (`DEMO_ASK_RATE_LIMIT_PER_MIN`/`_BURST`), tighter than the
  authenticated defaults, so a public link can't run up an AI bill.
- **The architecture diagram is generated once, total** — the first
  visitor pays the one AI call; everyone after gets the cached result.
  There's no `?refresh=true` for anonymous visitors.
- **Leaving `DEMO_REPO_URL` unset disables all of this** — zero extra
  routes, zero extra cost. It's fully opt-in.

Full implementation: [`backend/internal/api/demo.go`](backend/internal/api/demo.go).

</details>

---

## How to use it (the real, signed-in flow)

1. **Sign in with GitHub** — the only way in, no separate accounts.
2. **Import a repo** — paste a URL or `owner/repo` shorthand
   (`golang/example`). TeaHub clones it, walks the tree, and runs
   static analysis inline — seconds for a small-to-medium repo.
3. **Explore:**
   | Tab | What it does |
   |---|---|
   | **Explorer** | Browse the file tree, read any file with syntax highlighting |
   | **Chat / Ask** | Ask in plain English — answers cite the files they're grounded in |
   | **Search** | Finds files by what they *do*, not just keyword overlap |
   | **Architecture** | Generates a Mermaid diagram of the repo's structure with a written explanation |
4. **Delete** a repo any time — clears its metadata and cached index.

Everything above is also a plain HTTP API if you'd rather script it —
`POST /api/import`, `POST /api/ask`, `POST /api/repos/{id}/search`,
`POST /api/repos/{id}/diagram`. Full reference: [`API.md`](API.md) /
[`openapi.yaml`](openapi.yaml).

---

## Architecture

```text
                    ┌──────────────────────┐
   Browser ───────▶ │  frontend/index.html │  (single file, no build step)
                    └──────────┬───────────┘
                               │ same-origin fetch, session cookie
                               ▼
                    ┌────────────────────────────────────────┐
                    │           backend/internal/api          │
                    │  routing · auth middleware · ownership  │
                    └───────┬─────────────────────┬───────────┘
                            │                      │
             ┌──────────────▼───────────┐   ┌──────▼──────────────────┐
             │  auth: GitHub OAuth +    │   │  gitclone → parser →    │
             │  hand-rolled HS256 JWT   │   │  analysis → index       │
             └──────────────┬───────────┘   │  (static analysis,      │
                             │               │  incremental indexing)  │
             ┌──────────────▼───────────┐   └──────┬──────────────────┘
             │  dbstore (Postgres):     │          │
             │  users, repos, sessions  │   ┌──────▼──────────────────┐
             │  — metadata only, never  │   │  store: in-process      │
             │  file bodies             │   │  runtime cache (file    │
             └───────────────────────────┘   │  bodies + index),      │
                                              │  rebuilt on cache miss │
                                              └──────┬──────────────────┘
                                                      │
                                    ┌──────────────────▼───────────────────┐
                                    │  ai / diagram / semsearch             │
                                    │  → Anthropic Claude API (Messages)    │
                                    └────────────────────────────────────────┘
```

Cloned working trees live on disk (`REPO_STORAGE_DIR`) — source code is
**never** written to Postgres. Full storage-model reasoning, including
the Cloud Run ephemeral-disk case: [`ARCHITECTURE.md`](ARCHITECTURE.md).

<<<<<<< HEAD
---
=======
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
  GitHub Actions CI. Deployment targets: Render (Docker web service +
  managed Postgres, one-click via `render.yaml` — see `deploy/render.md`)
  and Google Cloud Run + Cloud SQL + Secret Manager (see
  `deploy/cloudrun.md`).
>>>>>>> 6dc9fe3 (Update project files and configurations)

## Tech stack

| Layer | Choice |
|---|---|
| **Backend** | Go 1.22 · stdlib `net/http` (`ServeMux` path variables — no web framework) · `database/sql` + `lib/pq` |
| **Database** | PostgreSQL 16 · embedded, versioned SQL migrations |
| **Auth** | GitHub OAuth (hand-rolled, no `x/oauth2`) + HS256 JWT (hand-rolled, no JWT library) · HttpOnly/SameSite=Lax cookies, server-revocable |
| **AI** | Anthropic Claude Messages API · one shared client for Q&A, diagrams, and search |
| **Frontend** | Single static HTML file, zero build step · `highlight.js` + `mermaid.js` via CDN |
| **Infra** | Docker (multi-stage, non-root) · docker-compose · GitHub Actions CI · Cloud Run + Cloud SQL + Secret Manager |

`github.com/lib/pq` is the **only** third-party Go dependency in the
whole project — `database/sql` needs an actual driver; everything else
(routing, JWT, OAuth) is small enough to implement directly against the
standard library. See "Why Go," below, and [`ROADMAP.md`](ROADMAP.md)
for the reasoning.

---

## Key engineering decisions

- **Ownership enforced in SQL, not just Go code.** Every repo lookup is
  scoped by `(id, owner_id)` *in the query itself*
  (`GetRepositoryForOwner`), so a missing check in a handler can't leak
  another user's data — the query just returns no rows. Cross-tenant
  lookups return 404, never 403, so a caller can't even tell "doesn't
  exist" from "exists but isn't yours."
- **Metadata in Postgres, file bodies on disk — never the reverse.**
  Repo metadata (owner, URL, status, file tree — paths and sizes, no
  content) is the only thing persisted. The parsed content map and
  static-analysis index are pure in-process cache, rebuilt from disk on
  a miss — this is what makes it survive Cloud Run's ephemeral disk with
  zero special-casing.
- **Hybrid search, not vector search.** Semantic search and Q&A
  retrieval use keyword pre-filtering + LLM re-ranking rather than an
  embeddings database — every result is also validated against the real
  file set before it's shown, so the model can't hallucinate a path into
  existence. Documented ceiling and reasoning in `ARCHITECTURE.md`.
- **Synchronous import.** Clone → parse → index happens inline within
  `POST /api/import`, keeping the flow simple to reason about and test,
  at the cost of the request taking as long as the clone does. Async
  import with a job queue is the next thing in `ROADMAP.md`.

---

## Performance

The incremental indexing engine (`backend/internal/index`) only
re-analyzes files whose content hash actually changed — a byte-identical
file never reaches an analyzer.

```bash
make bench
```

On a 1,000-file synthetic repo, an incremental `Update` with no real
changes ran in ~2.7ms/op vs. ~55ms/op for a full `Build` — **~20x
faster**, purely from skipping unchanged files. The concurrent worker
pool behind `Build` is proven leak-free under cancellation
(`TestRunPool_NoGoroutineLeaks`) and passes `go test -race` across the
whole project.

---

## Security

- **Path traversal** — every file read resolves against the real clone
  root and rejects anything that escapes it (`../`, absolute paths,
  symlink escapes), covered by explicit attack-case tests.
- **SQL injection** — 100% parameterized queries, zero string-concatenated SQL.
- **Sessions** — HttpOnly + SameSite=Lax cookies, `Secure` forced in
  production, constant-time (`crypto/subtle`) JWT signature checks,
  server-side revocation via a `sessions` table.
- **CSRF on the OAuth callback** — a random `state` token must round-trip
  through both a cookie and server memory before a callback is honored.
- **CORS is default-deny** — only ever reflects the configured
  `FRONTEND_ORIGIN` back, never an arbitrary request Origin.
- **No hardcoded secrets** — `.env` is gitignored; `config.Load` refuses
  to start in `TEAHUB_ENV=production` without real
  `JWT_SIGNING_KEY`/`DATABASE_URL`/OAuth credentials set.
- **Rate limiting** — per-user token buckets on the two expensive
  endpoint categories (`/api/import`, `/api/ask`+`/search`); Demo Mode
  gets the same treatment keyed by IP instead.
- **Non-root container** — the Docker image never runs as root.

Full list and reasoning: [`ARCHITECTURE.md`](ARCHITECTURE.md) § Security.

---

## Testing & CI

```bash
make test              # go test ./... — no DB required
make test-integration  # dbstore tests against a real, throwaway Postgres
make race               # go test -race ./...
make bench               # index package benchmarks
make vet
make fmt                 # gofmt -l . — should print nothing
```

`.github/workflows/ci.yml` runs `gofmt`, `go vet`, `go build`, `go test`
(with a real Postgres service container), `go test -race`, and a Docker
build on every push. Integration tests skip automatically — not fail —
when `TEAHUB_TEST_DATABASE_URL` isn't set, so `make test` works with
zero setup.

---

## Local setup (full stack)

Requires Go 1.22+, Docker (for Postgres), and an
[Anthropic API key](https://console.anthropic.com/).

```bash
git clone https://github.com/nutan654/Teahub.git && cd Teahub
go mod tidy                # fetches github.com/lib/pq, generates go.sum
cp .env.example .env       # fill in ANTHROPIC_API_KEY, and
                            # GITHUB_CLIENT_ID/SECRET for sign-in to work
docker compose up --build  # Postgres + TeaHub on :8080
```

Or against a Postgres you run yourself:

```bash
export DATABASE_URL=postgres://teahub:teahub@localhost:5432/teahub?sslmode=disable
export ANTHROPIC_API_KEY=sk-ant-...
export JWT_SIGNING_KEY=$(openssl rand -base64 48)
export GITHUB_CLIENT_ID=... GITHUB_CLIENT_SECRET=... GITHUB_OAUTH_REDIRECT_URL=http://localhost:8080/auth/github/callback
make run
```

Open `http://localhost:8080`, sign in with GitHub, and import a repo.
For zero-Postgres local hacking, see `TEAHUB_IN_MEMORY_STORE` in
[`.env.example`](.env.example) — never set in production.

---

## Deploying it for real

`deploy/cloudrun.md` documents the exact `gcloud` commands for Cloud Run
+ Cloud SQL + Secret Manager, including the extra `DEMO_REPO_URL`
env var to keep the zero-setup demo live at the same URL. Not
deployed anywhere yet — that's next.

<<<<<<< HEAD
---
=======
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

**Not currently deployed anywhere** — deploy from here using either path:
`render.yaml` at the repo root is a Render Blueprint that provisions a
Docker web service plus a managed Postgres database in one step (see
`deploy/render.md` for the full walkthrough, Blueprint and
manual-dashboard versions alike), or `deploy/cloudrun.md` documents the
equivalent `gcloud` commands for Cloud Run + Cloud SQL + Secret Manager.
Both are written and reviewed but not executed from this repo. Follow
either end-to-end to get a real URL, then update this section with the
live link. (If you're keeping the existing Vercel deployment of
`frontend/index.html` rather than letting Render serve it directly, see
the "Notes for a Vercel-hosted frontend" section in `deploy/render.md`
for wiring `FRONTEND_ORIGIN` and the OAuth callback URL between the two.)

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
>>>>>>> 6dc9fe3 (Update project files and configurations)

## Project layout

```text
teahub/
├── backend/
│   ├── cmd/server/            entrypoint: config, graceful shutdown, wiring
│   └── internal/
│       ├── config/            all environment-derived settings, in one place
│       ├── auth/               GitHub OAuth flow + hand-rolled HS256 JWT
│       ├── httpmw/             auth middleware, logging, recovery, rate limiting
│       ├── dbstore/             Postgres: migrations, users, repositories, sessions
│       ├── memstore/            in-memory fake of dbstore's interface
│       ├── gitclone/            shallow-clone + URL validation
│       ├── parser/              filesystem walk → tree + indexed file contents
│       ├── fsread/              sandboxed single-file reads (traversal-safe)
│       ├── analysis/            per-language static analysis (Go/Python/JS)
│       ├── index/                incremental indexing: concurrent Build, hash-based Update
│       ├── ai/                   shared Claude client + keyword retrieval
│       ├── diagram/              structural summary + Mermaid generation/validation
│       ├── semsearch/            hybrid keyword-prefilter + LLM re-ranking, path-validated
│       ├── store/                in-process runtime cache, not persisted
│       └── api/                  HTTP handlers, routing (incl. demo.go — unauthenticated Demo Mode)
├── frontend/index.html        single-file UI, no build step (hljs + mermaid.js via CDN)
├── deploy/render.md            Render (Docker web service + managed Postgres) deployment guide
├── deploy/cloudrun.md          Cloud Run + Cloud SQL + Secret Manager deployment guide
<<<<<<< HEAD
├── .github/workflows/ci.yml    CI: fmt, vet, build, test, race, Docker build
=======
├── render.yaml                  Render Blueprint (one-step web service + Postgres provisioning)
├── .github/workflows/ci.yml    CI: fmt, vet, build, test, test -race, Docker build
>>>>>>> 6dc9fe3 (Update project files and configurations)
├── Dockerfile                  multi-stage, non-root runtime
├── docker-compose.yml           local TeaHub + Postgres
├── API.md · openapi.yaml        hand-written + generated API reference
├── ARCHITECTURE.md
└── ROADMAP.md
```

---


