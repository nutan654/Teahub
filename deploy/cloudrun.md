# Deploying TeaHub to Google Cloud Run

This documents the exact manual steps to deploy TeaHub to Cloud Run with
Cloud SQL (Postgres) and Secret Manager. None of this has been executed
against a real GCP project from this repo/environment — there are no GCP
credentials available here. Treat every `gcloud` command below as reviewed-
but-unexecuted; run them yourself and adjust names/regions as needed.

Cloud Run (not GKE/Kubernetes) is intentional: TeaHub is a single stateless
HTTP service with no need for pod-level orchestration, custom schedulers,
or multi-container pods. Cloud Run gives autoscaling-to-zero, managed TLS,
and a much smaller operational surface for a project this size.

## 0. Prerequisites

- A GCP project with billing enabled.
- `gcloud` CLI installed and authenticated (`gcloud auth login`).
- APIs enabled:
  ```bash
  gcloud services enable run.googleapis.com \
    sqladmin.googleapis.com \
    secretmanager.googleapis.com \
    artifactregistry.googleapis.com \
    cloudbuild.googleapis.com
  ```

## 1. Create the Cloud SQL (Postgres) instance

```bash
gcloud sql instances create teahub-db \
  --database-version=POSTGRES_16 \
  --tier=db-f1-micro \
  --region=us-central1 \
  --storage-auto-increase

gcloud sql databases create teahub --instance=teahub-db

gcloud sql users create teahub \
  --instance=teahub-db \
  --password="$(openssl rand -base64 24)"
# Save that generated password — it goes into Secret Manager below, not
# into any file in this repo.
```

Cloud Run talks to Cloud SQL over a Unix socket via the Cloud SQL Auth
Proxy sidecar that Cloud Run manages automatically when you pass
`--add-cloudsql-instances`; the DSN format for that is:

```
postgres://teahub:<password>@/teahub?host=/cloudsql/<PROJECT_ID>:us-central1:teahub-db&sslmode=disable
```

## 2. Store secrets in Secret Manager

```bash
printf '%s' "postgres://teahub:<password>@/teahub?host=/cloudsql/<PROJECT_ID>:us-central1:teahub-db&sslmode=disable" \
  | gcloud secrets create teahub-database-url --data-file=-

openssl rand -base64 48 | tr -d '\n' | gcloud secrets create teahub-jwt-signing-key --data-file=-

printf '%s' "<your Anthropic API key>" | gcloud secrets create teahub-anthropic-api-key --data-file=-

printf '%s' "<GitHub OAuth App client ID>" | gcloud secrets create teahub-github-client-id --data-file=-
printf '%s' "<GitHub OAuth App client secret>" | gcloud secrets create teahub-github-client-secret --data-file=-
```

## 3. Create a GitHub OAuth App

At https://github.com/settings/developers → "New OAuth App":
- **Homepage URL**: `https://<your-cloud-run-url>`
- **Authorization callback URL**: `https://<your-cloud-run-url>/auth/github/callback`

You won't know the exact Cloud Run URL until after the first deploy (step
5), which is a chicken-and-egg problem solved by either: (a) deploy once
first without OAuth configured (the server starts fine, sign-in just 503s
until configured — see `config.Config.AuthEnabled`), note the assigned
URL, then create the OAuth App and update the secret + redeploy; or (b)
reserve a custom domain up front and map it in step 6, so the callback URL
is known before the first deploy.

## 4. Build and push the image

```bash
gcloud artifacts repositories create teahub \
  --repository-format=docker \
  --location=us-central1

export IMAGE=us-central1-docker.pkg.dev/<PROJECT_ID>/teahub/teahub:$(git rev-parse --short HEAD)

gcloud builds submit --tag "$IMAGE" .
# or locally: docker build -t "$IMAGE" . && docker push "$IMAGE"
```

## 5. Deploy to Cloud Run

```bash
gcloud run deploy teahub \
  --image="$IMAGE" \
  --region=us-central1 \
  --platform=managed \
  --allow-unauthenticated \
  --add-cloudsql-instances="<PROJECT_ID>:us-central1:teahub-db" \
  --set-env-vars="TEAHUB_ENV=production,SECURE_COOKIES=true,PORT=8080,GITHUB_OAUTH_REDIRECT_URL=https://<your-cloud-run-url>/auth/github/callback" \
  --set-secrets="DATABASE_URL=teahub-database-url:latest,JWT_SIGNING_KEY=teahub-jwt-signing-key:latest,ANTHROPIC_API_KEY=teahub-anthropic-api-key:latest,GITHUB_CLIENT_ID=teahub-github-client-id:latest,GITHUB_CLIENT_SECRET=teahub-github-client-secret:latest" \
  --min-instances=0 \
  --max-instances=4 \
  --memory=512Mi \
  --cpu=1 \
  --timeout=120 \
  --port=8080
```

`--allow-unauthenticated` makes the service publicly reachable at the HTTP
level (TeaHub does its own GitHub-OAuth-based authentication above that —
this flag is about Cloud Run's IAM layer, not TeaHub's session layer).

Cloud Run terminates TLS in front of the container and always talks plain
HTTP to it, which is why `SECURE_COOKIES=true` here is correct even though
the container itself never sees HTTPS directly.

## 6. Verify the deployment

```bash
SERVICE_URL=$(gcloud run services describe teahub --region=us-central1 --format='value(status.url)')
curl -sf "$SERVICE_URL/api/health"
curl -sf "$SERVICE_URL/api/ready"   # checks DB connectivity
```

If `/api/ready` returns 503, check `gcloud run services logs read teahub`
for the specific DB connection error — the most common cause is the Cloud
SQL instance connection name in `--add-cloudsql-instances` not matching
the `host=/cloudsql/...` value baked into the `DATABASE_URL` secret.

## 7. Persistent repository storage (optional)

By default, `REPO_STORAGE_DIR` (`/data/repos` in the container) sits on
Cloud Run's ephemeral, per-instance local disk: it disappears whenever an
instance is recycled or a new revision rolls out. TeaHub already tolerates
this — see `gitclone.CloneInto`'s doc comment and `api.Server.rehydrate` —
by re-cloning from the original URL on a cache/disk miss. Repository
*metadata* (owner, URL, status, file tree) is always durable in Postgres
regardless of this setting; this section is only about whether the actual
cloned working tree needs to survive too, which matters mainly for very
large repos where re-cloning is expensive.

To make it durable, mount a Cloud Storage bucket via GCS FUSE as a Cloud
Run volume (Cloud Run 2nd gen execution environment):

```bash
gcloud storage buckets create gs://<PROJECT_ID>-teahub-repos --location=us-central1

gcloud run services update teahub \
  --region=us-central1 \
  --execution-environment=gen2 \
  --add-volume=name=repos,type=cloud-storage,bucket=<PROJECT_ID>-teahub-repos \
  --add-volume-mount=volume=repos,mount-path=/data/repos
```

## 8. Logging and metrics

The app writes structured JSON logs to stdout in production
(`httpmw.NewLogger`, gated on `TEAHUB_ENV=production`) — Cloud Run ships
container stdout/stderr to Cloud Logging automatically, no sidecar or
extra configuration needed. Each request log line includes a request ID,
method, path, status, duration, and remote address (see
`httpmw.AccessLog`); panics are logged with a full stack trace before the
500 is returned (`httpmw.Recover`).

For metrics beyond what Cloud Run's built-in request-count/latency/
container-CPU dashboards already provide (Cloud Run > your service >
Metrics tab, no setup required), the structured logs above are queryable
directly in Cloud Logging's Logs Explorer, e.g.:

```
resource.type="cloud_run_revision"
jsonPayload.msg="http_request"
jsonPayload.status>=500
```

A dedicated `/metrics` Prometheus endpoint was deliberately not added —
see ROADMAP.md's "Deliberately not done" section for why, and what would
change if this became a multi-service deployment where cross-service
metric correlation actually mattered.

## 9. Rolling back

```bash
gcloud run revisions list --service=teahub --region=us-central1
gcloud run services update-traffic teahub --region=us-central1 \
  --to-revisions=<previous-revision-name>=100
```

## 10. What's NOT covered here (by design)

- **Kubernetes** — deliberately not used; see the top of this file and
  ROADMAP.md.
- **CI/CD auto-deploy on merge** — `.github/workflows/ci.yml` builds and
  tests every push but does not deploy; wiring the `gcloud run deploy`
  command above into a workflow step needs GCP credentials stored as
  repository secrets (`GCP_SA_KEY` or, better, Workload Identity
  Federation), which is a per-fork decision this template doesn't make for
  you.
- **Custom domain + managed SSL** — `gcloud run domain-mappings create`,
  standard Cloud Run flow, not TeaHub-specific.
