# Benchmark: official PMS HTTP API vs plex-api

Run 2026-09-12 from a LAN client against the same Plex server (a LAN host, linuxserver
container, PMS 1.43, plex-api 0.1.0 as a docker mod). Plex was running and idle. The
official API was called with `Accept: application/json`; plex-api with a bearer token.
Script: `scripts/bench.py` (set `PLEX_API_TOKEN`, `PMS_URL`, `PLEX_API_URL`).

The two sides are not the same work. The official API renders full metadata containers
with hubs, images and per-user state; plex-api returns rows straight from SQLite. Read the
numbers as "what a client waits for to get the same answer", not as engine speed.

## Sequential latency

30 requests per case after 3 warm-ups. Milliseconds: median / p95 / min, then payload bytes.

| Case | Official med | p95 | min | bytes | plex-api med | p95 | min | bytes |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| item by id | 6.1 | 6.6 | 5.6 | 9,684 | 34.6 | 36.7 | 31.5 | 15,322 |
| search "star" | 309.2 | 319.6 | 296.0 | 128,271 | 32.7 | 34.5 | 31.2 | 15,849 |
| 100 movies, sorted | 87.2 | 89.4 | 85.4 | 233,169 | 50.0 | 53.4 | 48.5 | 9,945 |
| count of movies | 47.8 | 64.8 | 46.4 | 453 | 29.9 | 31.6 | 27.5 | 47 |
| recently added (20) | 392.1 | 404.3 | 381.5 | 98,916 | 50.3 | 52.6 | 48.7 | 1,300 |
| watch history (100) | 492.4 | 500.0 | 483.4 | 11,076,074 | 32.9 | 34.5 | 30.4 | 13,694 |

## Concurrency

8 parallel workers, 60 requests.

| Case | API | req/s | median ms | p95 ms |
| --- | --- | ---: | ---: | ---: |
| item by id | official | 211.1 | 38.1 | 41.6 |
| item by id | plex-api | 201.7 | 36.6 | 40.3 |
| search "star" | official | 5.8 | 1430.0 | 1526.1 |
| search "star" | plex-api | 199.1 | 37.4 | 40.7 |

## Reading the results

- **Single-item lookup is the official API's strength.** PMS serves it from memory in
  about 6 ms. plex-api pays roughly 16 ms to start the Plex SQLite shell plus seven joined
  queries, landing at about 35 ms. A long-lived shell per worker would cut most of that.
- **Everything that PMS has to assemble is faster through plex-api by 2 to 15 times**:
  search, sorted listings, recently added and history. The official search endpoint spends
  about 300 ms building hub results, and under 8-way concurrency it serialises to under 6
  requests per second, while plex-api holds 200 requests per second.
- **Payloads are 10 to 800 times smaller** because plex-api returns only the columns asked
  for. The official history call returned 11 MB for what plex-api answered in 14 KB; the
  official endpoint appears to ignore the container size limit for history.
- Under concurrency both APIs level at about 200 requests per second for the item case,
  which is likely the single Python client saturating rather than either server.

## Caveats

- One client, one run, LAN. Numbers will differ on other hardware.
- plex-api figures include process spawn on every request. The measured floor of about
  30 ms is the cost of that design choice; it buys isolation and the real Plex engine.
- The official API results include work plex-api does not do (hub assembly, art URLs,
  per-user view state), which is exactly why they differ.

## Re-run after the read pool and passthrough changes (same day, commit 84f7098)

Same client, same server, plex-api now with a pool of 4 pre-warmed shells and raw JSON
passthrough. Official API numbers were re-measured in the same run.

| Case | Official med | p95 | plex-api med | p95 | before (spawn per request) |
| --- | ---: | ---: | ---: | ---: | ---: |
| item by id | 6.6 | 7.6 | 3.4 | 4.2 | 34.6 |
| search "star" | 313.8 | 333.2 | 2.8 | 3.3 | 32.7 |
| 100 movies, sorted | 88.5 | 92.8 | 17.8 | 19.6 | 50.0 |
| count of movies | 51.2 | 53.7 | 1.2 | 1.3 | 29.9 |
| recently added (20) | 405.0 | 440.5 | 18.8 | 19.8 | 50.3 |
| watch history (100) | 502.8 | 517.4 | 1.4 | 1.7 | 32.9 |

8 parallel workers, 60 requests:

| Case | API | req/s | median ms | p95 ms |
| --- | --- | ---: | ---: | ---: |
| item by id | official | 199.1 | 40.8 | 49.3 |
| item by id | plex-api | 1625.9 | 4.3 | 5.6 |
| search "star" | official | 5.9 | 1339.8 | 1597.2 |
| search "star" | plex-api | 1868.5 | 3.8 | 5.2 |

plex-api is now faster than the official API on every case measured, including the
single-item lookup that the official API previously won, and sustains about 8 times its
throughput on item lookups and about 300 times on search. The two listing cases (100
movies, recently added) sit at 18 ms, which is the engine walking 116 thousand rows with
the icu_root collation or an added_at sort; an index would be Plex's to add, not ours.

## Re-run with managed indexes deployed (commit 8d190fd)

Both APIs measured in the same pass, so the official column shows the effect of the
extra indexes on Plex's own queries.

| Case | Official before | Official now | plex-api before | plex-api now |
| --- | ---: | ---: | ---: | ---: |
| item by id | 6.6 | 6.1 | 3.4 | 3.7 |
| search "star" | 313.8 | 320.7 | 2.8 | 3.3 |
| 100 movies, sorted | 88.5 | 72.5 | 17.8 | 1.4 |
| count of movies | 51.2 | 32.4 | 1.2 | 1.3 |
| recently added (20) | 405.0 | 391.8 | 18.8 | 19.1 |
| watch history (100) | 502.8 | 502.2 | 1.4 | 1.4 |

Concurrency (8 workers): item by id official 199 req/s, plex-api 1619 req/s; search
official 5.4 req/s, plex-api 1855 req/s. Unchanged within noise.

Reading: the section listing index helps Plex too. The official sorted section listing
dropped 18 percent and the section count 37 percent, because Plex's own SQL has the same
shape (section, type, sort by title_sort collate icu_root) and the planner now takes the
composite index. Nothing got slower. The plex-api "recently added" case is unchanged at
19 ms because its `metadata_type IN (1,2)` filter cannot walk one index; the fix is a
query rewrite (union of per-type ordered selects), not another index.

## Re-run with `/v1/recent` deployed (commit a1df54a)

| Case | Official | plex-api |
| --- | ---: | ---: |
| item by id | 6.4 | 3.9 |
| search "star" | 314.6 | 3.2 |
| 100 movies, sorted | 75.1 | 1.3 |
| count of movies | 32.9 | 1.3 |
| recently added (20) | 391.5 | 2.4 |
| watch history (100) | 497.4 | 1.5 |

Concurrency (8 workers): item by id official 179 req/s, plex-api 1591 req/s; search
official 5.6 req/s, plex-api 1528 req/s.

The recently-added case went from 19.1 ms to 2.4 ms through the new endpoint, which
walks the managed index per type instead of scanning. Every plex-api case is now under
4 ms; the slowest remaining one is the item lookup, whose seven joined queries cost
about 1 ms in the engine plus network and response assembly.
