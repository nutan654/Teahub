-- 0001_init.sql
-- Core schema: users (from GitHub OAuth), repositories (metadata only —
-- file bodies live on disk/object storage, never here), and sessions
-- (for JWT revocation / audit, since the JWT itself is stateless).
--
-- Migrations are applied in filename order by backend/internal/dbstore,
-- tracked in schema_migrations. Each file is a single forward migration;
-- TeaHub's schema is young enough that we haven't needed down-migrations
-- yet (see ARCHITECTURE.md "Migrations" for the tradeoff).

CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE IF NOT EXISTS schema_migrations (
    version     TEXT PRIMARY KEY,
    applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS users (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    github_id       BIGINT NOT NULL UNIQUE,
    github_login    TEXT NOT NULL,
    display_name    TEXT NOT NULL DEFAULT '',
    avatar_url      TEXT NOT NULL DEFAULT '',
    email           TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at   TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS repositories (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id        UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    url             TEXT NOT NULL,
    local_path      TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'pending',
        -- one of: pending, cloning, indexing, ready, failed
    error_message   TEXT NOT NULL DEFAULT '',
    file_count      INTEGER NOT NULL DEFAULT 0,
    tree_json       JSONB,
        -- the file tree is small (paths + sizes, no content) so it is
        -- persisted as JSON for fast reload after a restart; file bodies
        -- and the derived analysis index are never stored here (see
        -- ARCHITECTURE.md "Storage model").
    imported_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (owner_id, url)
);

CREATE INDEX IF NOT EXISTS idx_repositories_owner_id ON repositories(owner_id);
CREATE INDEX IF NOT EXISTS idx_repositories_status ON repositories(status);

CREATE TABLE IF NOT EXISTS sessions (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    jwt_id          TEXT NOT NULL UNIQUE,
        -- the JWT's "jti" claim; lets us revoke a specific issued token
        -- (logout, "sign out everywhere") without maintaining server-side
        -- session state for every request — normal requests only check
        -- the JWT signature/expiry, this table is consulted on
        -- logout/revocation paths only.
    user_agent      TEXT NOT NULL DEFAULT '',
    ip_address      TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at      TIMESTAMPTZ NOT NULL,
    revoked_at      TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_sessions_user_id ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_expires_at ON sessions(expires_at);
