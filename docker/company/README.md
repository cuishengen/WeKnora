# DokuWiki source deployment

The company branch stays on upstream v0.9.0. Use `origin` for this fork and
`upstream` for Tencent/WeKnora. Keep knowledge files, database backups, account
data, API keys and actual source bindings outside the source repository.

The NAS connector reuses the knowledge-base data-source editor, scheduler,
queue, parser and index. Its service-defined alias is `vendor-wiki`; readers
never enter a server path or Wiki password. See
[the connector contract](../../internal/datasource/connector/dokuwiki/README.md)
for the JSON allowlist and namespace policy. Mount only the Wiki data directory
read-only, plus the reviewed ACL file separately when using its fingerprint
guard. It does not grant members the source Wiki's permissions automatically.

The first sync should run manually. Inspect parsed, failed and excluded items
before enabling `CRON_TZ=Asia/Shanghai 0 0 1 * * *`. Pausing automatic sync still
permits manual runs. Deletion sync is unsupported and must remain off; narrowing
the selection or moving a source does not remove prior publications.

## Build

The upstream `docker/Dockerfile.app` and `frontend/Dockerfile` can build the
complete source with the committed Go, Cargo and npm lockfiles. Record the
resolved build images, Go/Rust/Node/compiler versions, source commit, image ID
and validation results. Match the builder distribution to the runtime when
linking native libraries. The upstream v0.9.0 application runtime is Debian 12;
an unqualified Go image may use a newer distribution and produce an incompatible
glibc/libstdc++ binary even when the Go version matches.

The smaller assembly Dockerfiles here support a separately validated build:

1. Build `third_party/anydoc-go` with `scripts/build-anydoc-lib.sh` and its
   locked Cargo dependency graph. Compile with `make build-prod
   GO_BUILD_TAGS=anydoc`, setting the source commit/version fields. Keep CGO
   enabled and check the executable with `go version -m`.
2. Build the frontend with the Node version pinned in its Dockerfile,
   `npm ci`, type checking and `VITE_IS_DOCKER=true npm run build`.
3. Assemble the executable and migrations with `Dockerfile.app`, or the
   frontend `dist/` with `Dockerfile.ui`. Supply the verified upstream runtime
   and source revision as build arguments. If using an offline imported image,
   verify and record its full image config ID before assigning a local base tag.

Executable build dependencies should run on the container filesystem when a
NAS bind mount disallows execution. Caches and output artifacts may remain in
the controlled build area. Do not copy runtime secrets into either build context.

## Validation and rollback

Validate on a separate database, Redis, files directory and source fixture.
Preserve the existing encryption key when restoring model configuration; keep
the clone's automatic sources and external message channels disabled. Check
new/unchanged/replaced/rejected documents, logical source identity, raw-byte
hashes, authenticated citations and unpublished-resource denial.

The added migrations are PostgreSQL 135 and SQLite 52. A document becomes
visible only after its mandatory indexes are ready and source verification
succeeds. Cleanup follows publication; a failed version leaves the predecessor
usable. Historical versions are not archived by this change.

Before rolling back to an application without the publication barrier, pause
sources, stop their jobs and resolve unpublished candidates using the new
version. The down migration refuses to remove the barrier while staged rows
exist. Back up database, files and private encryption configuration, and verify
the rollback on a clone. Do not restore an old database over later user work.
