# Roadmap

Ordered as a 10-stage internal-platform build. Each stage is meant to land fully working, tested, and documented before the next starts — no stage should ever be "half in."

## Done

**Stage 0 — Repo import + AI Q&A**
Clone + parse, file tree, keyword-retrieval Q&A over a codebase.

**Stage 1 — Interactive Repository Explorer**
Click any file in the tree to view its real content (syntax-highlighted, language auto-detected). Reads are sandboxed to the repo's clone directory — path traversal via `../` or symlinks is blocked in `fsread.Resolve` and covered by tests at both the package and HTTP layer. "Explain this file" is a one-click shortcut into the existing Q&A feature, not a separate AI path.

**Stage 2 — Architecture Diagram Generator**
A structural summary (directories, detected ecosystems, entrypoints — no source dumped) is sent to Claude, which returns a Mermaid diagram + explanation. The response is validated as real Mermaid before ever reaching the frontend (`diagram.ExtractMermaid` — malformed output fails loudly instead of rendering broken). Cached per-repo; regeneration is opt-in since it costs a real API call. Rendered client-side with mermaid.js.

**Stage 3 — Semantic Code Search**
Shipped as a hybrid, not dense-vector embeddings — see `ARCHITECTURE.md` for the reasoning (no embeddings provider is reachable from every deployment environment, and adding one for a single feature would itself violate "avoid unnecessary dependencies"). `semsearch.buildCandidates` narrows a repo to a bounded candidate pool via keyword scoring (falling back to the rest of the indexed files when there's zero literal overlap, so a query like "prevent replay attacks" still gets a full pool to judge), then Claude re-ranks that pool by actual meaning and explains each match. Every returned path is validated against the real candidate set server-side before reaching the UI — a model can't hallucinate a file into existence. The known ceiling: candidate *selection* is still keyword-first, so a huge repo where the truly relevant file shares no keywords with the query and falls outside the fallback budget could be missed. A true embeddings upgrade (see the "possible future upgrade" note below) removes that ceiling entirely without changing anything that calls `Search`.

**Phase 4 — Incremental Index + Static Analysis Engine**
The foundational shift from "AI wrapper" to developer intelligence platform: real per-language static analysis (`analysis` package — Go via `go/ast`, Python via a real `python3 ast` subprocess, JS/TS via honest heuristic regex, all documented with their exact tradeoffs in `ARCHITECTURE.md`) feeding an incremental index (`index` package) that's built once at import and re-indexes only changed files afterward, proven with a real call-counting test rather than just asserted. Concurrent worker pool with configurable parallelism, full context cancellation, and a goroutine-leak test. `go test -race` clean across the whole project; benchmarks show the incremental `Update` path running roughly 20x faster than a full `Build` at 1000 files. Deliberately **not** wired into Search/Explorer/Docs yet — see `ARCHITECTURE.md`'s "What's deliberately not done in this phase" — the index exists now as a tested, reusable foundation; rewiring existing features onto it is separate follow-up work so this phase's diff stayed reviewable in isolation.

## Next up

**Stage 4 — AI Documentation Generator**
Generate README/architecture-doc-style output for an *imported* repo (not this one) — reuses `ai.Complete` with a docs-specific system prompt, same pattern as `diagram`. Output should be a real Markdown file the user can download, not just chat text. Natural candidate to be the first feature actually built on top of Phase 4's index (real extracted doc comments and signatures as grounding, not just raw file content) rather than continuing the keyword-first pattern — worth deciding explicitly when this stage starts rather than defaulting to the old approach out of habit.

**Stage 5 — Technical Debt Analyzer**
Static-analysis-first, AI-second: run real tooling (cyclomatic complexity via existing Go/JS analyzers, duplicate-code detection) and use the model to summarize/prioritize findings in plain English — not ask the model to eyeball "is this complex," which would be unreliable and unverifiable.

**Stage 6 — Pull Request Review**
Requires the GitHub API for diffs, which means either public-repo-only PR review (no auth needed, works now) or waiting for Stage 9 (OAuth) to support private repos. Ship the public-repo version first.

**Stage 7 — Dependency Graph Visualization**
Parse import/require statements per language, render as an interactive graph (reuse mermaid.js `graph` type or move to a dedicated D3 view if the graphs get large). Mostly parser work, not AI work.

**Stage 8 — Background Job Queue + WebSockets**
Import, diagram generation, doc generation, and PR review all currently block the HTTP request for their duration. This stage makes them async with progress updates — needed before the platform can handle large repos or concurrent users gracefully. This is also where the in-memory `store` gets replaced with Postgres, since a job queue needs durable job state anyway.

## Possible future upgrade: real embeddings for search

The one real ceiling on Stage 3 as shipped: candidate *selection* for semantic search is keyword-first, so a repo where the truly relevant file shares no literal words with the query and falls outside the fallback budget could be missed. Fixing that means dense-vector embeddings — an embeddings API (Anthropic doesn't offer one directly; Voyage AI is Anthropic's recommended pairing) for indexing at import time, plus pgvector once Stage 8 brings in Postgres. Deliberately not done now: it's a new external API key and dependency for a ceiling that likely doesn't matter until repos get large, and `semsearch.Search`'s callers wouldn't need to change when it eventually lands.

## Done (continued) — Persistence, auth, deployment

**Stage 9 — GitHub OAuth + Postgres persistence + ownership**
Shipped: GitHub OAuth authorization-code flow, HS256 JWT sessions
(HttpOnly/SameSite cookie, revocable via a `sessions` table), Postgres
persistence for users/repositories/sessions via embedded migrations, and
ownership enforced at the SQL level on every repository-scoped endpoint
(404, not 403, for someone else's repo). Covered by HTTP-level ownership
tests (`api/handlers_test.go`) and real-Postgres integration tests
(`dbstore/dbstore_integration_test.go`, skipped without
`TEAHUB_TEST_DATABASE_URL`). See `ARCHITECTURE.md` "Persistence, auth, and
deployment" for the full design writeup. **What's unverified**: the code
was written and reviewed but the *live* OAuth round-trip against a real
registered GitHub OAuth App has not been exercised in this environment (no
network/credentials available) — you'll need to register an app (see
`.env.example`) and try a real sign-in once.

**Stage 10 — Containerization + CI + Cloud Run deployment target**
Shipped: multi-stage `Dockerfile` (non-root runtime), `docker-compose.yml`
(app + Postgres + disposable test-DB profile), GitHub Actions CI (fmt,
vet, build, test, test -race against a real Postgres service container,
Docker build), health (`/api/health`) and readiness (`/api/ready`, checks
DB connectivity) endpoints, structured JSON request logging with request
IDs (`httpmw`), and a full step-by-step Cloud Run + Cloud SQL + Secret
Manager deployment guide (`deploy/cloudrun.md`). **What's unverified**: no
GCP project, billing, or credentials were available in this environment —
the Dockerfile has not actually been built, and no live Cloud Run service
exists yet. Run `docker build .` and follow `deploy/cloudrun.md` yourself
to get a real deployment and a live URL.

## Deliberately deferred / cut for realism
Voice interaction, real-time multiplayer collaboration, a live coding-interview mode, and a browser-embedded terminal from the original brief are each their own significant project (WebRTC signaling, CRDTs, sandboxed code execution). Not on this roadmap — revisit only after Stage 10, and only if there's a concrete reason to want them, since a platform that does ten things well beats one that does fourteen things thinly.

## First Contribution Mode (flagship capstone)
Deliberately placed after Stage 5 rather than done first, because it's a composition of existing capabilities, not a new one:
- "Explain the architecture" → reuses Stage 2 (diagram) + Stage 0 (Q&A) — ready now
- "Suggest realistic issues" grounded in actual code → reuses Stage 3 (semantic search), now shipped, to find genuinely related code across the repo rather than just keyword-adjacent files
- "Identify beginner-friendly files" and "estimate difficulty" → needs Stage 5's static analysis (a file's real complexity/churn, not the model's guess) to be trustworthy rather than plausible-sounding — the remaining blocker
- "Link directly to code locations" → the Stage 1 explorer already provides this UI primitive

Building it before Stage 5 lands would mean faking the difficulty/beginner-friendliness claims (the model just asserting them with no analysis behind them). Once Stage 5 lands, First Contribution Mode becomes a thin orchestration layer over real capabilities — which is both faster to build well and a stronger thing to demo, since every claim it makes ("this file is beginner-friendly," "this issue touches these 3 files") will be traceable to real analysis instead of a single unverified model call.
