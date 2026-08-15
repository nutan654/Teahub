# Deploying TeaHub to Render

This documents the exact steps to deploy TeaHub to [Render](https://render.com)
using the existing multi-stage `Dockerfile` as-is, with Render's managed
Postgres for the database. None of this has been executed against a real
Render account from this environment — there are no Render credentials
available here. Treat every step below as reviewed-but-unexecuted; adjust
names/regions as needed.

Render (rather than Cloud Run — see `deploy/cloudrun.md`, which remains
valid and is not being replaced by this) is a reasonable default when you
don't have a GCP project handy: it builds directly from the Dockerfile
already in this repo, gives you a managed Postgres instance in the same
dashboard, and needs no CLI install or cloud IAM setup to get a public URL.

There are two ways to do this: the **Blueprint** (`render.yaml`, one click,
recommended) or **manual dashboard setup** (if you want to see/adjust every
option yourself). Both produce the same result.

## Option A — Blueprint (`render.yaml`)

This repo includes a `render.yaml` at the root that defines the web
service and the Postgres database together.

1. Push this repo (with `render.yaml` at the root) to GitHub/GitLab.
2. In the Render Dashboard: **New +** → **Blueprint**, and select this repo.
3. Render parses `render.yaml` and shows you everything it's about to
   create: the `teahub` web service (built from `Dockerfile`) and the
   `teahub-db` Postgres database. Click **Apply**.
4. You'll be prompted to fill in the handful of variables marked
   `sync: false` in `render.yaml` — these are secrets/deployment-specific
   values deliberately left out of the committed file:
   - `GITHUB_CLIENT_ID`, `GITHUB_CLIENT_SECRET` — from a GitHub OAuth App
     (see step 3 in Option B below; you can leave these blank on the
     first deploy and fill them in after, same chicken-and-egg note as
     `deploy/cloudrun.md`).
   - `GITHUB_OAUTH_REDIRECT_URL` — `https://<your-service>.onrender.com/auth/github/callback`.
     Render Blueprints don't support variable interpolation, so this has
     to be a literal URL you fill in once you know the assigned
     `onrender.com` hostname (or your custom domain).
   - `ANTHROPIC_API_KEY` — required for Q&A, diagram generation, and
     semantic search; the server still boots without it.
   - `DEMO_REPO_URL` — optional, leave blank unless you want Demo Mode on.
5. `DATABASE_URL` and `JWT_SIGNING_KEY` are filled in automatically
   (`fromDatabase` and `generateValue: true` respectively) — nothing to
   do there.
6. First deploy takes a few minutes (Go build + Docker image push).
   Render assigns a URL like `https://teahub.onrender.com`.

That's the whole thing. Skip to **Verify the deployment** below.

## Option B — Manual dashboard setup

If you'd rather not use the Blueprint, or want to attach the web service
to a Postgres instance you already have:

### 1. Create the Postgres database

Dashboard → **New +** → **PostgreSQL**.
- Name: `teahub-db`
- Plan: Free (fine for evaluation; upgrade for anything real — the free
  tier expires after 30 days and has no backups)
- Region: pick the same region you'll use for the web service below
  (cross-region adds latency and, on some plans, egress cost)

Once created, copy the **Internal Database URL** shown on the database's
page — it looks like
`postgres://teahub:<password>@<host>/teahub` — you'll paste this as
`DATABASE_URL` in step 2. Use the *internal* URL, not the external one:
it's faster and doesn't count against Render's external bandwidth, and
it's reachable precisely because the web service and database live in
the same Render region/private network.

### 2. Create the web service

Dashboard → **New +** → **Web Service** → connect this repo.
- **Environment**: Docker (Render detects the `Dockerfile` automatically;
  no build/start command needed — the Dockerfile's own `ENTRYPOINT` is
  used)
- **Plan**: Starter (or Free, with the caveat below)
- **Health check path**: `/api/health`
- **Environment variables** (Dashboard → your service → Environment):

  | Key | Value |
  |---|---|
  | `TEAHUB_ENV` | `production` |
  | `SECURE_COOKIES` | `true` |
  | `PORT` | `8080` |
  | `REPO_STORAGE_DIR` | `/data/repos` |
  | `DATABASE_URL` | the Internal Database URL from step 1 |
  | `JWT_SIGNING_KEY` | output of `openssl rand -base64 48` |
  | `GITHUB_CLIENT_ID` | from step 3 below |
  | `GITHUB_CLIENT_SECRET` | from step 3 below |
  | `GITHUB_OAUTH_REDIRECT_URL` | `https://<your-service>.onrender.com/auth/github/callback` |
  | `ANTHROPIC_API_KEY` | your Anthropic API key |
  | `ANTHROPIC_MODEL` | `claude-sonnet-5` |

  (`DEMO_REPO_URL` and the rate-limit/request-size variables in
  `.env.example` all have working defaults — only add them if you need
  to change the defaults.)

Render terminates TLS in front of the container and always talks plain
HTTP to it, so `SECURE_COOKIES=true` here is correct even though the
container itself never sees HTTPS directly — same reasoning as the Cloud
Run guide.

### 3. Create a GitHub OAuth App

At https://github.com/settings/developers → **New OAuth App**:
- **Homepage URL**: `https://<your-service>.onrender.com`
- **Authorization callback URL**: `https://<your-service>.onrender.com/auth/github/callback`

You won't know the exact `onrender.com` hostname until after the first
deploy. Easiest path: deploy once with `GITHUB_CLIENT_ID`/`GITHUB_CLIENT_SECRET`
left blank (the server starts fine; sign-in just won't work until
configured), note the assigned URL, then create the OAuth App, fill in
the two secrets, and let Render redeploy (or click **Manual Deploy**).

### 4. Persistent repository storage (optional)

By default, `REPO_STORAGE_DIR` (`/data/repos`) sits on Render's disk for
that service instance. On the **Free** plan there's no persistent disk
option at all, and even on paid plans a disk survives restarts but is
tied to that one service instance. TeaHub already tolerates this either
way — see `gitclone.CloneInto`'s doc comment and `api.Server.rehydrate` —
by re-cloning from the original URL on a cache/disk miss. Repository
*metadata* (owner, URL, status, file tree) is always durable in Postgres
regardless of this setting; this only affects whether the actual cloned
working tree needs to survive too, which mainly matters for very large
repos where re-cloning is expensive.

To make it durable: Dashboard → your service → **Disks** → **Add Disk**,
mount path `/data`, and give it enough size for however many repos you
expect to keep cloned at once (1 GB is a reasonable starting point). This
is exactly what the `disk:` block in `render.yaml` already does if you
used Option A.

## Verify the deployment

```bash
SERVICE_URL=https://<your-service>.onrender.com
curl -sf "$SERVICE_URL/api/health"
curl -sf "$SERVICE_URL/api/ready"   # checks DB connectivity
```

If `/api/ready` returns 503, check the service's **Logs** tab in the
Render Dashboard for the specific DB connection error — the most common
cause is having pasted the *external* database URL somewhere that only
the internal one works, or a region mismatch between the web service and
the database.

## Notes for a Vercel-hosted frontend

If you're keeping a separate frontend deployment on Vercel rather than
letting this Docker image serve `frontend/index.html` itself (the
default — see `Dockerfile`'s `COPY --chown=teahub:teahub frontend
/app/frontend`), point that Vercel deployment's API calls at
`https://<your-service>.onrender.com` and set `FRONTEND_ORIGIN` on the
Render web service to your Vercel URL (e.g.
`https://your-app.vercel.app`) so CORS and the post-login OAuth redirect
both allow it. `GITHUB_OAUTH_REDIRECT_URL` still points back at the
Render service's own `/auth/github/callback` either way — that callback
is the backend completing the OAuth handshake, not the frontend.

## Rolling back

Render dashboard → your service → **Events**/**Deploys** tab → pick a
previous successful deploy → **Rollback to this deploy**. No CLI needed.

## What's NOT covered here (by design)

- **CI/CD auto-deploy on merge** — Render auto-deploys on push to the
  connected branch by default once the service exists; nothing extra to
  wire up (unlike the Cloud Run guide, which needs a manual `gcloud`
  step or Workload Identity Federation in CI). `.github/workflows/ci.yml`
  still runs fmt/vet/build/test on every push independently of this.
- **Custom domain + managed SSL** — Dashboard → your service → **Settings**
  → **Custom Domains**, standard Render flow, not TeaHub-specific.
- **Cloud Run** — still fully documented in `deploy/cloudrun.md` if a GCP
  project becomes available again later; the two guides aren't mutually
  exclusive.
