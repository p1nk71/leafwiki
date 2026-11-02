# ADR-0018: All canonical state lives in PostgreSQL; instances are disposable

## Status

Proposed (2026-11-02)

## Context

This is a fork decision for a private deployment, not an upstream LeafWiki change.

The operator's infrastructure runs containers in Docker/Kubernetes with an externally
managed, backed-up PostgreSQL. The goal: a wiki instance must be disposable — kill the
pod, start it anywhere, it reattaches to the database and continues. No persistent
volume, no per-instance VM, no per-app backup job.

ADR-0001 makes this impossible today: canonical data is Markdown files under `root/`,
asset files, revision blobs, per-slice SQLite databases and JSON settings files — all
under a single `data/` directory that must live on a persistent volume.

Constraints accepted for this fork:

- **Fork, not upstream PR.** Upstream's storage model is a product feature; this change
  deliberately diverges and will not preserve file-storage compatibility.
- **Ephemeral sessions.** After a restart everyone logs in again — session state does
  not need to survive.
- **No data migration.** Existing file-based wikis are not carried over; clean start.

Where the coupling lives today (from code survey):

- `internal/core/tree` (`node_store.go` et al.) — the page/section tree IS the
  filesystem: markdown files, `index.md`, `.order.json` ordering files.
- `internal/core/revision/fs_store.go` — content-addressed blobs + `_index.json`.
- `internal/core/assets`, `internal/branding`, avatars — binary files on disk.
- `internal/core/settings` — `Store[T]` over flat JSON files (branding.json,
  public-access.json, …); `internal/backup`'s git-backup.json (SecretBox-encrypted).
- SQLite real-state stores: users, sessions, email_tokens, api_keys, favorites,
  usersettings, share_links (`internal/core/auth/*_store.go`, `internal/favorites`,
  `internal/usersettings`, `internal/sharing`).
- SQLite derived indexes: links, tags, properties, search (FTS5) — rebuildable via
  resync (ADR-0004).
- `internal/restore`/`internal/snapshot` — file-level swap + ZIP backup.
- `internal/importer/plan_store.go` — transient import plans.

## Decision

**PostgreSQL becomes the single source of truth for all canonical state.**
`pgx/v5` + `pgxpool`, reached via `--database-url` / `LEAFWIKI_DATABASE_URL`.
Schema migrations: embedded SQL files applied in order by a small internal runner
(`schema_migrations` table) — the project's dependency rules favor ~50 lines of own
code over a migration framework.

| Canonical data | Today | In this fork |
|---|---|---|
| Page/section tree | `root/` markdown + `index.md` + `.order.json` | `nodes` table: id, parent, kind, slug, title, content (markdown+frontmatter), position, timestamps |
| Assets, avatars, branding images | files under `assets/` etc. | `bytea` columns; still served via the existing `/assets`, `/branding` HTTP routes |
| Revision blobs + metadata | content-addressed files | table rows keyed by the same SHA-256 content hash |
| Real state (users, email tokens, API keys, share links, user settings, favorites) | per-slice `*.db` | per-slice Postgres tables; SQL dialect rewritten (`?`→`$1`, `INSERT OR REPLACE`→`ON CONFLICT … DO UPDATE`, PRAGMA→schema migrations) |
| Settings JSON files | flat `*.json` in data dir | `settings` table (`key`, `value jsonb`); `settings.Store[T]` keeps its load/save/cache shape |
| Sessions | `sessions.db` | in-memory refresh-token registry — restart logs everyone out (accepted) |

**Derived indexes stay SQLite** (links, tags, properties, search FTS5) but move to a
local scratch location (tmpfs/`emptyDir`). They are already rebuildable by design:
on boot the resync walks the canonical store — now Postgres — and rebuilds them.
No Postgres-FTS work needed; FTS5 stays. Import plans are transient and stay
ephemeral the same way.

**Removed or reworked:**

- `internal/snapshot` + `internal/restore` file-swap — obsolete; data safety is the
  managed database's job (pg_dump/base backups). May later return as a DB→ZIP export.
- `internal/backup` git push — rework later as an "export markdown tree from DB to
  git" job if wanted; disabled initially.
- `--data-dir` shrinks to a scratch dir for derived SQLite (or `:memory:`); flags
  for snapshot/restore/git-backup removed or made no-ops with a warning.
- External file editing of the wiki content is gone — content is now DB rows.

### Rollout stages

1. **Infra**: `pgx` dependency, `--database-url`, migration runner, `postgres`
   service for CI/dev tests.
2. **Settings**: JSON `Store[T]` → `settings` table (smallest seam, proves the
   pipeline end to end).
3. **Auth + state stores** → Postgres: users, email tokens, API keys, share links,
   user settings, favorites; session store → in-memory.
4. **Tree** → `nodes` table (largest piece; keep the `NodeStore` method surface,
   replace internals).
5. **Assets / avatars / branding** → `bytea`.
6. **Revisions** → Postgres.
7. **Derived indexes**: scratch dir, resync-from-Postgres on boot.
8. **Cleanup**: remove file-swap restore/snapshot, disable git-backup push, flags
   and docs (`README`, `.env.example`, `docs/`), Dockerfile volume notes.

Each stage keeps `main` green: behind `--database-url`, the file path may remain
until its stage lands, but no mixed file+pg canonical state is left per stage.

## Alternatives considered

- **Keep SQLite + persistent volume**: rejected — the deployment target has no
  acceptable volume story; operator explicitly wants disposable instances.
- **Everything into Postgres including derived indexes**: rejected — derived data
  is rebuildable by design; re-running the existing resync against ephemeral SQLite
  is strictly less work and keeps search on proven FTS5.
- **Postgres FTS instead of FTS5**: rejected for now (extra work for no durability
  gain); noted as a possible later upgrade — `tsvector` offers Russian stemming,
  which FTS5 lacks.
- **Object storage (S3) for binary blobs**: rejected — `bytea` keeps one dependency
  and one backup boundary; asset sizes here are modest.
- **File→Postgres migration tooling**: rejected — clean start, no existing data.
- **Upstream contribution**: rejected — contradicts ADR-0001's product-level intent.

## Consequences

- The instance becomes disposable: any pod anywhere with `--database-url` serves
  the same wiki. Durability/backup story is exactly the managed Postgres one.
- Tests against stores need a Postgres instance (test service in CI or
  testcontainers); SQLite temp-dir tests stop covering the canonical path.
- Lost: inspect/edit content as files, git-diff of content history, trivial
  self-host for others, file-swap restore, easy upstream merges.
- Cost: near-total rewrite of the storage layer; every `*_store.go`, tree,
  revision, assets and the restore/snapshot/backup features are touched.
- Revisit if: the fork should again serve volume-less single-binary users
  (then keep an upstream-sync strategy), or asset sizes outgrow comfortable
  `bytea` rows (then add object storage).
