#!/usr/bin/env python3
"""Latency/throughput comparison: official PMS HTTP API vs plex-api, same host."""
import json, os, statistics, sys, time, urllib.request
from concurrent.futures import ThreadPoolExecutor

PMS = os.environ.get("PMS_URL", "http://localhost:32400")
API = os.environ.get("PLEX_API_URL", "http://localhost:32500")
TOKEN = os.environ.get("PLEX_API_TOKEN", "")
N = 30
CONC = 8

def get(url, headers=None, body=None):
    req = urllib.request.Request(url, data=body, headers=headers or {}, method="POST" if body else "GET")
    t0 = time.perf_counter()
    with urllib.request.urlopen(req, timeout=60) as r:
        data = r.read()
    return (time.perf_counter() - t0) * 1000, len(data)

PMS_H = {"Accept": "application/json"}
API_H = {"Authorization": "Bearer " + TOKEN, "Content-Type": "application/json"}

def api_query(sql):
    return lambda: get(API + "/v1/query", API_H, json.dumps({"sql": sql}).encode())

ITEM = 29196
cases = [
    ("item by id",
     lambda: get(f"{PMS}/library/metadata/{ITEM}", PMS_H),
     lambda: get(f"{API}/v1/items/{ITEM}", API_H)),
    ("search 'star'",
     lambda: get(f"{PMS}/library/search?query=star&limit=50", PMS_H),
     lambda: get(f"{API}/v1/search?q=star&limit=50", API_H)),
    ("100 movies, sorted",
     lambda: get(f"{PMS}/library/sections/5/all?X-Plex-Container-Start=0&X-Plex-Container-Size=100", PMS_H),
     api_query("select id,title,year,guid from metadata_items where library_section_id=5 and metadata_type=1 "
               "order by title_sort collate icu_root limit 100")),
    ("count of movies",
     lambda: get(f"{PMS}/library/sections/5/all?X-Plex-Container-Start=0&X-Plex-Container-Size=0", PMS_H),
     api_query("select count(*) n from metadata_items where library_section_id=5 and metadata_type=1")),
    ("recently added (20)",
     lambda: get(f"{PMS}/library/recentlyAdded?X-Plex-Container-Size=20", PMS_H),
     lambda: get(f"{API}/v1/recent?types=1,2&limit=20", API_H)),
    ("watch history (100)",
     lambda: get(f"{PMS}/status/sessions/history/all?X-Plex-Container-Size=100", PMS_H),
     api_query("select id,account_id,guid,title,viewed_at from metadata_item_views order by viewed_at desc limit 100")),
]

def run(fn, n):
    for _ in range(3):
        fn()
    lat, size = [], 0
    for _ in range(n):
        ms, sz = fn(); lat.append(ms); size = sz
    return lat, size

def stats(lat):
    lat = sorted(lat)
    return statistics.median(lat), lat[int(len(lat) * 0.95) - 1], min(lat)

print(f"Sequential, {N} requests each after 3 warm-ups. Latency in ms (median / p95 / min), payload bytes.\n")
print(f"{'case':22} {'official med':>13} {'p95':>7} {'min':>7} {'bytes':>9} | {'plex-api med':>13} {'p95':>7} {'min':>7} {'bytes':>9}")
for name, off, new in cases:
    try:
        lo, so = run(off, N); mo, po, no = stats(lo)
    except Exception as e:
        mo = po = no = float("nan"); so = -1; print(f"official {name} failed: {e}", file=sys.stderr)
    ln, sn = run(new, N); mn, pn, nn = stats(ln)
    print(f"{name:22} {mo:13.1f} {po:7.1f} {no:7.1f} {so:9d} | {mn:13.1f} {pn:7.1f} {nn:7.1f} {sn:9d}")

print(f"\nConcurrent: {CONC} workers, {N*2} requests total, item-by-id and search. Requests/second.")
for name, off, new in cases[:2]:
    for label, fn in (("official", off), ("plex-api", new)):
        with ThreadPoolExecutor(CONC) as ex:
            t0 = time.perf_counter()
            lats = list(ex.map(lambda _: fn()[0], range(N * 2)))
            wall = time.perf_counter() - t0
        print(f"{name:22} {label:9} {N*2/wall:7.1f} req/s   median {statistics.median(lats):6.1f} ms  p95 {sorted(lats)[int(len(lats)*0.95)-1]:6.1f} ms")
