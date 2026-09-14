# Performance review of plex-api 0.1.0 (pooled build)

Reviewed 2026-09-12 after the read pool landed (commit 2f1db86). Measurements are on the
development machine against the real 966 MB library, pool of 4, warm.

## Where the time goes now

| Request | plex-api | engine alone | overhead |
| --- | ---: | ---: | ---: |
| `/v1/items/29196` | 2.8 ms | under 1 ms | ~2 ms |
| `/v1/search?q=star&limit=50` | 2.0 ms | under 1 ms | ~1 ms |
| `/v1/tables/metadata_items?limit=100` | 10.7 ms | ~2 ms | ~9 ms |
| `/v1/tables/metadata_items?limit=1000` | 88.8 ms | 18.5 ms | ~70 ms |
| `/v1/tables/media_streams?limit=1000` | 27.0 ms | | |

Small responses are already at the floor. Large pages spend most of their time in Go, not
in SQLite: the shell's JSON is decoded token by token into `map[string]any`, then
re-encoded by `encoding/json`, which also sorts map keys and so loses column order.

## Findings, ranked by payoff

1. **Pass row JSON through untouched.** The shell already emits valid JSON arrays. Keep
   each statement's output as `json.RawMessage` and splice it into the response instead
   of decode plus encode. Removes roughly 70 of the 89 ms on a 1000-row page, preserves
   the engine's column order, and cuts allocations for every endpoint. Decoding is still
   needed where the handler reads values (item existence check, `changes()`, health,
   schema lookups); do it only there. `internal/sqlite/driver.go` `decodeSegment`,
   `internal/api/*` handlers.
2. **`/v1/tables` makes three pool round trips per request** (`sqlite_master` lookup,
   `PRAGMA table_info`, then the page). Send all three in one script, and cache
   `table_info` per database and table, invalidated when `PRAGMA schema_version`
   changes. Saves 2 round trips and two PRAGMA executions per request; on the spawn
   path (pool disabled) it is the difference between one and three process starts,
   which is why this endpoint measured 50 ms on the deployed server against 33 for the
   others. `internal/api/tables.go`, `schema.go`.
3. **Do blob base64 in SQL.** The Plex shell has the CLI `base64()` function (verified:
   `select base64(x'00ff10')`). Select `replace(base64(col), char(10), '')` instead of
   `hex()` and drop the Go hex-to-base64 pass over every row. Prerequisite for finding 1
   on tables with blob columns. `internal/api/tables.go`, `values.go`.
4. **Pre-warm the pool and recycle asynchronously.** Shells are spawned lazily, so the
   first requests after a restart, and one request in every 1000 at recycle time, pay the
   16 ms start. Spawn the configured shells at startup in the background, and when a
   shell hits `PoolMaxUses`, start its replacement before discarding it.
   `internal/sqlite/pool.go` `acquire`.
5. **Pooled output is copied twice.** `Pool.run` joins lines into one string, and
   `splitResults` splits it again on the sentinel. Splitting on sentinel lines as they
   arrive from the reader channel avoids the second pass. Small, but on the path of
   every read. `internal/sqlite/pool.go`.
6. **Stop counting rows twice on `/v1/items`.** Seven statements are fine, but the
   `media_parts` and `media_streams` queries each re-walk `media_items`; selecting the
   media item ids once and using `IN (...)` would be marginally cheaper. Sub-millisecond;
   only worth doing alongside finding 1.

## Things that are fine

- Pool size 4 sustains about 1100 requests per second on this machine; raising it only
  helps if requests are long. Each shell is a full PMS process in shell mode.
- The write path spawning a private process per request is the right trade; writes are
  rare and isolation matters.
- HTTP keep-alive, header timeouts and shutdown handling are standard and correct.
- `pmsRunning` dials 127.0.0.1 with a 500 ms cap; on a closed port it fails instantly.
- Compression would shrink the 1.7 MB 1000-row page, but on a LAN the transfer is not
  where the time goes.

## Outcome (implemented the same day)

Findings 1 to 5 were implemented: raw JSON passthrough with a spliced envelope
(`writeFields`), a fast single-array path and line-count row counting in the driver, one
round trip plus a cached, schema-version-checked table description for `/v1/tables`,
`base64()` in SQL for blob columns, pool warm-up at startup with asynchronous recycling,
and sentinel splitting as lines arrive from a pooled shell.

| Request | before | after | engine alone |
| --- | ---: | ---: | ---: |
| `/v1/tables/metadata_items?limit=1000` | 88.8 ms | 22.7 ms | 18.5 ms |
| `/v1/tables/metadata_items?limit=100` | 10.7 ms | 3.1 ms | ~2 ms |
| `/v1/tables/media_streams?limit=1000` | 27.0 ms | 6.6 ms | |
| `/v1/tables/blobs?db=blobs&limit=50` (3.8 MB) | 91.3 ms | 34.9 ms | |
| `/v1/items/29196` | 2.8 ms | 1.7 ms | under 1 ms |
| `/v1/search?q=star&limit=50` | 2.0 ms | 1.3 ms | under 1 ms |

Column order in responses now matches the SELECT, since rows are no longer rebuilt from
maps. Finding 6 (the item queries) was left alone; it is below the measurement noise.
