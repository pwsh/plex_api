# Direct access to the Plex library database: feasibility review

Written 2026-09-12. Evidence comes from hands-on tests against the repaired PMS 1.43 library
pair in `~/Documents/plexdb/post`, the PMS 1.43.4.10903 Linux package (downloaded and
unpacked, not installed), stock SQLite 3.46.1 / Python's sqlite3 3.45.1, and web research on
Plex forum threads and the SQLite-over-HTTP tool landscape. See
`PlexDatabaseLayout.md` for the schema itself.

## Verdict

**Feasible, with one hard constraint.** Full read *and* write access to the library database
needs Plex's own SQLite engine, and that engine only exists inside the `Plex Media Server`
executable. Stock SQLite can read almost everything and write to a subset of tables, but it
cannot insert, delete, or retitle `metadata_items`, cannot touch `tags` at all, and cannot run
a full-text search.

The recommended shape is an **HTTP API service that drives the bundled `Plex SQLite` shell
as its backend**. The shell speaks JSON, binds parameters, honours busy timeouts, starts in
about 16 ms, and is the exact engine PMS uses, so every trigger, collation and FTS table
behaves as it does under PMS. Clients then need nothing but HTTP.

## 1. Where the custom pieces live

| Component | Location | Evidence |
| --- | --- | --- |
| `Plex SQLite` | 11 KB stub, `/usr/lib/plexmediaserver/Plex SQLite` | Has only `execv`/`readlink` imports and the string `Plex Media Server`; it re-executes the server binary in SQLite-shell mode. |
| SQLite 3.53.3 engine | `lib/libsqlite3.so` (1 MB) | Custom build with `SQLITE_ENABLE_ICU`; links Plex's private ICU 69 (`libicu*plex.so.69`). Exports only the standard `sqlite3_*` API. |
| `collating` FTS4 tokenizer | `Plex Media Server` binary | The strings `collating`, `icu_root` and the FTS `CREATE VIRTUAL TABLE` statements appear only in the server binary, not in `libsqlite3.so`. |
| `icu_root` collation | `Plex Media Server` binary | Same. Used by `index_title_sort_icu` and by PMS's own `ORDER BY title_sort COLLATE icu_root` queries. |

Consequence: preloading Plex's `libsqlite3.so` into another program (tested with Python via
`LD_PRELOAD`) gives SQLite 3.53.3 but still fails with `unknown tokenizer: collating` and
`no such collation sequence: icu_root`. There is no loadable extension to borrow. The only
ways to get the real behaviour are to run the PMS binary as a shell, or to re-implement the
tokenizer and collation.

PMS itself opens the databases with `PRAGMA journal_mode=WAL`, `synchronous=NORMAL`,
`foreign_keys=OFF` and `cache_size=4096` (strings in the binary; the repaired copy is in
`delete` mode only because DBRepair rebuilt it). PMS holds a persistent connection while
running.

## 2. What stock SQLite can and cannot do

Tested on a copy of the 966 MB main database with Python's sqlite3, each statement in a
transaction that was rolled back.

### Reads

| Operation | Result |
| --- | --- |
| Any `SELECT` on ordinary tables and the FTS `_content` shadow tables | works |
| `ORDER BY title_sort` (NOCASE index) | works |
| `ORDER BY title_sort COLLATE icu_root` (what PMS does for library listings) | `no such collation sequence: icu_root` |
| `... WHERE fts4_metadata_titles_icu MATCH 'star'` | `unknown tokenizer: collating` |

### Writes

| Statement | Result | Why |
| --- | --- | --- |
| `UPDATE metadata_items SET summary=…` | works | no trigger, no ICU index |
| `UPDATE metadata_items SET added_at=…` | works | |
| `UPDATE metadata_item_settings SET view_count=…` | works | watch state is trigger-free |
| `INSERT INTO taggings …` | works | |
| `UPDATE media_parts SET file=…` | works | path fixes are possible |
| `UPDATE metadata_items SET title=…` | fails | FTS trigger needs the tokenizer |
| `UPDATE metadata_items SET title_sort=…` | fails | `index_title_sort_icu` needs the collation |
| `INSERT INTO metadata_items …` | fails | same index |
| `DELETE FROM metadata_items …` | fails | same index plus FTS trigger |
| `INSERT INTO tags …` (any `tag_type`, even ones the trigger's `WHEN` excludes) | fails | SQLite compiles the trigger body before evaluating `WHEN` |
| `UPDATE tags SET tag=…` | fails | FTS trigger |

Registering a stand-in `icu_root` collation from Python removes the collation error but the
FTS trigger still fails, and a stand-in collation would corrupt the ordering of the index for
PMS. So stock SQLite is a **read-mostly** engine for this database: watch state, settings,
paths, summaries and taggings are writable; the item and tag tables are not.

## 3. The Plex shell as a backend

All tested with `Plex SQLite` from the 1.43.4 package, `LD_LIBRARY_PATH` pointing at its
`lib` directory, against the real database.

| Capability | Result |
| --- | --- |
| `-json` flag or `.mode json` | JSON array per statement |
| `.parameter set :t 'star wars'` then `MATCH :t` | bound parameters work, so SQL injection is avoidable |
| SQL over stdin, several statements, `.print` separators | works; an error goes to stderr and the shell continues with the next statement (exit code 1 at the end) |
| `.timeout 3000` then `BEGIN IMMEDIATE` while another process holds a write lock (WAL) | waits and completes; integrity check still `ok` |
| `-readonly` | honoured |
| Process start plus one indexed query | about 16 ms |
| Ten statements through one process | about 2 ms each |

Both usage patterns are therefore viable: **spawn per request** (simple, isolated, 16 ms)
or **one long-lived shell per worker** fed over stdin with sentinel `.print` lines to frame
results (faster, needs a small protocol). Neither needs a database driver in the API process.

Note for future schema versions: the 1.43.4 binary also contains
`CREATE VIRTUAL TABLE … USING fts4(content='metadata_items', …)` variants. A later migration
may switch the FTS tables to external-content mode. The engine-backed design is unaffected;
anything that reads the `_content` shadow tables directly would break.

## 4. Coexisting with a running PMS

* **Reads** while PMS runs are safe. WAL allows any number of readers alongside PMS's writer.
  Open read-only and do not use `immutable=1` on a live database (it disables locking and can
  return torn pages).
* **Writes** while PMS runs are mechanically possible (WAL, `busy_timeout`, `BEGIN IMMEDIATE`,
  short transactions) and the lock test above shows the two writers queue correctly. The risk
  is semantic, not structural: PMS keeps state in memory and is not told about external edits.
  Plex staff and the DBRepair README both say to stop PMS before editing; whether PMS caches a
  given table is not documented. A practical policy is to classify writes: tables PMS re-reads
  on demand (watch state, settings) may be edited live at the operator's option, while
  `metadata_items`, `tags`, `media_*` edits default to "PMS must be stopped" with an explicit
  override.
* **FTS content format.** PMS populates `fts4_metadata_titles_icu` with the title plus a
  trailing space, and appends the show title for episodes. The triggers copy columns verbatim,
  so a title edit made through SQL (even with the Plex engine) produces a different FTS row
  than PMS would. Searching that episode by show name then fails until PMS refreshes the item.
  The API should either re-insert the FTS row in PMS's format after a title change or ask PMS
  to refresh the item afterwards.
* **PMS upgrades** add rows to `schema_migrations` and can change table shapes. The API should
  record the migration version it was validated against and refuse or warn on a newer one.
* **File ownership.** The service must run as the `plex` user (or with equivalent group access)
  and must locate the PMS install directory, which `DBRepair.sh` already knows how to do for
  every supported platform.

## 5. What the official HTTP API already covers

Use PMS's own API where it can do the job, because PMS then handles cache and FTS itself:
editing title, summary, ratings and other fields with per-field locks
(`PUT /library/metadata/{id}`), watched state (`/:/scrobble`), collections, labels, posters
and art, refresh and rescan. Direct database access earns its place for what the API cannot
do: bulk or cross-library edits in one transaction, rewriting `media_parts.file` paths,
repairing GUIDs, editing or importing watch history and settings at scale, cleaning dangling
rows (see the integrity review in `PlexDatabaseLayout.md`), and reading analysis blobs.

## 6. Options, ranked

1. **API service backed by the Plex shell (recommended).** A small service (Python, Go, or
   Rust; no SQLite driver needed) that exposes typed endpoints for the common operations and a
   guarded parameterised-SQL endpoint, and executes them by driving `Plex SQLite`. Full
   fidelity, works on every PMS platform DBRepair supports, survives PMS upgrades because the
   engine upgrades with PMS. Costs: a subprocess protocol, and the service must live on the PMS
   host.
2. **API service on stock SQLite.** Datasette, or FastAPI plus Python's sqlite3, opened
   read-write. Zero dependency on the Plex install and usable off-host on a copy, but no FTS
   search, no `icu_root` ordering, and no writes to `metadata_items` or `tags`. Good enough for
   a read API and for watch-state or path tools; wrong for anything editorial.
3. **Re-implement `collating` and `icu_root` as a loadable extension.** ICU 69 word-break
   tokens folded through a root collator at primary strength with shifted alternates. Feasible
   in principle and verifiable (the FTS `integrity-check` command would flag any mismatch
   against Plex-built segments), but it is reverse engineering, must track Plex's ICU version,
   and one wrong byte silently poisons the index. Only worth it if off-host full-fidelity writes
   become a requirement.
4. **Generic SQLite-over-HTTP products** (roapi, sqlite-rest, NocoDB, sqld, rqlite). None can
   load the Plex engine; most are read-only or want to own the file. Datasette is the only one
   worth using, and only as option 2.

## 7. Suggested next step

A one-day spike of option 1: a service with `GET /items/{id}`, `GET /search?q=`,
`POST /sql` (parameterised, read-only by default), and `PATCH /settings/{guid}` for watch
state, all executed through `Plex SQLite` with `.mode json` and `.parameter set`. Measure
per-request latency for both spawn-per-request and long-lived-shell modes, and run the FTS
`integrity-check` after a batch of title edits to decide the FTS repopulation strategy.
