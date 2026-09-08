# Backup and restore

This document is the operator-facing procedure for Issue #13's backup/
restore acceptance criterion (AC6): how to take an online-safe SQLite
backup, verify it, and restore from one. It uses `cmd/backupctl`, a
host-local CLI in the same family as `miauthctl`/`jobsctl` — it runs on
the same host and against the same `DB_PATH` as the server, and requires
no HTTP surface or credentials of its own.

## Why not `sqlite3 .backup`

This service's runtime container image is
`gcr.io/distroless/static-debian12`, which ships neither a shell nor the
`sqlite3` CLI, and the storage layer only ever links
`modernc.org/sqlite` (a pure-Go driver — see `internal/storage/sqlite`'s
package doc comment). `backupctl backup` uses SQLite's own `VACUUM INTO`
statement instead, which this driver supports directly: no external
binary, shell, or additional dependency is required.

`VACUUM INTO` is safe to run against a live database in WAL mode without
stopping the server: it reads through SQLite's own consistent snapshot,
not the raw database file, so it also does not need to know about or
merge in the separate `-wal`/`-shm` side files a plain file copy (`cp`)
would silently miss data from.

## Taking a backup

```sh
bin/backupctl backup /path/to/backup-2026-09-05.db
```

This reads `DB_PATH` (and the same config file / environment variables as
`bin/server`; see [configuration.md](configuration.md)) to find the live
database, runs its migrations (a no-op against an already-migrated
database), and writes a complete snapshot to the given destination path.
The destination must not already exist — `VACUUM INTO` refuses to
overwrite a file, so a backup script must generate a fresh destination
path (for example one that includes a timestamp) on every run.

Run this on a schedule (cron, systemd timer) from the same host as the
server, and copy the resulting file to off-host storage as your
retention policy requires; `backupctl` itself does not manage retention
or off-host transfer.

## Verifying a backup

```sh
bin/backupctl verify /path/to/backup-2026-09-05.db
```

Opens the file read-only (it is never written to) and reports:

- **Schema migrations** — every embedded migration's checksum still
  matches what is recorded in the backup's own `schema_migrations` table.
  This reuses the exact same checksum comparison
  `internal/storage/sqlite.Migrate` enforces on every server startup (see
  [configuration.md](configuration.md)), so a backup that fails this
  check would also fail to start a server.
- **Row counts** for `actors`, `entries`, `jobs`, `external_sources`, and
  (Issue #77 PR7) `files` — enough for an operator to sanity-check the
  backup contains the data they expect. `files`' row count is Drive
  *metadata* only (see "Drive-backed media" below for the actual object
  bytes, which live outside this database).

Add `--deep` to additionally run SQLite's `PRAGMA integrity_check`, a
full scan of every table and index:

```sh
bin/backupctl verify --deep /path/to/backup-2026-09-05.db
```

This is deliberately not run by default — it is a full-database scan and
can take a long time on a large database — but is worth running
periodically, and always before relying on a backup for an actual
restore.

## Restoring

Restoring is a manual, documented procedure rather than a `backupctl`
subcommand, since it requires stopping the running server:

1. Stop `bin/server` (send `SIGTERM`/`SIGINT` and let it shut down
   gracefully, or otherwise stop the process/container).
2. Verify the backup first (see above) if you have not already.
3. Move the current `DB_PATH` file (and any `-wal`/`-shm` side files next
   to it) out of the way, in case it is still needed for forensics.
4. Copy or move the verified backup file to `DB_PATH`.
5. Start `bin/server` again. It runs its own migration check on startup
   (`internal/storage/sqlite.Migrate`), which acts as a second integrity
   net: a backup file that was silently corrupted in a way that changed
   its recorded migration checksums fails startup instead of serving
   with a damaged schema.

This procedure — including that the restored data, thread parent/child
relationships, job state, and an external source's fetch cursor all
survive intact — is covered by an automated restore-drill integration
test (`cmd/backupctl`'s
`TestRestoreDrill_BackupSurvivesSourceDestructionAndRestoresRelationships`),
not just documentation.

## Drive-backed media (Issue #77 PR7)

Everything above backs up and restores this database — including the
`files` table, one row of Drive metadata (owner, purpose, MIME, size,
`storage_key`, ...) per stored object. **It never touches the actual
object bytes** a `files` row's `storage_key` points at: those live in
whichever `internal/drive.Storage` backend `DRIVE_BACKEND` selects
(see [configuration.md](configuration.md)), entirely outside `DB_PATH`.
Restoring only the database without also restoring the matching object
bytes leaves every `files` row pointing at content that may no longer
exist — plan-77 v2 §2.7's own framing is that reconciling this
(`internal/drive.Service.RunOrphanGC`, `DRIVE_ORPHAN_GC_INTERVAL`) only
ever cleans up an object no row references, the opposite direction from
a row whose object is missing, which is instead a sign a restore's two
halves (database and object storage) came from different points in
time and should be redone together from backups taken at the same
moment.

The right way to back up the object bytes themselves depends on
`DRIVE_BACKEND`:

- **`localdisk`** (the default): `DRIVE_DATA_DIR` is an ordinary
  directory tree keyed by `storage_key` — back it up the same way you
  would any other directory of files on this host (`rsync`, `tar`, a
  filesystem/volume snapshot, ...) on the same schedule as the database
  backup above, ideally to the same off-host destination so a restore
  always pairs a database snapshot with the object tree taken at the
  same time. This repository does not provide a dedicated tool for this
  step — it is a plain directory, and every general-purpose backup tool
  already handles that well.
- **`s3compat`**: back up the bucket itself by enabling the object
  store's own versioning and (if it supports it) cross-region or
  cross-account replication, rather than pulling every object through
  this service to copy it elsewhere. Real Misskey-shaped deployments at
  any scale already lean on the object store's own durability/backup
  story for exactly this reason, and reimplementing bucket-level backup
  in this repository would duplicate infrastructure the object store
  already provides more robustly. Recommended, at minimum: enable
  bucket versioning so an accidental overwrite or delete
  (`drive/files/delete`, or a bug) can be undone from a previous
  version rather than being unrecoverable.

Restoring a Drive-backed deployment therefore has one extra step beyond
"Restoring" above: before starting `bin/server` again, also restore
`DRIVE_DATA_DIR` (localdisk) or confirm the S3 bucket's current state
(S3-compat) to the same point in time as the database snapshot you
restored. A database restored to a point in time when a file existed,
paired with object storage that already lost that object (or vice
versa), is exactly the metadata/object mismatch the previous paragraph
describes — it will not crash the server, but `GET /files/{id}` for that
file will 404 (localdisk) or fail (S3) until the mismatch is corrected.

## Known limitation

`backupctl` is a build target (`make build`) but, like `jobsctl`, is not
currently bundled into this service's distroless container image
(`Dockerfile`); run it from a host build of this repository, or extend
the image if you need it available inside a container.
