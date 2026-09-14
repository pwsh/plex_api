# Adding plex-api to Plex on Unraid

This guide adds the plex-api service to the linuxserver.io Plex container that Community
Applications installs on Unraid. Nothing on the Unraid OS itself changes. The only edits are
to the Plex container's template: one variable that loads the mod, a few variables that
configure it, and, only if the container is in bridge mode, one port mapping.

Everything below was verified on 2026-09-12 against `lscr.io/linuxserver/plex:latest`
(mod loader script 3.20250825) using a mod image published to a self-hosted registry.
Replace `ghcr.io/pwsh/plex-api-mod:latest` below with the image reference you publish.

## What you get

A second service inside the Plex container, listening on port 32500, that reads and
optionally writes the Plex library database through Plex's own SQLite engine. Clients talk
plain HTTP with JSON. The full endpoint list is in the root `README.md`.

## Why a mod and not a separate container

The tokenizer and collation Plex uses for search and sorting exist only inside the
`Plex Media Server` binary. A separate container cannot get them, so plex-api has to run
where the Plex install is. linuxserver containers support exactly this through "docker
mods": a tiny image whose files are downloaded and unpacked into the container at every
start, before Plex launches. The mod adds an s6 service definition and one static binary.
It does not modify Plex, the transcoder, hardware transcoding, or any existing setting.

## Requirements

- Unraid 6.10 or later with the linuxserver.io Plex container (template name `plex`,
  repository `lscr.io/linuxserver/plex`). The official `plexinc/pms-docker` image uses a
  different init system and is not supported by this mod.
- The Unraid server must be able to reach the registry hosting the mod over HTTPS at
  container start.

## Step 1: know your network mode

Open the Docker tab, click the Plex icon, choose Edit. Look at Network Type.

| Network type | What to do about port 32500 |
| --- | --- |
| Host (the template default) | Nothing. The service is reachable at `http://<unraid-ip>:32500` as soon as it starts. |
| Bridge or a custom network | Add a port mapping in step 3. |

## Step 2: add the mod variable

Still in the Edit view, switch on Advanced View (top right), scroll to the bottom and click
"Add another Path, Port, Variable, Label or Device".

| Field | Value |
| --- | --- |
| Config Type | Variable |
| Name | Docker mods |
| Key | `DOCKER_MODS` |
| Value | `ghcr.io/pwsh/plex-api-mod:latest` |

If the Plex template already has a `DOCKER_MODS` variable for another mod, keep it and append
this one with a pipe: `existing/mod:tag|ghcr.io/pwsh/plex-api-mod:latest`.

## Step 3: add the configuration variables

Add these the same way. All are optional, but set the token unless the API is bound to
localhost.

| Key | Suggested value | Meaning |
| --- | --- | --- |
| `PLEX_API_TOKEN` | a long random string | Required on every request except `/health`, as `Authorization: Bearer <token>` or `X-Plex-Api-Token: <token>`. |
| `PLEX_API_WRITE` | `false` | Leave off until you need write endpoints. When `true`, `/v1/exec` and `PATCH /v1/settings` are enabled. |
| `PLEX_API_WRITE_WHILE_RUNNING` | `false` | When `false`, writes are refused with 409 while Plex is listening on 32400. |
| `PLEX_API_BIND` | `0.0.0.0:32500` | Use `127.0.0.1:32500` to make the API reachable only from the Unraid host itself (host network mode) or from inside the container. |
| `PLEX_API_INDEXES` | `false` | When `true`, two extra indexes are added to the library database before Plex starts. See "Managed indexes" below. |
| `PLEX_API_INDEX_BACKUP` | `true` | Take an online backup of the library database before creating those indexes. |
| `PLEX_API_BACKUP_KEEP` | `3` | How many of those backups to keep. Each is the size of your library database. |
| `PLEX_API_BACKUP_DIR` | `/config/plex-api/backups` | Where they go, inside the container. |
| `PLEX_API_STATE_DIR` | `/config/plex-api` | Where `indexes.json` goes, inside the container. |

Bridge mode only: add one more entry.

| Field | Value |
| --- | --- |
| Config Type | Port |
| Container Port | `32500` |
| Host Port | `32500` |
| Connection Type | TCP |

Click Apply. Unraid removes and re-creates the container with the new settings. Plex data
in `/mnt/user/appdata/plex` is untouched.

## Step 4: check it

Container log (Docker tab, Plex icon, Logs) should contain:

```text
[mod-init] Adding pwsh/plex-api-mod:latest to container
[mod-init] Downloading pwsh/plex-api-mod:latest from ghcr.io
[mod-init] Installing pwsh/plex-api-mod:latest
[mod-init] pwsh/plex-api-mod:latest applied to container
Starting plex-api on port 32500 . . .
plex-api 0.1.0 listening on 0.0.0.0:32500
```

From any machine on the LAN:

```sh
curl http://<unraid-ip>:32500/health
curl -H "Authorization: Bearer <token>" "http://<unraid-ip>:32500/v1/search?q=star"
```

The health response shows the database paths, whether they exist, the schema migration
version, whether Plex is running, and the write policy in force.

## Managed indexes

Setting `PLEX_API_INDEXES` to `true` lets plex-api add two indexes of its own to the
library database. They make sorted library listings roughly forty times faster (7.7 ms to
0.2 ms on a 116k-item library). Both are named with a `zz_plexapi_` prefix so they can
never collide with an index Plex adds later.

The work happens in an s6 step that runs **before Plex starts**, so Plex is never stopped
and never competes for the database. Plex waits for it — that is the whole ordering.

What to expect:

- **The first start after you set the variable takes about 6 seconds longer.** The Plex
  container log shows the backup, the index build and the verification, and only then does
  Plex start.
- **Every start after that adds about 30 milliseconds** — it only checks that the indexes
  are still there.
- **A backup is taken first**, into
  `/mnt/user/appdata/plex/plex-api/backups/<timestamp>/com.plexapp.plugins.library.db`.
  It is a real online backup (SQLite's backup API), not a file copy, so it is consistent.
  The newest three are kept; older ones are deleted automatically. Each one is the size of
  your library database — about 1 GB is typical, so budget ~3 GB of appdata, or set
  `PLEX_API_BACKUP_KEEP` to `1`, or `PLEX_API_INDEX_BACKUP` to `false` if you back up
  appdata another way.
- **A Plex upgrade drops the indexes automatically**, before Plex runs its schema
  migrations, and the start after that rebuilds them. You will see one slower start after
  each Plex update. This is deliberate: SQLite will not let Plex drop a column that an
  index mentions, so our indexes get out of the way of migrations rather than risk blocking
  one.
- **Nothing else is changed.** The indexes are derived data; removing them is always safe.

The log lines look like this:

```text
[plex-api] checking managed indexes . . .
[plex-api] index status read in 16ms (schema_version=243, migrations=500000000001.231)
[plex-api] backup written to /config/plex-api/backups/20260912-212626/com.plexapp.plugins.library.db (965791744 bytes) in 545ms
[plex-api] created zz_plexapi_section_type_title, zz_plexapi_type_added in 1.651s; quick_check ok in 3.778s
[plex-api] wrote /config/plex-api/indexes.json (pms_version=v1.43.4.10903-e5521bd8c)
```

If anything goes wrong the step logs it and Plex starts anyway:

```text
[plex-api] index step failed (see above); Plex will start anyway
```

To check the current state at any time:

```sh
curl -H "Authorization: Bearer <token>" http://<unraid-ip>:32500/v1/indexes
```

To remove the indexes again, set `PLEX_API_INDEXES` to `false` and then, with Plex stopped:

```sh
docker exec plex s6-rc -t 60000 -d change svc-plex
docker exec -u abc plex /usr/local/bin/plex-api -drop-indexes
docker exec plex s6-rc -u change svc-plex
```

## Using the write endpoints safely

The database belongs to Plex. Plex keeps state in memory and is not told about outside
edits, so the recommended flow for anything beyond watch state is to stop Plex, write, then
start it again. You can do that without stopping the container, which keeps the API up:

```sh
docker exec plex s6-rc -t 60000 -d change svc-plex   # ask Plex to stop, wait up to 60 s
docker exec plex s6-svstat /run/service/svc-plex     # should say "down"
# ... make your changes through the API ...
docker exec plex s6-rc -u change svc-plex            # start Plex again
```

Run these from the Unraid terminal or a User Scripts script. While Plex is down, `/health`
reports `pms_running: false` and writes are permitted with `PLEX_API_WRITE=true` alone.
The API keeps serving throughout; it was verified to answer while Plex was down and to see
Plex back within seconds of the start command.

If `s6-svstat` still says `up ... want down` after the timeout, Plex received the stop
signal but hung during shutdown. In testing this happened with a fresh, unclaimed server.
Force it with:

```sh
docker exec plex s6-svc -k /run/service/svc-plex     # SIGKILL; s6 will not restart it
```

The database is in WAL mode and survives this; a `quick_check` afterwards returned `ok`.
Still, prefer to wait for a clean stop on a real library, and take the backup first.

Take a backup first. The Plex databases live in
`/mnt/user/appdata/plex/Library/Application Support/Plex Media Server/Plug-in Support/Databases/`.
The Appdata Backup plugin, or a plain copy of that folder while Plex is stopped, is enough.

Two caveats from the research in `docs/PlexDirectAccessFeasibility.md`:

- Editing a title through SQL indexes it differently from how Plex would. Search for that
  item by show name can fail until Plex refreshes it. Prefer the Plex HTTP API for title
  edits; use plex-api for bulk work, paths, watch state, and cleanup.
- A Plex upgrade can change the schema. Compare the `schema_migrations` value in `/health`
  with the one this version was tested against (`500000000001.231`) before bulk writes.

## Updating and pinning

The mod is downloaded fresh at every container start, so restarting the Plex container picks
up whatever the tag currently points to. `dev` moves with each build. For a production
server, use a fixed version tag once one is published, and change it deliberately.

## Security notes

- In host network mode port 32500 is open to your LAN. Set `PLEX_API_TOKEN`, or bind to
  `127.0.0.1` and reach it only from the Unraid host.
- The API is exactly as privileged as the database. Do not expose it through a reverse
  proxy to the internet.
- Docker mods run as root during install (they only unpack files), and the service itself
  runs as the container's `abc` user, which Unraid maps to PUID 99 / PGID 100 by default.
  That is the same user that owns the Plex databases.

## Editing the template file directly

The UI writes the template to
`/boot/config/plugins/dockerMan/templates-user/my-plex.xml`. The same additions can be made
there and applied by editing the container once in the UI. The entries look like:

```xml
<Config Name="Docker mods" Target="DOCKER_MODS" Default="" Mode=""
  Description="linuxserver docker mods, pipe separated" Type="Variable"
  Display="always" Required="false" Mask="false">ghcr.io/pwsh/plex-api-mod:latest</Config>
<Config Name="plex-api token" Target="PLEX_API_TOKEN" Default="" Mode=""
  Description="Bearer token for plex-api" Type="Variable"
  Display="always" Required="false" Mask="true">change-me</Config>
<!-- bridge mode only -->
<Config Name="plex-api port" Target="32500" Default="32500" Mode="tcp"
  Description="plex-api HTTP" Type="Port" Display="always" Required="false" Mask="false">32500</Config>
```text

## Troubleshooting

| Symptom | Cause and fix |
| --- | --- |
| No `[mod-init]` lines in the log | `DOCKER_MODS` is not set on the container. Check the variable key spelling. |
| `[mod-init]` reports a download failure | The Unraid host cannot reach the registry over HTTPS, or the tag does not exist. Set `DOCKER_MODS_DEBUG=true` and restart for details. |
| `unable to exec /usr/local/bin/plex-api: No such file or directory` | The mod image has more than one layer; the loader installs only the first. Rebuild with the single-layer `deploy/Dockerfile.mod`. |
| `/health` shows `exists: false` for the databases | Plex has not created them yet (fresh install), or `/config` is mapped somewhere other than `/mnt/user/appdata/plex`. Set `PLEX_API_DB_DIR` if your layout differs. |
| 401 on every request | Send the token as `Authorization: Bearer <token>`. `/health` never needs it. |
| 403 `writes_disabled` or 409 `pms_running` | Write policy. See "Using the write endpoints safely". |
| `index step failed` in the log | Plex still starts. Common causes: `/config/plex-api` is not writable by PUID/PGID, or the appdata share is out of space for the backup. The full error is on the line above. |
| The first start after setting `PLEX_API_INDEXES` seems to hang | It is copying the database and building the indexes; about 6 seconds for a 1 GB library, longer on a slow array. |

## Alternative: a pre-built layer image

If a registry that allows anonymous pulls is not available, build `deploy/Dockerfile.layer`
elsewhere, push the result to a private registry, and point the Plex template's Repository
field at it. Unraid cannot build images itself, and `docker login` credentials on Unraid do
not survive a reboot unless the login command is added to `/boot/config/go`. The mod route
avoids both problems, which is why it is the recommended one.
