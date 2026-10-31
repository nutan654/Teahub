# Architecture

## Current

```
Browser (frontend/index.html, vanilla JS, no build step; hljs + mermaid.js via CDN)
        │  fetch()
        ▼
Go HTTP server (backend/cmd/server)
   ├── POST /api/import                → gitclone.Clone → parser.Walk → index.Build → store.Put
   ├── POST /api/ask                   → ai.Retrieve → ai.BuildContext → ai.Client.Complete
   ├── GET  /api/repos/{id}/file       → fsread.Read (sandboxed to the repo's clone dir)
   ├── POST /api/repos/{id}/search     → semsearch.Search (keyword prefilter + LLM re-rank)
   ├── POST /api/repos/{id}/diagram    → diagram.Generate (or store.SetDiagram cache hit)
   ├── GET  /api/repos
   ├── GET  /api/health
   └── GET  /metrics                   → in-process request/error/latency counters (metrics.Collector)
        │  (all routes: withCORS(FRONTEND_ORIGIN); /api/import and /api/ask: per-user token-bucket rate limit)
        ▼
In-memory store.Store (map[id]*Repo, RWMutex-protected)
        │
        ▼
Repo.Index (backend/internal/index) — built once at import, queried (not rebuilt) by every feature that reuses it
        │
        ▼
backend/internal/analysis — per-file, per-language symbol extraction (go/ast, python3's ast, JS/TS heuristics)
```

Each package has one job and depends only on `store`'s types where needed, so any of them can be swapped independently:

- `gitclone` — shells out to `git clone --depth 1`. Swappable for `go-git` or a job queue if clone volume grows.
- `parser` — filesystem walk with an extension allowlist and size caps. This is where language-aware chunking (AST-based instead of whole-file) would go next.
- `fsread` — sandboxed single-file reads for the Interactive Repository Explorer. This package exists specifically to own path-traversal safety in one place (`Resolve`), rather than scattering "is this path safe" checks across handlers. It resolves symlinks on both the repo root and the final path and verifies containment after each resolution, so a symlink planted inside a cloned repo can't be used to escape it.
- `ai` — the shared Gemini API client plus keyword retrieval. `Client.Complete(ctx, systemPrompt, userMessage, opts...)` is the one method that actually talks to the network; every AI feature (Q&A, diagrams, semantic search, and future ones) builds its own prompt and calls it, rather than each feature owning its own HTTP code. `Client` takes its `BaseURL` and `HTTPClient` as fields specifically so tests can point it at an `httptest` mock server instead of the real API — see `client_test.go` for the pattern every other package's AI-integration tests follow. `Retrieve` is the keyword-scoring seam: replacing it with embedding similarity means changing this function's internals, not its callers.
- `diagram` — architecture diagram generation. Two concerns are deliberately separated: `BuildSummary` (pure, deterministic, no network — a repo's directory list + detected ecosystems + entrypoint guesses) and `Generate` (prompts Gemini with that summary, then validates the response via `ExtractMermaid` before returning it). The validation step matters: a diagram feature that silently renders malformed output erodes trust faster than one that fails with a clear error, so `ErrInvalidDiagram` is returned rather than forwarding unparseable text to the frontend.
- `semsearch` — hybrid semantic search. `buildCandidates` (pure, no network) narrows a repo to at most 60 files using keyword scoring, falling back to the rest of the indexed files when the query shares no literal words with anything (so a query like "prevent replay attacks" still gets a full candidate pool to judge semantically). `Search` sends that pool to Gemini and asks for a ranked JSON subset with reasons, then — this is the part worth reading `semsearch.go`'s comments for — validates every returned path against the actual candidate set before returning anything, so a model hallucinating a plausible-looking file path can never reach the UI. `TestSearch_ValidatesAgainstHallucinatedPaths` exercises exactly this.
- `store` — currently an in-memory map. The `Store` type is small enough that a Postgres-backed implementation with the same method set (`Put`, `Get`, `List`, `SetDiagram`) drops in without touching `api/`. Diagram caching intentionally goes through `Store.SetDiagram` (which takes the store's own lock) rather than a second per-`Repo` mutex — one lock per shared object, not two, to avoid the class of bugs where two locks guard the same data inconsistently. `Repo.Index` is the exception to that pattern, deliberately: `index.Index` has its own internal `sync.RWMutex` and is safe for concurrent use on its own, so it's read and mutated directly rather than being routed through `Store`'s lock — see the Concurrency model section below for why that's the right call here specifically.
- `analysis` — per-file, per-language static analysis. Extracts functions, methods, structs/classes, interfaces, constants, imports, doc comments, and TODO/FIXME markers. See the dedicated section below for what's real parsing vs. heuristic per language, and why.
- `index` — the incremental indexing engine built on top of `analysis`. `Build` does a full analysis pass once; `Update` re-analyzes only files whose content hash changed. See the dedicated section below.
- `api` — HTTP handlers. `Server.aiClient()` builds an `ai.Client` fresh from the environment on every call unless a test has injected one via `New(store, client)` — this matters because it means `GEMINI_API_KEY` can be set after the process starts (or rotated) without a restart, and it's what lets tests unset the env var *after* constructing the server and still observe the expected failure. `Server.WarmCache` (called once from `main.go`'s `run()` after construction) rehydrates the in-memory `Files`/`Index`/`Tree` cache for every repo that was already `ready` before this process started, so a restart doesn't force every repo through a cold re-clone+re-parse on its next request; repos whose local clone is missing are logged and left to warm lazily on first access instead of blocking startup.
- `httpmw` — cross-cutting HTTP middleware: request logging, session auth, and `UserRateLimiter`, an in-memory per-user token-bucket limiter guarding `/api/import` and `/api/ask` (the two request types that either shell out to `git clone` or spend a Gemini API call). See "Known limitations" below for why this is deliberately not distributed.
- `metrics` — a small dependency-free `Collector` (total requests, requests by status class, 5xx count, and a bounded recent-latency window for p50/p99) wrapping every route via `Collector.Middleware`, served as JSON at `GET /metrics`. Cloud Run's own dashboards already cover CPU/memory/instance-count, so this intentionally isn't a full metrics stack (e.g. Prometheus client) — swapping the internals later wouldn't change the `/metrics` contract.

## Request flow: asking a question

1. Client sends `{repoId, question}`.
2. `ai.Retrieve` scores every indexed file by keyword overlap with the question (path matches weighted 3x over body matches) and returns the top 8 paths.
3. `ai.BuildContext` assembles those files' contents plus a top-level tree summary, truncated to a fixed character budget so the prompt stays bounded regardless of repo size.
4. `Server.aiClient().Complete` sends `{system prompt, context + question}` to `POST https://api.anthropic.com/v1/messages` and extracts the text content block from the response. The Q&A-specific system prompt ("answer only from provided context, cite file paths") lives in the handler, not in the `ai` package — `ai.Client` stays a general-purpose caller so other features can bring their own prompts.
5. The answer and the list of source files used are returned to the client together, so the UI can show which files backed the answer — this is the "cite file paths" behavior from the system prompt made visible.

## Request flow: semantic code search

1. Client sends `{query}` to `POST /api/repos/{id}/search`.
2. `semsearch.buildCandidates` runs `ai.Retrieve` for a keyword-based first pass (up to 60 files), then — if the repo has files left over and the keyword pass didn't fill the budget — appends the remaining indexed files in deterministic (sorted) order. This means a query with zero keyword overlap with anything still gets a full candidate pool, not an empty one.
3. `semsearch.buildPrompt` describes each candidate as `path — first 300 chars of content` and asks Gemini to return a JSON array of `{path, reason}`, explicitly instructed to judge by meaning and to only use paths from the given list.
4. `semsearch.ExtractJSONArray` pulls the array out of the response (models sometimes wrap JSON in prose or a code fence despite instructions not to).
5. **Every returned path is checked against the actual candidate set before being returned to the caller.** This is the one non-negotiable step in the package: a model can claim relevance for a path it invented, but that path will never appear in the response the frontend sees. `TestSearch_ValidatesAgainstHallucinatedPaths` mocks a response containing exactly this and asserts the fake path is dropped.

## Request flow: generating an architecture diagram

1. Client switches to the Architecture tab; browser sends `POST /api/repos/{id}/diagram`.
2. If a cached diagram exists on the `Repo` and the client didn't pass `?refresh=true`, it's returned immediately with `cached: true` — **no network call is made**, which is asserted directly by the diagram-caching tests (the test unsets `GEMINI_API_KEY` entirely and still expects success).
3. Otherwise, `diagram.BuildSummary` derives top-level directories, ecosystems (from manifest files like `go.mod`/`package.json`), and likely entrypoints — no file contents are sent, only shape.
4. `diagram.Generate` calls `Client.Complete` with a diagram-specific system prompt instructing it not to invent components the structure doesn't support.
5. `diagram.ExtractMermaid` pulls the ` ```mermaid ` block out of the response and checks it starts with a real Mermaid diagram-type keyword (`flowchart`, `graph`, `sequenceDiagram`, etc.). A response that doesn't validate returns `ErrInvalidDiagram` rather than being shown to the user.
6. On success, the result is cached via `store.SetDiagram` and returned. The frontend renders it with `mermaid.js` (`securityLevel: 'strict'`, since the diagram source is model-generated text).

## Request flow: viewing a file (Interactive Repository Explorer)

1. Client clicks a file node in the tree (or a Search result); browser sends `GET /api/repos/{id}/file?path=...`.
2. Handler looks up the repo (404 if unknown), then calls `fsread.Read(repo.LocalPath, path)`.
3. `fsread.Resolve` joins the root and the requested path, evaluates symlinks on both, and rejects anything that resolves outside the root — before any file I/O happens. This is covered by `fsread_test.go` with five distinct traversal attack shapes (`../`, nested `../../`, absolute paths, mixed traversal) and again at the HTTP layer in `handlers_test.go`.
4. On success, content (capped at 512KB), detected language, size, and a truncation flag are returned as JSON. The frontend syntax-highlights it client-side with highlight.js.

## Incremental Index + Static Analysis Engine

This is TeaHub's foundational subsystem as of Phase 4 — the thing that turns it from "an AI wrapper with a nice UI" into a real developer intelligence platform, per the stated project goal. It's built and wired into `/api/import` in this phase; **it is not yet wired into Search, the Explorer, or Documentation** — those still use their original keyword/file-map-based approaches (see "Known limitations" below). The index exists now as a reusable, tested foundation; rewiring existing features onto it is deliberately left for a later, separate change so this phase's diff stays reviewable and its behavior stays isolated.

### Static analysis, per language (`backend/internal/analysis`)

| Language | Method | Why |
|---|---|---|
| Go | `go/parser` + `go/ast` + `go/printer`, stdlib only | This is a real parser, not a heuristic — Go ships its own compiler frontend in the standard library, so there's no reason to approximate. Function/method signatures are regenerated via `go/printer` (always syntactically valid) rather than hand-assembled from AST fields. |
| Python | Shells out to `python3 -c '<script>'` using Python's own `ast` module, output as JSON on stdout | There's no Python grammar parser in the Go standard library, and the sandbox this was built in can't reach the Go module proxy for a third-party one — which turned out to be the right constraint anyway, since it forced the simpler, more correct answer: Python already ships a real parser for its own language. `python3` is checked once via `exec.LookPath` (cached in a `sync.Once`, not re-checked per file) and analysis degrades gracefully (`Partial: true`, a clear `Warning`, TODO/FIXME extraction still runs) rather than failing the whole file when it's missing. |
| JavaScript/TypeScript | Line-based regex heuristics in Go | No JS/TS AST in the Go standard library, and a full parser dependency (or a Node.js subprocess, mirroring the Python approach) was judged not worth it for this phase. Every `FileAnalysis` this produces is marked `Partial: true` with an explicit warning — nothing downstream can mistake it for a complete parse. It handles the common shapes (named `function`, arrow function assigned to `const`, `class` + methods, `import`/`require`, `SCREAMING_CASE` constants) and will miss decorators, multi-line signatures, and destructured exports. |

TODO/FIXME extraction is language-agnostic by design: a single regex over raw lines, run identically regardless of comment syntax (`//`, `#`, `<!--`, ...). Go's own AST strips comments from doc-comment association in a way that would need separate handling per language anyway, so there was no complexity savings in trying to make TODO extraction "AST-aware" per language — a flat regex pass is simpler and equally correct for this specific marker-convention use case.

A parse failure in any single file (a Go syntax error, Python syntax error, or malformed JS) never aborts the whole index build — it's recorded as `Partial: true` + `Warning` on that file's `FileAnalysis`, and every other file still indexes normally. `TestBuild_PartialFailureDoesNotAbortWholeIndex` asserts this directly: one broken file, one good file, and the good file's symbols are still fully queryable.

### The Index (`backend/internal/index`)

**Complexity.** `Build` is O(n) in total file content size, since it analyzes every file exactly once — there is no re-walking within a single `Build` call. `Update` is O(m) in the size of the *changed* set only: computing each changed file's content hash is O(file size), but a file whose hash matches what's already stored is never passed to an analyzer at all. Deletion is O(1) per deleted path (map delete). Every query method (`SymbolsByKind`, `FindByName`, `TODOs`, `Stats`) is O(total symbols) — there's no secondary index by name or kind yet, which is a reasonable choice at the file counts a single-repo tool deals with (thousands, not millions, of symbols) but would be worth revisiting with real inverted indexes if `FindByName` ever needs to serve much larger result sets under load.

**The incremental guarantee, concretely.** `Update(ctx, changed, deleted, opts)` computes `hashContent(newContent)` for every entry in `changed` and compares it against the hash stored from the last `Build`/`Update` — only a mismatch gets added to the batch actually sent to the worker pool. This isn't just asserted by checking output correctness; `TestIndex_UpdateSkipsUnchangedFiles` wraps the real analyzer with an atomic call counter and asserts the exact count: 2 files → 2 calls on initial `Build`, then calling `Update` again with byte-identical content → **still 2 calls, zero new ones** — then changing only one of the two files → exactly 3 calls total, not 4. The benchmark numbers back this up at scale: `BenchmarkUpdate_NoChanges` against 1000 files runs in ~2.7ms/op versus `BenchmarkBuild_1000Files`'s ~55ms/op for a full rebuild — roughly a **20x** difference, entirely from skipping the hash-unchanged files before they ever reach an analyzer (measured on this environment's single-core sandbox; see the concurrency note below for why raw analysis time itself doesn't drop further from concurrency here specifically).

**Concurrency model (`pool.go`).** One producer goroutine feeds an unbuffered `jobs` channel; `concurrency` worker goroutines (default `runtime.NumCPU()`, overridable via `Options.Concurrency`) each analyze files and send results on an unbuffered `results` channel; one closer goroutine calls `wg.Wait()` then closes `results` once every worker has exited. Every send on both channels is wrapped in a `select` against `ctx.Done()`, which is what makes cancellation actually work rather than just being accepted and ignored: a cancelled context stops the producer from blocking on a full `jobs` channel, stops a worker from blocking on a full `results` channel, and — because the producer's `defer close(jobs)` always runs regardless of *how* it returns — still lets in-flight workers drain to a clean exit via their `for range jobs` loop instead of hanging. `runPool` itself is the only reader of `results` and always drains it to completion, so no worker can be left blocked trying to send to a channel nobody's reading. `TestRunPool_NoGoroutineLeaks` is the direct evidence for this: it snapshots `runtime.NumGoroutine()`, runs a pool against 100 slow jobs with the context cancelled partway through (once 5 jobs are in flight), and polls for the goroutine count to return to baseline within 2 seconds — proving cancellation doesn't leave workers stranded, not just asserting it by inspection.

Honest caveat, verified rather than assumed: **this sandbox has exactly 1 CPU core** (`runtime.NumCPU() == 1`, confirmed directly). `BenchmarkBuild_1000Files` (default concurrency) and `BenchmarkBuild_1000Files_Concurrency1` (forced serial) run within noise of each other here — 55.5ms vs. 52.4ms in one run, 59.1ms vs. 58.9ms in another — because there's no second core for the worker pool to actually parallelize onto; forcing `Concurrency: 8` on this single core measured *slightly slower* (60.1ms) purely from goroutine scheduling overhead, exactly as you'd expect. The pool's correctness (all files processed, results correctly separated from errors, respects the concurrency cap when there *is* room to use it — see `TestRunPool_RespectsMaxConcurrency`, which measures actual observed overlap) doesn't depend on multiple cores being available; the *performance benefit* does, and would show up as real wall-clock improvement on any multi-core machine, which is every realistic deployment target for this server.

**Race safety.** `go test -race ./...` passes across the whole project, including `index` and `analysis`. The two things a concurrent indexer most needs to get right — the pool's channel handoffs and the `Index`'s `sync.RWMutex`-guarded map access from concurrent `Build`/`Update`/query calls — are both exercised under `-race` by the existing test suite (the pool tests spin up real concurrent workers; `TestBuild_ManyFilesConcurrently` builds 200 files with `Concurrency: 8`).

### What's deliberately not done in this phase

- **Not wired into Search/Explorer/Docs yet.** The index's query API (`SymbolsByKind`, `FindByName`, `TODOs`) is designed to be the shared substrate those features described in `README.md`'s architecture diagram, but rewiring `semsearch` or the Explorer onto it is separate follow-up work, not part of this phase.
- **No secondary indexes** (by name, by kind) beyond the linear scans described above — not needed yet at realistic single-repo symbol counts, and adding one preemptively without a measured need would be exactly the kind of unnecessary complexity this project has tried to avoid elsewhere.
- **No file-watching / git-diff integration** to automatically call `Update` on new commits. `Update`'s signature (`changed map[string]string, deleted []string`) is shaped to make that straightforward to add later (a caller just needs to diff two commits and pass the changed/deleted sets), but nothing calls it that way yet — `/api/import` only ever calls `Build`.

## Known limitations (by design, for current scope)

- **Retrieval is keyword-first, not embeddings-based**, for Q&A, diagram grounding, and semantic search's candidate selection. Semantic search's *ranking* step is genuinely meaning-based (that's what the LLM re-rank does), but which files even get considered still starts from keyword overlap plus a fallback fill — a huge repo where the relevant file shares no keywords with the query *and* doesn't fit in the fallback budget could still be missed. Real embeddings remove that ceiling entirely; the `Retrieve`/`buildCandidates` seams are where they'd go.
- **The Incremental Index isn't wired into Search/Explorer/Docs yet** (see above) — it's built at import time and immediately available for future use, but existing features still work the way they did before this phase.
- **JS/TS static analysis is heuristic, not a real parse** — every result is marked `Partial` for exactly this reason. Decorators, multi-line signatures, and destructured exports will be missed.
- **Python analysis requires `python3` on the server's `PATH`.** It degrades gracefully (marked `Partial`, TODOs still extracted) rather than failing when it's absent, but structural Python symbols won't appear in that case.
- ~~In-memory store means repos don't survive a restart~~ — **resolved**: repository metadata (owner, URL, status, file tree) now persists in Postgres via `dbstore`; see the "Storage model" and "Persistence, auth, and deployment" sections below. `TEAHUB_IN_MEMORY_STORE=true` still exists as a local-dev-only convenience but must never be set in production.
- ~~No auth~~ — **resolved**: GitHub OAuth + session cookies gate every repository-scoped endpoint; see "Persistence, auth, and deployment" below.
- **Single shallow clone per import, synchronous** — and as of this phase, indexing happens synchronously within the same request too. Large monorepos will make `/api/import` slower. A background job queue (Stage 8 in `ROADMAP.md`) fixes this for import *and* indexing *and* diagram/search/documentation generation, which are the other calls that can take a few seconds.
- **The 512KB file-read cap in `fsread`** means very large generated files (bundled JS, data dumps) will be truncated in the explorer view. This is a deliberate ceiling, not a bug — the explorer is for reading source, not serving arbitrary blobs.
- **Semantic search sends up to 60 file snippets (300 chars each) in one prompt.** That's a bounded, predictable cost, but it means very large repos are sampled, not exhaustively searched, in the fallback-fill case described above.
- **The per-user rate limiter (`httpmw.UserRateLimiter`) is in-memory and single-instance by design**, guarding `/api/import` (default 5/min, burst 3) and `/api/ask` (default 20/min, burst 10) — see `.env.example` for the corresponding env vars. On a single Cloud Run instance this is an exact bound; if the service ever scales to N concurrent instances, the effective per-user limit becomes `(configured rate × N)`, since each instance tracks its own buckets. A shared store (Redis, or a Postgres table) would be needed for a real distributed guarantee — same tradeoff already made for OAuth state tokens.
- **`/metrics` is in-process and unauthenticated**, tracking total requests, requests by status class, 5xx count, and a bounded recent-latency window (p50/p99) — see the `metrics` package above. It resets on restart and isn't aggregated across instances; treat it as a lightweight per-instance health signal, not a replacement for Cloud Run's own dashboards.
- **CORS is default-deny.** `withCORS` only ever reflects the `Access-Control-Allow-Origin` header back for requests whose `Origin` exactly matches `FRONTEND_ORIGIN`; it never echoes an arbitrary request origin. If `FRONTEND_ORIGIN` is unset, no CORS headers are added at all, so browsers fall back to same-origin-only behavior. This means: if you ever serve the frontend from a different origin than the API, you must set `FRONTEND_ORIGIN` explicitly or cross-origin requests will be silently rejected — that's the intended tightening, not a bug.

None of these are hidden — they're the honest boundary of what's built vs. not yet, and each maps to a specific item in `ROADMAP.md`.


## Persistence, auth, and deployment

The sections above describe TeaHub's indexing/AI core, which hasn't
changed. This section covers what was added on top: durable storage,
authentication, containerization, and a Cloud Run deployment target.

### Storage model

Two different kinds of state, deliberately kept apart:

1. **Metadata** (who owns which repo, its URL, import status, file tree —
   paths and sizes only, never content) lives in **Postgres**
   (`backend/internal/dbstore`). This is small, structured, and exactly
   what a relational database is good at; it's also the only state that
   *must* survive a restart for the product to make sense (you shouldn't
   lose your repo list because an instance recycled).
2. **File bodies and derived state** (parsed file content, the
   static-analysis index, the cached diagram) are never written to
   Postgres — putting arbitrary source blobs in a relational database is
   the wrong tool for the job (bloats the DB, no benefit over a
   filesystem/object store, makes backups slower for no reason). Cloned
   working trees live on disk under `REPO_STORAGE_DIR`
   (`backend/internal/gitclone.CloneInto`); the parsed-content map and
   analysis index live in an **in-process cache**
   (`backend/internal/store`), rebuilt from disk on a cache miss.

**The Cloud Run wrinkle:** Cloud Run's default local disk is ephemeral —
scoped to a single container instance, gone on restart or when traffic
scales to a new instance. Rather than requiring a persistent volume as a
hard dependency (see `deploy/cloudrun.md` step 7 for the optional GCS FUSE
mount that adds one), TeaHub treats a missing on-disk clone as a cache
miss just like a missing in-process cache entry: `api.Server.rehydrate`
re-clones from the repository's original URL, re-parses, and rebuilds the
index transparently before serving the request. This means "repository
metadata survives a restart" is true unconditionally (Postgres), while
"the repository is instantly available after a restart" depends on
whether a persistent volume is mounted — without one, the first request
after a cold start pays a re-clone; every request after that hits the warm
cache again. This tradeoff is documented, not hidden, and is the reason
`ListAllReadyRepositories` exists (a hook for optionally warming the cache
proactively at startup, not currently called — see ROADMAP).

### Ownership enforcement

Every repository-scoped query in `dbstore` takes `(id, ownerID)` as a pair
and filters on both in the SQL itself (e.g.
`WHERE id = $1 AND owner_id = $2`), rather than fetching by ID and
checking ownership afterward in Go. This matters because it means a
missing check in a *handler* can't leak cross-tenant data — the query
itself returns zero rows for a repo that isn't yours, full stop. The `api`
package layers a second convention on top: `repoLookupError` maps that
"zero rows" case to `404 Not Found`, never `403 Forbidden`, so a caller
probing repository IDs can't distinguish "doesn't exist" from "isn't
yours" — a deliberate choice to not leak existence information.

### Auth: GitHub OAuth + JWT sessions

Standard three-step OAuth2 authorization-code flow
(`backend/internal/auth/github.go`), implemented directly against
`net/http` rather than `golang.org/x/oauth2` — the flow GitHub requires
is small enough (build an authorize URL, exchange a code for a token,
fetch a profile) that a dependency wasn't justified. CSRF protection uses
a random `state` value that must round-trip through both a short-lived
cookie and server-side memory (`Server.states`) before a callback is
honored — noted in code as a documented limitation for multi-instance
deployments (login must complete on the same instance within 10 minutes;
acceptable for Cloud Run's default routing, would need to move to
Postgres/Redis if that stopped being true).

Sessions are HS256 JWTs (`backend/internal/auth/jwt.go`), also hand-rolled
against stdlib `crypto/hmac` rather than a JWT library, for the same
minimal-dependency reasoning. The JWT carries only a subject (user ID) and
a JWT ID (`jti`); every other profile field is looked up fresh from
Postgres on each request so a stale token can never serve stale profile
data. The `jti` is recorded in a `sessions` table specifically so a
session can be **revoked** (logout, or a future "sign out everywhere")
without needing full server-side session state for ordinary requests —
the hot path is a pure signature+expiry check; only logout and the
revocation check itself touch the `sessions` table.

### Migrations

A single embedded SQL file per migration (`backend/internal/dbstore/migrations`,
loaded via `go:embed`), applied in filename order inside a transaction,
tracked in a `schema_migrations` table. No external migration tool
(`golang-migrate`, `goose`, etc.) — the project's migration history is
young enough that a ~100-line hand-rolled runner
(`backend/internal/dbstore/db.go`) covers everything needed (apply-once,
idempotent re-runs, transactional per-file) without adding a dependency
for it. Forward-only for now (no down-migrations) since nothing has
needed to roll back a schema change yet; if that changes, down-migrations
should be added file-by-file rather than retrofitted all at once.

### Containerization

`Dockerfile` is a two-stage build: `golang:1.22-bookworm` compiles a fully
static binary (`CGO_ENABLED=0`), then a `debian:bookworm-slim` runtime
stage installs only `git` (TeaHub shells out to a real `git` binary at
request time — see `gitclone`) and `ca-certificates`, and runs as a
non-root user. `distroless` was considered and rejected for the runtime
stage specifically because reproducing `git`'s real shared-library
dependency graph (libpcre2, zlib, etc.) by hand on top of a minimal base
risks a silent missing-library failure that's hard to catch without
actually running the container in this environment — a real Debian base
guarantees those libraries are consistent with what `git` was built
against, which was judged the safer default for a project this size.

### CI

`.github/workflows/ci.yml` runs `gofmt -l`, `go vet`, `go build`,
`go test` (against a real ephemeral Postgres service container, so
`dbstore`'s integration tests actually run in CI), `go test -race`, and a
Docker image build, on every push/PR. Deploying to Cloud Run is
deliberately a separate, manual step (`deploy/cloudrun.md`) rather than an
automatic CI job, since it needs GCP credentials as repository secrets
that this template doesn't assume exist.

### Deployment target: Cloud Run, not Kubernetes

TeaHub is one stateless HTTP service with no need for pod-level
orchestration, custom schedulers, or sidecar containers beyond what Cloud
Run already manages for you (the Cloud SQL Auth Proxy). Kubernetes would
add real operational surface (cluster upgrades, node pools, ingress
controllers) for no corresponding benefit at this scope — Cloud Run's
autoscale-to-zero, managed TLS, and built-in request logging/metrics cover
everything this project needs. See `deploy/cloudrun.md` for the full,
step-by-step (unexecuted — no GCP credentials were available while
building this) deployment guide.
