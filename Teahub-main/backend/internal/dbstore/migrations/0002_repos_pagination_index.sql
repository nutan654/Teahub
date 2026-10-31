-- 0002_repos_pagination_index.sql
-- Supports ListRepositoriesForOwnerPage's keyset pagination query, which
-- filters on owner_id and orders by (imported_at DESC, id DESC). The
-- existing idx_repositories_owner_id index only covers the equality
-- filter; without this composite index, every page fetch beyond the
-- first would still need a sort over all of that owner's rows.

CREATE INDEX IF NOT EXISTS idx_repositories_owner_imported_id
    ON repositories (owner_id, imported_at DESC, id DESC);
