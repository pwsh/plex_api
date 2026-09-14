# Deploying `plex-api` inside the Plex container

`plex-api` is a small Go HTTP service that exposes the Plex library SQLite
database. It does not link a SQLite library of its own: it drives
`/usr/lib/plexmediaserver/Plex SQLite` as a subprocess.

## Why this runs *inside* the Plex container, not as a sidecar

The Plex library database cannot be safely opened by stock SQLite. Plex ships
its own build with a custom collating sequence and tokenizer that live only in
the PMS install (the `Plex SQLite` wrapper plus `/usr/lib/plexmediaserver/lib`).
Open `metadata_items` or the FTS tables with an ordinary `sqlite3` and you get
`no such collation sequence` errors, or — worse — writes that silently produce
an index a real PMS will reject.

So the engine has to come from a PMS install. A sidecar container would have to
carry its own copy of Plex Media Server (a ~400 MB duplicate that must be kept
version-matched with the real one) *and* bind-mount the live database across a
container boundary. Running in the same container as PMS gives us the exact
matching engine, the same filesystem, the same uid, and the ability to check
whether PMS is actually running before allowing a write.

## Two ways to install it

### 1. As a linuxserver "docker mod" (preferred, once published)

linuxserver.io images support `DOCKER_MODS`: at startup the container downloads
the named image's layers and untars them onto `/`. That is the idiomatic way to
add a service to an lsio image without rebuilding it — you keep tracking
`lscr.io/linuxserver/plex:latest` and get Plex updates for free.

```yaml
services:
  plex:
    image: lscr.io/linuxserver/plex:latest
    environment:
      - PUID=1000
      - PGID=1000
      - TZ=America/Chicago
      - VERSION=docker
      - DOCKER_MODS=ghcr.io/<owner>/plex-api-mod:latest
      - PLEX_API_TOKEN=change-me
    volumes:
      - ./plex-config:/config
    ports:
      - "32400:32400"
      - "127.0.0.1:32500:32500"
```

Build and publish the mod:

```sh
make mod                      # -> plex-api-mod:dev
docker tag plex-api-mod:dev ghcr.io/<owner>/plex-api-mod:latest
docker push ghcr.io/<owner>/plex-api-mod:latest
```

**The mod image must be anonymously pullable and must be a single layer.**
The container fetches it without credentials, and the loader (script
3.20250825) installs only the *first* layer of the image, which is why
`Dockerfile.mod` assembles the whole overlay in the build stage and copies it
in one `COPY`.

Verified 2026-09-12: a self-hosted Forgejo/Gitea container registry works as a
mod source. `DOCKER_MODS=registry.example.com/<user>/plex-api-mod:dev` downloads
and installs in a stock `lscr.io/linuxserver/plex:latest`, because the loader
follows the registry's `Www-Authenticate` header to an anonymous token, and
Forgejo package visibility follows the owning user's profile rather than the
source repository. Publish with:

```sh
docker login registry.example.com -u <user>
docker tag plex-api-mod:dev registry.example.com/<user>/plex-api-mod:dev
docker push registry.example.com/<user>/plex-api-mod:dev
```

Note that the package must be anonymously pullable, which makes the compiled
mod (not the source) publicly downloadable. If that is not acceptable, use the
layer image below from a private registry.

### 2. As a derived image ("layer") — what we use for private development

```sh
make layer                    # -> plex-api-layer:dev
docker compose -f deploy/docker-compose.yml up
```

`deploy/Dockerfile.layer` is `FROM lscr.io/linuxserver/plex:latest` and copies
exactly the same two things as the mod: `deploy/root/` onto `/`, and the binary
to `/usr/local/bin/plex-api`. The tradeoff is that you must rebuild to pick up
a new Plex release.

## What gets installed

```
/usr/local/bin/plex-api
/etc/s6-overlay/s6-rc.d/svc-plex-api/type                      -> longrun
/etc/s6-overlay/s6-rc.d/svc-plex-api/run                       -> the service
/etc/s6-overlay/s6-rc.d/svc-plex-api/notification-fd           -> 3
/etc/s6-overlay/s6-rc.d/svc-plex-api/dependencies.d/init-services
/etc/s6-overlay/s6-rc.d/user/contents.d/svc-plex-api           -> registers it

/etc/s6-overlay/s6-rc.d/init-plex-api-indexes/type             -> oneshot
/etc/s6-overlay/s6-rc.d/init-plex-api-indexes/up               -> path of run
/etc/s6-overlay/s6-rc.d/init-plex-api-indexes/run              -> the index step
/etc/s6-overlay/s6-rc.d/init-plex-api-indexes/dependencies.d/init-services
/etc/s6-overlay/s6-rc.d/user/contents.d/init-plex-api-indexes  -> registers it
/etc/s6-overlay/s6-rc.d/svc-plex/dependencies.d/init-plex-api-indexes
```

The run script mirrors the shipped `svc-plex`: `#!/usr/bin/with-contenv bash`,
`exec s6-setuidgid abc /usr/local/bin/plex-api` (skipping `s6-setuidgid` when
`LSIO_NON_ROOT_USER` is set), wrapped in `s6-notifyoncheck -c "nc -z localhost
<port>"` so s6 knows when the API is actually listening. The probe port is read
from `PLEX_API_BIND`, defaulting to 32500.

### Service dependency: `init-services` only, deliberately *not* `svc-plex`

`svc-plex-api` depends on `init-services`, the same dependency `svc-plex` has.
It does **not** depend on `svc-plex`, for two reasons:

* The API has to keep working while Plex Media Server is stopped. Stopping PMS
  is precisely the supported way to do maintenance writes
  (`PLEX_API_WRITE_WHILE_RUNNING=false`), and an API that dies with PMS could
  not be used for it.
* A PMS crash-loop (bad database, failed claim, unsupported hardware) would
  otherwise take the API down too — exactly when you most want to query the
  database to find out what is wrong.

The cost is that the API may start before the database file exists on a brand
new `/config`. The service reports that through `GET /health` instead of
refusing to start.

## The managed-index oneshot: `init-plex-api-indexes`

`PLEX_API_INDEXES=true` adds two extra indexes to the library database (the
what and the why are in the root `README.md`). Building them is a write, and a
write wants Plex not to be touching the database.

The obvious design is "stop Plex, create the indexes, start Plex again". This
does something better: it runs **before Plex has ever started**.

```
init-services  ──▶  init-plex-api-indexes  ──▶  svc-plex
                                           └─▶  (svc-plex-api starts in parallel)
```

`init-plex-api-indexes` is a oneshot that depends on `init-services`, and the
mod drops an empty file at
`/etc/s6-overlay/s6-rc.d/svc-plex/dependencies.d/init-plex-api-indexes`, which
makes **Plex wait for it**. s6-rc will not bring `svc-plex` up until the
oneshot has returned.

Depending on `init-services` also puts the step in the right place relative to
Plex's own init chain. In `lscr.io/linuxserver/plex:latest` that chain is
`init-config` → `init-plex-chown` → `init-plex-claim` → `init-plex-update` →
… → `init-mods` → `init-custom-files` → `init-services`, so by the time our
oneshot runs, `/config` has been chowned to `abc`, the mod itself has been
unpacked, **and any Plex binary update has already been installed** — which is
what makes the PMS version we read the version Plex is about to run.

Why this and not stop/start:

* There is nothing to stop. On a fresh container start PMS has not opened the
  database yet, so there is no lock contention, no busy timeout, no WAL
  checkpoint racing a writer, and no chance of PMS caching state that our DDL
  invalidates.
* **PMS shutdown was observed to hang.** `s6-rc -t 60000 -d change svc-plex`
  can time out with the service still `up ... want down` (see
  `docs/Unraid.md`), and the only way out is `s6-svc -k`, a SIGKILL. Building
  an index is not worth a forced kill of a media server.
* It is one place in the boot order instead of a stop/start dance that has to
  work identically on every restart.

The ordering also gives the version-change safeguard its teeth: when a Plex
upgrade is detected, the indexes are dropped *before* Plex runs its schema
migrations, so the migrations only ever see a stock-looking schema.

### The run script

`run` is `#!/usr/bin/with-contenv bash` and **always exits 0**. Plex starting is
more important than our indexes; a failure is logged as

```text
[plex-api] index step failed (see above); Plex will start anyway
```

and the boot continues. The Go binary itself exits 1 on failure and does not
write the state file, so the next start retries.

The cheapest case is short-circuited in shell: when `PLEX_API_INDEXES` is not
true **and** no state file exists, the script prints one line and exits without
starting the binary at all. Otherwise it runs

```sh
s6-setuidgid abc /usr/local/bin/plex-api -ensure-indexes
```

— `abc` is the PUID/PGID user that owns the databases, the same user
`svc-plex-api` and Plex itself run as, so the state file, the backups and the
indexes are all owned by it and nothing in `/config` ends up root-owned. When
`LSIO_NON_ROOT_USER` is set the container already runs as that user and
`s6-setuidgid` is skipped, exactly as in `svc-plex-api/run`. The script creates
`PLEX_API_STATE_DIR` and chowns it to `abc` first, because it runs as root and
`/config/plex-api` does not exist on a fresh install.

### State and backups inside the container

| Path | What |
| --- | --- |
| `/config/plex-api/indexes.json` | what was created, and the PMS version it was created against |
| `/config/plex-api/backups/<YYYYmmdd-HHMMSS>/com.plexapp.plugins.library.db` | online backup taken before a create |

Both live under `/config`, so they land in your appdata volume and survive
container recreation. Backups are pruned to the newest `PLEX_API_BACKUP_KEEP`
(default 3) — with a 1 GB library that is about 3 GB of appdata, so lower it to
1, or set `PLEX_API_INDEX_BACKUP=false`, if space is tight. Only the main
library database is copied; the blobs database never gets an index and is never
touched.

Timings on a 966 MB / 116k-row library: first start adds about **6 s** to boot
(0.55 s backup, 1.65 s build + `ANALYZE`, 3.8 s `quick_check`); every later
start adds about **30 ms**.

## Environment variables

All are optional; defaults are what a stock linuxserver/plex container needs.

| Variable | Default | Meaning |
| --- | --- | --- |
| `PLEX_API_BIND` | `0.0.0.0:32500` | listen address inside the container |
| `PLEX_API_PMS_DIR` | `/usr/lib/plexmediaserver` | where `Plex SQLite` lives |
| `PLEX_API_DB_DIR` | `/config/Library/Application Support/Plex Media Server/Plug-in Support/Databases` | database directory |
| `PLEX_API_TOKEN` | *(unset)* | bearer token required on every endpoint except `/health` |
| `PLEX_API_WRITE` | `false` | allow write statements at all |
| `PLEX_API_WRITE_WHILE_RUNNING` | `false` | allow writes while PMS is up |
| `PLEX_API_PMS_ADDR` | `127.0.0.1:32400` | used to detect whether PMS is running |
| `PLEX_API_TIMEOUT_MS` | *(service default)* | overall request timeout |
| `PLEX_API_QUERY_TIMEOUT` | *(service default)* | per-query timeout |
| `PLEX_API_INDEXES` | `false` | create the managed indexes before Plex starts |
| `PLEX_API_INDEX_BACKUP` | `true` | online backup of the library database before creating them |
| `PLEX_API_STATE_DIR` | `/config/plex-api` | holds `indexes.json` |
| `PLEX_API_BACKUP_DIR` | `/config/plex-api/backups` | timestamped backup directories |
| `PLEX_API_BACKUP_KEEP` | `3` | how many backups to keep |

Port: **32500** (TCP). `GET /health` is unauthenticated; everything else
requires the token when one is set.

## Security notes

The API is *exactly as privileged as the database file*. Anything that can
reach port 32500 can read your entire library — every title, every file path on
disk, every account row and, with writes enabled, can corrupt the library
beyond what PMS can repair.

* **Publish it on loopback only** (`-p 127.0.0.1:32500:32500`) or keep it on an
  internal docker network. Never map it to `0.0.0.0` on a host with a public
  interface.
* **Set `PLEX_API_TOKEN`** whenever anything other than localhost can reach the
  port. There is no TLS and no user model — put it behind a reverse proxy if
  you need either.
* **Leave `PLEX_API_WRITE=false`** unless you are actively doing maintenance,
  and leave `PLEX_API_WRITE_WHILE_RUNNING=false` essentially always: PMS holds
  the database open, and a concurrent writer can produce an inconsistent FTS
  index that `integrity_check` will flag on PMS 1.43+.
* **Back up before any write.** Stop PMS, copy both
  `com.plexapp.plugins.library.db` and `.blobs.db`, then write.
* The service runs as `abc` (PUID/PGID), the same uid as PMS — it has no more
  filesystem access than Plex itself, but no less either.

## Testing locally

```sh
make layer
./deploy/test-container.sh          # full smoke test against the real DBs
./deploy/test-container.sh --keep   # leave it running to poke at by hand
```

`test-container.sh` reflink-copies the repaired databases from
`$SRC_DB_DIR` into a scratch `/config`, starts the layer
image with writes enabled and token `test`, waits for `/health`, and exercises
`/v1/search`, `/v1/tables/metadata_items` and `/v1/items/<id>`. It never writes
to the originals, and it does not require PMS inside the container to have
finished starting.
