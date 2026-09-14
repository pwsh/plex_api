# plex-api

A small HTTP service that exposes the Plex Media Server library SQLite
databases as JSON. It is written in Go with the standard library only: no cgo,
no SQLite driver, no external dependencies.

## Documentation

| Document | What it covers |
| --- | --- |
| [docs/Architecture.md](docs/Architecture.md) | topology, delivery as a docker mod, start-up sequence, request path |
| [docs/Benchmark.md](docs/Benchmark.md) | head-to-head latency and throughput against the official Plex API |
| [docs/PerformanceReview.md](docs/PerformanceReview.md) | the optimisation passes and what each bought |
| [docs/Unraid.md](docs/Unraid.md) | installing on Unraid: template variables, verification, safe writes |
| [deploy/README.md](deploy/README.md) | the mod and layer images, s6 services, publishing |
| [docs/PlexDirectAccessFeasibility.md](docs/PlexDirectAccessFeasibility.md) | why stock SQLite is not enough and what the Plex engine provides |
| [docs/PlexDatabaseLayout.md](docs/PlexDatabaseLayout.md) | the library schema, type codes, joins and an integrity review |

## Why it drives Plex's own SQLite shell

Plex's library database cannot be fully served by stock SQLite. The
`fts4_metadata_titles_icu` and `fts4_tag_titles_icu` virtual tables use a
`collating` tokenizer, and `index_title_sort_icu` uses an `icu_root` collation,
both of which exist only inside the `Plex Media Server` executable — not in the
bundled `libsqlite3.so`, and not as a loadable extension. With stock SQLite you
get `unknown tokenizer: collating` and `no such collation sequence: icu_root`,
which means no full-text search and no `INSERT`, `DELETE` or title/`title_sort`
`UPDATE` on `metadata_items`, and no writes to `tags` at all.

So every query here is executed by the shell Plex ships:
`<PLEX_API_PMS_DIR>/Plex SQLite`, an 11 KB stub that re-executes the server
binary in sqlite3-shell mode with `LD_LIBRARY_PATH` pointing at
`<PLEX_API_PMS_DIR>/lib`. The engine is therefore always the exact one PMS
uses, and it upgrades when PMS upgrades.

## The read pool

Starting that shell costs about 16 ms, which used to dominate every request:
the queries behind `/v1/items/{id}` take well under 1 ms once a shell is warm.

So reads go through a pool of long-lived shells instead. `PLEX_API_POOL`
(default 4) shells per database are started lazily with `-readonly -bail`, get
`.timeout`, `.mode json` and `.headers off` once, and then serve one request at
a time: the parameters and statements of a request are written to stdin framed
by the usual sentinels and terminated by a unique `.print` end marker, and
stdout is read back up to that marker.

Writes are unaffected: `/v1/exec` and `PATCH /v1/settings` still spawn a
private read-write process each, because they need `BEGIN IMMEDIATE` isolation
and `-bail`'s "exit before COMMIT" rollback.

Because `-bail` makes any SQL error exit the process, a failing pooled request
shows up as EOF on stdout; the driver then reads stderr, reports the same
structured error the spawn path would have (including a script-relative line
number), discards the process and starts a fresh one on the next request. A
request that outruns `PLEX_API_QUERY_TIMEOUT` kills its shell the same way.

**State leakage is the risk of sharing a connection**, and it is contained
three ways:

* `.parameter clear` is written before every request, so a binding from one
  request can never be read by the next;
* a shell is recycled after `PLEX_API_POOL_MAX_USES` requests (default 1000);
* caller-supplied SQL whose statements begin with `ATTACH`, `DETACH` or
  `PRAGMA` never runs on a pooled shell. `POST /v1/query` detects those and
  transparently falls back to a private spawned process, so such a statement
  works exactly as before and cannot change the state of a shared connection.
  (Internal endpoints' own `PRAGMA table_info` calls are read-only and stay
  pooled.)

Set `PLEX_API_POOL=0` to switch the pool off and go back to spawning a process
per request for everything.

Measured against a 966 MB real library:

| | pool off | pool on (4) |
| --- | --- | --- |
| `GET /v1/items/{id}`, 30 sequential, median | 18.6 ms | 2.5 ms |
| `GET /v1/search?q=star&limit=50`, 30 sequential, median | 17.9 ms | 2.0 ms |
| `GET /v1/items/{id}`, 8 concurrent clients | 260 req/s | 1130 req/s |

The service must run on the PMS host, as a user that can read (and, for
writes, write) the database files.

Background on the schema and on what is and is not safe to touch:
`docs/PlexDatabaseLayout.md` and `docs/PlexDirectAccessFeasibility.md`.

## Build and run

```sh
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' ./cmd/plex-api
./plex-api -version
PLEX_API_PMS_DIR=/usr/lib/plexmediaserver \
PLEX_API_DB_DIR="/config/Library/Application Support/Plex Media Server/Plug-in Support/Databases" \
  ./plex-api
```

The result is a static binary. It logs one line per request to stdout
(`method path status duration bytes`) and shuts down gracefully on SIGTERM.

Container packaging lives in `deploy/`.

## Configuration

All configuration is by environment variable.

| Variable | Default | Meaning |
| --- | --- | --- |
| `PLEX_API_BIND` | `0.0.0.0:32500` | listen address |
| `PLEX_API_PMS_DIR` | `/usr/lib/plexmediaserver` | PMS install; shell is `<dir>/Plex SQLite`, libraries `<dir>/lib` |
| `PLEX_API_DB_DIR` | `/config/Library/Application Support/Plex Media Server/Plug-in Support/Databases` | directory holding `com.plexapp.plugins.library.db` and `…blobs.db` |
| `PLEX_API_TOKEN` | *(unset)* | when set, every endpoint except `/health` requires `Authorization: Bearer <token>` or `X-Plex-Api-Token: <token>` |
| `PLEX_API_WRITE` | `false` | when false, all write endpoints return 403 |
| `PLEX_API_WRITE_WHILE_RUNNING` | `false` | when false, writes return 409 if a TCP connect to `PLEX_API_PMS_ADDR` succeeds |
| `PLEX_API_PMS_ADDR` | `127.0.0.1:32400` | address probed to decide whether PMS is running |
| `PLEX_API_TIMEOUT_MS` | `5000` | SQLite busy timeout (`.timeout`) |
| `PLEX_API_QUERY_TIMEOUT` | `30s` | wall-clock limit before the subprocess is killed |
| `PLEX_API_POOL` | `4` | long-lived read-only shells per database; `0` spawns a process per request |
| `PLEX_API_POOL_MAX_USES` | `1000` | requests a pooled shell serves before it is recycled |
| `PLEX_API_INDEXES` | `false` | create the managed indexes at start (`-ensure-indexes`) |
| `PLEX_API_INDEX_BACKUP` | `true` | take an online backup before creating them |
| `PLEX_API_STATE_DIR` | `/config/plex-api` | holds `indexes.json` |
| `PLEX_API_BACKUP_DIR` | `<state dir>/backups` | one timestamped directory per backup |
| `PLEX_API_BACKUP_KEEP` | `3` | backups kept; older ones are pruned |

## Endpoints

Responses are JSON. Errors are `{"error": {"code": "...", "message": "...", "line": N}}`,
where `line` is the line of the generated script when SQLite reported one.

Assume `API=http://localhost:32500` below. With `PLEX_API_TOKEN` set, add
`-H "Authorization: Bearer $TOKEN"` to every call except `/health`.

### `GET /health`

Never requires authentication. Reports the service version, the shell path and
whether it exists, `sqlite_version()`, each database path/size/existence, the
row count and `max(version)` of `schema_migrations`, whether PMS looks like it
is running, and the effective write policy. Returns 503 when the shell or the
main database is missing or unreadable.

```sh
curl -s $API/health
```

### `GET /v1/schema`, `GET /v1/schema/{table}`

`sqlite_master` grouped into `tables`, `views`, `triggers`, `indexes`; then
`PRAGMA table_info`, the indexes, the triggers and the `CREATE` statement for
one object. Both accept `?db=blobs`.

```sh
curl -s $API/v1/schema | head -c 400
curl -s $API/v1/schema/metadata_items
```

### `GET /v1/tables/{table}`

Paged rows. `limit` (default 100, max 1000), `offset`, `order` (a column name,
validated against `PRAGMA table_info`; prefix `-` for descending), `db=blobs`.
The table name is validated against `sqlite_master`.

With no `order`, `metadata_items` is sorted the way PMS sorts library listings:
`ORDER BY title_sort COLLATE icu_root` (only this engine can evaluate it).

```sh
curl -s "$API/v1/tables/metadata_items?limit=2"
curl -s "$API/v1/tables/library_sections?order=-id"
curl -s "$API/v1/tables/blobs?db=blobs&limit=1"
```

**Blob columns.** Columns whose declared type contains `BLOB` are selected as
`hex("col")` and decoded to **standard base64** before the response is written.
This is deliberate: the shell's JSON writer emits raw bytes as `\uXXXX`
escapes, which is lossy for anything that is not valid text. The response lists
the affected columns in `blob_columns`.

### `GET /v1/items/{id}`

One `metadata_items` row plus `media_items` → `media_parts` → `media_streams`,
`taggings` joined to `tags` (`tag_type`, `tag`, `text`, `index`, offsets),
`metadata_item_settings` joined by `guid`, and the item's extras via
`metadata_relations`. Joins follow `docs/PlexDatabaseLayout.md` section 8.

```sh
curl -s $API/v1/items/4350
```

### `GET /v1/search?q=&limit=`, `GET /v1/tags/search?q=&limit=`

Full-text search through `fts4_metadata_titles_icu` and
`fts4_tag_titles_icu`. `q` is passed as a bound parameter to `MATCH`, so FTS
query syntax (`star*`, `"star wars"`, `a OR b`) works and injection does not.
`limit` defaults to 50, max 1000.

Item results carry both the indexed text (`fts_title`, `fts_title_sort`) and
the real `metadata_items` columns (`id`, `metadata_type`,
`library_section_id`, `year`, `guid`, `title`, `title_sort`, `parent_id`).

```sh
curl -s "$API/v1/search?q=star%20wars&limit=10"
curl -s "$API/v1/tags/search?q=spielberg"
```

### `GET /v1/recent?types=1,2&limit=20&section=`

The most recently added items of the given `metadata_type` codes (default
`1,2`: movies and shows), newest first, optionally within one library
section. Each type is fetched as its own ordered, limited branch and the
branches are merged, because a single `IN (...) ORDER BY added_at` cannot walk
an index; with the managed `zz_plexapi_type_added` index this is an index
walk (0.35 ms on 116k rows against 8 ms for the naive query). With `section`
set, Plex's own `(library_section_id, metadata_type, added_at)` index serves
it. `limit` defaults to 20, max 1000; at most 16 types.

```sh
curl -s "$API/v1/recent?types=1&limit=10&section=5"
```

### `GET /v1/indexes`

The managed index status: whether the feature is on, the PMS version, the
state file's contents (or `null`), and one entry per managed index with
`present`. One read round trip. There are no create or drop endpoints — both
need Plex stopped, so they are CLI modes (see "Managed indexes").

```sh
curl -s $API/v1/indexes
```

### `POST /v1/query`

Read-only SQL with named parameters.

```sh
curl -s $API/v1/query -H 'Content-Type: application/json' -d '{
  "sql": "SELECT id, title, year FROM metadata_items WHERE metadata_type = :t AND year > :y LIMIT 5",
  "params": {"t": 1, "y": 2015},
  "db": "main"
}'
```

Read-only-ness is enforced by the engine: the shell runs with `-readonly`, so a
write returns 403 `readonly` rather than being detected by inspecting the SQL.

Statements beginning with `ATTACH`, `DETACH` or `PRAGMA` are run in their own
throw-away process rather than on a pooled shell, so they behave as they always
did but cannot leave connection state behind for the next caller. Everything
else is served from the pool.

Multiple statements in one `sql` string are allowed. One result set comes back
as `{"rows": [...], "count": n, "columns": [...]}`; two or more come back as
`{"results": [{"rows": ..., "count": ..., "columns": ...}, ...]}`.

Parameters are bound as `:name`. Names must match `^[A-Za-z_][A-Za-z0-9_]*$`.
Values may be strings, numbers, booleans (`1`/`0`) or `null`; objects and
arrays are rejected. Numbers keep their exact JSON text, so 64-bit ids are not
mangled by float conversion.

### `POST /v1/exec`

Write statements, run in one `BEGIN IMMEDIATE … COMMIT` transaction with
`-bail`. The first failure stops the shell before `COMMIT`, so nothing is
committed and the response is a 4xx describing the failing statement.

```sh
curl -s $API/v1/exec -H 'Content-Type: application/json' -d '{
  "statements": [
    {"sql": "UPDATE media_parts SET file = replace(file, :old, :new) WHERE file LIKE :pat",
     "params": {"old": "/mnt/old", "new": "/pool", "pat": "/mnt/old/%"}}
  ],
  "db": "main"
}'
```

A `SELECT changes() AS changes` is appended after each statement, so the
response reports per-statement `changes` plus `total_changes`.

### `GET /v1/settings/{guid}`, `PATCH /v1/settings/{guid}`

Per-user watch state. The `metadata_item_settings` table is keyed by `guid`
rather than by item id, so these rows survive a re-match or a library rebuild.
URL-encode the guid (`plex://movie/5d77…` → `plex:%2F%2Fmovie%2F5d77…`).

`PATCH` accepts `account_id` (default `1`), `view_count`, `view_offset`,
`last_viewed_at` and `rating`; at least one of the four is required and unknown
fields are rejected. It updates the existing row for that `(guid, account_id)`
or inserts one if there is none — both statements run in the same transaction,
because the table has no `UNIQUE` index to hang `ON CONFLICT` off.

```sh
GUID='plex:%2F%2Fmovie%2F5d7770c8fb0d55001f5f8bd0'
curl -s "$API/v1/settings/$GUID"
curl -s -X PATCH "$API/v1/settings/$GUID" -H 'Content-Type: application/json' \
  -d '{"account_id": 1, "view_count": 1, "view_offset": 0, "last_viewed_at": 1700000000}'
```

## Managed indexes

Plex's own indexes do not cover everything this API asks of the database. The
worst case measured on a 116k-row `metadata_items` is the library listing that
`/v1/tables/metadata_items` serves: Plex has an index on
`(library_section_id, metadata_type, added_at)`, so the section and type are
found quickly, but the `ORDER BY title_sort COLLATE icu_root` then has to be
satisfied with a temporary B-tree. **7.7 ms** per page, all of it sorting.

`plex-api` can optionally add two indexes of its own:

| Index | On | For |
| --- | --- | --- |
| `zz_plexapi_section_type_title` | `metadata_items(library_section_id, metadata_type, title_sort COLLATE icu_root)` | sorted library listings — 7.7 ms → **0.2 ms**, and the temp B-tree disappears from the query plan |
| `zz_plexapi_type_added` | `metadata_items(metadata_type, added_at)` | "recently added" across sections |

The `COLLATE icu_root` is not decoration: an index is only usable for an
`ORDER BY` whose collation it matches, and `icu_root` exists only in the Plex
engine — which is the engine this service drives, so it can build the index
and Plex can use it.

Every name carries the **`zz_plexapi_` prefix**, so a Plex migration adding an
index of its own can never collide with one of ours.

### Turning it on

Set `PLEX_API_INDEXES=true`. In the container this is all you do: an s6 oneshot
runs `plex-api -ensure-indexes` before Plex starts (see `deploy/README.md`).
Outside the container, run it yourself with Plex stopped:

```sh
PLEX_API_INDEXES=true ./plex-api -ensure-indexes
```

It refuses to run if anything is listening on `PLEX_API_PMS_ADDR`.

The first run takes an online backup, creates the indexes, runs `ANALYZE` so
the planner has statistics for them, verifies with `PRAGMA quick_check(1)`, and
records what it did in `<PLEX_API_STATE_DIR>/indexes.json`. On a 966 MB / 116k
row library that is about **6 s** in total: 0.55 s backup, 1.65 s build plus
`ANALYZE`, 3.8 s `quick_check`.

Every later start is a presence check — one read round trip — and costs about
**30 ms** end to end, process start included.

The backup is taken with the shell's `.backup` dot-command, i.e. SQLite's
online backup API, so it is a consistent copy including whatever is still in
the WAL. Only `com.plexapp.plugins.library.db` is backed up, because it is the
only database that gets indexes. Backups land in one timestamped directory each
under `PLEX_API_BACKUP_DIR` and are pruned to the newest `PLEX_API_BACKUP_KEEP`
(default 3). Set `PLEX_API_INDEX_BACKUP=false` to skip it — an index is
recreatable, so this is reasonable if you have your own backups and are short
on disk.

### The risk, and what is done about it

**SQLite refuses `ALTER TABLE ... DROP COLUMN` on a column that any index
mentions.** If a future Plex release migrates `metadata_items` by dropping
`title_sort` or `added_at`, that migration would fail while our index exists —
and a Plex that cannot migrate its database is a Plex that will not start.

So the ensure step watches the PMS version. When the version recorded in
`indexes.json` differs from the version the installed `Plex Media Server`
binary reports, it **drops every managed index and deletes the state file**
before Plex starts, logs why, and exits. Plex then migrates a schema that looks
exactly like a stock one, and the next container start rebuilds the indexes
against the new schema. The cost of a Plex upgrade is therefore one extra
6-second index build, and the failure mode is "slower listings for one start",
not "Plex will not boot".

Nothing else in the database is modified. The indexes are pure derived data:
dropping them is always safe and always sufficient.

### Removing them

```sh
PLEX_API_INDEXES=false ./plex-api -drop-indexes    # Plex stopped
```

`-drop-indexes` removes every `zz_plexapi_` index and the state file. Setting
`PLEX_API_INDEXES=false` on its own deliberately does **not** remove anything —
unsetting a variable should not silently rewrite your database — but it does
make `-ensure-indexes` log that the indexes are still there and how to get rid
of them.

## Safety notes

**Writes are off by default.** Set `PLEX_API_WRITE=true` to enable
`/v1/exec` and `PATCH /v1/settings`. Without it they return 403
`writes_disabled`. `/v1/query` is never affected: it is read-only at the engine
level.

**PMS should be stopped for writes.** By default a write is refused with 409
`pms_running` when something answers on `PLEX_API_PMS_ADDR`. The risk is
semantic, not structural — WAL, `.timeout` and `BEGIN IMMEDIATE` make
concurrent writing mechanically safe, but PMS caches state in memory and is
never told about external edits. Watch state and settings are the tables PMS
re-reads on demand and the usual candidates for
`PLEX_API_WRITE_WHILE_RUNNING=true`; `metadata_items`, `tags` and `media_*`
edits are not.

**Reads while PMS runs are safe.** WAL allows any number of readers alongside
PMS's writer. The databases are never opened with `immutable=1`, which would
disable locking and can return torn pages from a live file.

**FTS title format.** PMS populates `fts4_metadata_titles_icu` with the title
plus a trailing space, and for episodes it appends the show title
(`"Tape 1, Side A 13 Reasons Why"`). The FTS triggers copy columns verbatim, so
a title changed through this API — even through the Plex engine — produces a
different indexed row than PMS would have written, and searching that episode
by show name stops working until PMS refreshes the item. `/v1/search` returns
the indexed text as `fts_title` so the difference is visible. If you change
titles in bulk, ask PMS to refresh the affected items afterwards.

**Prefer PMS's own HTTP API where it can do the job**, because then PMS keeps
its cache and the FTS index correct itself: `PUT /library/metadata/{id}` for
field edits, `/:/scrobble` for watched state, collections, labels, artwork,
refresh and rescan. This service earns its place for what that API cannot do —
bulk or cross-library edits in one transaction, rewriting `media_parts.file`
paths, repairing GUIDs, importing watch history at scale, cleaning dangling
rows, and reading analysis blobs.

**Schema drift.** A PMS upgrade adds `schema_migrations` rows and can change
table shapes. `/health` reports `schema_migrations.max_version`; the version
this was developed against is `500000000001.231` (PMS 1.43.4, SQLite 3.53.3).

**Authentication.** `PLEX_API_TOKEN` is a single shared bearer token compared
in constant time; there is no TLS and no per-user authorization. Bind to
localhost or put it behind a reverse proxy.

## Tests

```sh
go test ./...
```

Unit tests (parameter literal quoting, stderr parsing, script building, result
framing) always run. The integration tests are skipped unless both variables
are set:

```sh
PLEX_API_TEST_PMS_DIR=/usr/lib/plexmediaserver \
PLEX_API_TEST_DB=/path/to/com.plexapp.plugins.library.db \
  go test ./... -v
```

The read tests never write to `PLEX_API_TEST_DB`. The write tests and the
managed-index tests copy it to a temporary directory first and exercise `/v1/exec` rollback, `PATCH
/v1/settings` (update and insert) and a `metadata_items.title` change, which is
the write stock SQLite cannot perform.
