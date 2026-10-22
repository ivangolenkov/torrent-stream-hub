# Torrent-Stream-Hub

Torrent-Stream-Hub is a hybrid torrent client and HTTP streaming server for local networks. It combines regular background torrent downloading and seeding with sequential file loading for real-time playback over HTTP with `Range` request support.

The service is designed for home LAN usage, including low-power devices such as NAS boxes. It exposes a TorrServer-compatible API for Smart TV applications such as Lampa and Num, and also provides its own Web GUI built with Vue 3.

## Features

- Add and manage torrents through a Web GUI or API.
- Stream torrent files over HTTP while data is being downloaded sequentially.
- Support media players that use HTTP `Range` requests.
- Persist torrent state in SQLite.
- Use WAL mode for database reliability.
- Run as a single binary or as a Docker container.
- Publish a LAN-friendly API with permissive CORS.

## Storage Layout, Upgrade And Recovery

Payload is stored under `HUB_DOWNLOAD_DIR/payload/<stable-infohash>/`, followed
by the original metainfo paths. SQLite stores the versioned mapping; hybrid
v1/v2 aliases share one stable identity and directory. Do not move payload
directories or edit mappings while the service is running. Keep the database,
metainfo and other service files outside the payload namespace.

Before upgrading an installation using the old shared layout:

1. Stop the service cleanly. Back up the **entire download and config directories**,
   including SQLite, metainfo, completion data and any remaining `-wal`/`-shm`
   files as one consistent, offline snapshot.
2. Provide additional free space for a copy of the largest torrent being migrated,
   plus the configured `HUB_MIN_FREE_SPACE_GB` reserve. Migration copies files
   sequentially, preserving original files until the mapping commit. It does not
   use an in-place rename optimization.
3. Start the new binary. A background worker inventories ownership, journals the
   copy, checks its digest and performs an offline torrent hash pass. Existing
   partial or corrupt pieces remain incomplete and must be downloaded again;
   verification does not turn them into trusted data. Migration does not change
   temporary/permanent policy, FIFO position or saved file priorities.
4. Monitor `storage_state` / `storage_error` in the torrent list/API and the logs.
   `legacy`, `moving` and `blocked` entries cannot write or stream. Deletion is
   refused while a migration journal is pending or ownership cannot be proven.
   Unaffected ready entries remain usable.
   Interrupted migrations retain their journal and retry automatically (including
   after restart); do not delete journal-owned copies or temporary files manually.

Shared legacy paths, an occupied target, missing original metainfo, symlinks,
hardlinks or ambiguous aliases cause a conservative block rather than choosing
an owner or overwriting files. Stop the service and make another backup before
repairing anything. Restore the exact original `.torrent` in the config
`metainfo/<stable-infohash>.torrent` location when it is missing; free disk space
or correct permissions for I/O failures. Restart to retry. For ambiguous ownership
or an occupied target, restore a known-consistent backup or resolve ownership
offline; never merge directories or blindly remove a conflicting target. Preserve
both copies and the database journal if recovery needs manual investigation.

After each startup/reopen, existing payload is hash-checked again with bounded
background concurrency; old completion hints, a final filename, file size and a
saved 100% progress value do not authorize streaming/download/seeding. Verified
pieces can be read while other pieces are still being checked. Direct streaming
and completed downloads require all relevant pieces to be verified. Changes to
inode, size or modification time revoke trust and trigger rechecking when
observed. External writers must remain stopped during verification and transfers:
stat checks cannot detect content changes that preserve every checked attribute,
nor guarantee an immutable file while it is being served.

**Downgrade is not an in-place operation.** Old binaries do not understand the new
mapping. To return to an old version, stop the new service and restore the entire
pre-upgrade download/config snapshot together, then start the old binary. Do not
point an old binary at the migrated database or isolated payload tree.

## Download Priorities And DiskFull Recovery

File priorities (Normal/High/Skip) are persisted user settings, not runtime
statistics. Startup and Resume restore them before enabling payload requests,
including when a magnet is still waiting for metadata. Stream/preload overlays
do not change these saved settings. A paused torrent stays paused after restart.
Use Resume, not a duplicate add, to restore an existing DB-only torrent.

The service checks free space on each payload root's filesystem before admitting
payload downloads, at Resume, and periodically. Falling below
`HUB_MIN_FREE_SPACE_GB`, an unknown free-space measurement, or a payload storage
write error closes the library-level download gate, including reader/stream
requests and queued torrents. An unknown measurement is reported separately from
an actual `ENOSPC`. The block is stored separately in SQLite and survives restart;
freeing space alone does **not** restart downloads.

To recover, free space (or correct the measurement/storage error), then explicitly
Resume each affected torrent. Resume returns HTTP 409 if the disk check still
fails, or HTTP 500 if its database changes cannot be committed; it does not report
a false success. Successful Resume retains file priorities and obeys the background
download limit. `download_block` and the error reason are visible through REST/SSE
and the Web GUI; manually paused torrents are not activated by the disk monitor.

Already verified data can still be streamed/downloaded by the direct disk fast
path and seeded without payload writes. A stream waiting for missing data fails
with a diagnostic error when blocked; it must be reopened after recovery. The
service does not reset QoS reference counts/debounce timers to remove a disk block.

The gate cancels pending/new payload requests; it cannot roll back a system write
already in progress. Such in-flight operations may finish or fail. Writer errors
also close admission synchronously in the torrent library, before the asynchronous
application observer persists the reason; automatic callbacks cannot erase that
error latch. SQLite, completion-cache and log writes themselves can fail on a full
or read-only volume. Persistence is not guaranteed when the underlying filesystem
cannot commit it: failure keeps the runtime gate closed, and startup rechecks disk
space before any payload admission. Keep backups and leave free-space headroom.

## Concurrent Commands, Shutdown And Deletion Recovery

Pause, Resume, scheduler activation and stream priority reconcile share an engine
admission lock. A delayed metadata/recheck/discovery/stream callback cannot undo
a completed Pause or act on a deleted torrent generation. Stream overlays combine
ranges across files, including shared boundary pieces. Seek debounce and stream
reference counting remain active; old timer/session tokens cannot clear a new
session or a re-added torrent with the same hash.

The background scheduler reserves slots atomically, including metadata-pending
downloads and active rechecks. A positive `HUB_MAX_ACTIVE_DOWNLOADS` limits
background downloads; zero does not promote background downloads. Stream-driven
queued requests remain separate and do not enable the whole background selection.
Queue ordering and selected-files completion policy have not changed.

Shutdown rejects new work, cancels torrent/reader contexts and timers, joins
accepted engine tasks, and then closes library/storage resources. Cancellation
cannot forcibly interrupt an already running filesystem syscall or an arbitrary
blocking input reader supplied to an add operation. Internal ownership and lock
ordering are documented in `internal/engine/CONCURRENCY.md`.

Deletion records durable intent before stopping the runtime. With
`delete_files=true`, a verified manifest is committed before unlink; with
`delete_files=false`, mode `keep` forbids payload removal, including on retries.
If deletion fails, REST/SSE and both GUI layouts show `deletion_mode` and
`deletion_error`. Resume, transfers and re-add conflict with pending deletion;
restart does not restore its download. Fix the reported problem and retry Delete
**with the same delete-files mode**. A different mode returns a conflict, not an
implicit escalation to destructive deletion. Missing files are safe on retry.

Manual pending deletions are not automatically retried at startup; temporary
payload cleanup retains its existing periodic retry policy. An unsafe/incomplete
manifest is never guessed or treated as proof of an empty payload. Deletion errors
return failure, not success with hidden pending work. If recording the error also
fails, the already committed intent remains the recovery authority.

Final database deletion is transactional. Payload deletion removes unused mapping
aliases with their root; aliases still referenced by another torrent are retained.
Keep-data deletion deliberately retains namespace reservations so a later re-add
can reopen the same isolated data. Cached metainfo and application-owned files
are not payload and are not removed. Schema upgrades import existing cleanup
manifests as pending `payload` deletion; back up SQLite and payload before upgrades.

## Ports

- `8080/tcp` - Web GUI and HTTP API.
- `50007/tcp` - BitTorrent TCP port.
- `50007/udp` - BitTorrent UDP port.

Open `http://<host-ip>:8080` from another device in the same local network after starting the service.

## Run With Docker

The Docker image is published to GitHub Container Registry:

```bash
docker run -d \
  --name torrent-stream-hub \
  --restart unless-stopped \
  -p 8080:8080 \
  -p 50007:50007/tcp \
  -p 50007:50007/udp \
  -v "$PWD/downloads:/downloads" \
  -v "$PWD/config:/config" \
  -e HUB_PORT=8080 \
  -e HUB_TORRENT_PORT=50007 \
  -e HUB_DOWNLOAD_DIR=/downloads \
  -e HUB_DB_PATH=/config/hub.db \
  ghcr.io/ivangolenkov/torrent-stream-hub:latest
```

## Run With Docker Compose

Create `docker-compose.yml`:

```yaml
services:
  torrent-stream-hub:
    image: ghcr.io/ivangolenkov/torrent-stream-hub:latest
    container_name: torrent-stream-hub
    restart: unless-stopped
    ports:
      - "8080:8080"
      - "50007:50007/tcp"
      - "50007:50007/udp"
    volumes:
      - ./downloads:/downloads
      - ./config:/config
    environment:
      HUB_PORT: "8080"
      HUB_TORRENT_PORT: "50007"
      HUB_DOWNLOAD_DIR: /downloads
      HUB_DB_PATH: /config/hub.db
      HUB_MIN_FREE_SPACE_GB: "5"
```

Start it:

```bash
docker compose up -d
```

View logs:

```bash
docker compose logs -f
```

Stop it:

```bash
docker compose down
```

## Run From Binary

Download a binary archive from GitHub Releases for your platform:

- macOS Apple Silicon: `torrent-stream-hub-macos-arm64.tar.gz`
- macOS Intel: `torrent-stream-hub-macos-amd64.tar.gz`
- Windows x86_64: `torrent-stream-hub-windows-amd64.zip`
- Linux x86_64: `torrent-stream-hub-linux-amd64.tar.gz`

Example for Linux or macOS:

```bash
mkdir -p downloads config
tar -xzf torrent-stream-hub-linux-amd64.tar.gz

HUB_PORT=8080 \
HUB_TORRENT_PORT=50007 \
HUB_DOWNLOAD_DIR="$PWD/downloads" \
HUB_DB_PATH="$PWD/config/hub.db" \
./torrent-stream-hub-linux-amd64
```

Example for Windows PowerShell:

```powershell
New-Item -ItemType Directory -Force downloads, config
Expand-Archive .\torrent-stream-hub-windows-amd64.zip -DestinationPath .

$env:HUB_PORT = "8080"
$env:HUB_TORRENT_PORT = "50007"
$env:HUB_DOWNLOAD_DIR = "$PWD\downloads"
$env:HUB_DB_PATH = "$PWD\config\hub.db"
.\torrent-stream-hub-windows-amd64.exe
```

## Configuration

The service can be configured with command-line flags or environment variables. Common options:

| Environment variable | Default | Description |
| --- | --- | --- |
| `HUB_PORT` | `8080` | Web GUI and API port. |
| `HUB_TORRENT_PORT` | `50007` | BitTorrent listen port. |
| `HUB_DOWNLOAD_DIR` | `/downloads` | Directory for downloaded torrent files. |
| `HUB_DB_PATH` | `/config/hub.db` | SQLite database path. |
| `HUB_MAX_ACTIVE_STREAMS` | `4` | Maximum active streams. |
| `HUB_MAX_ACTIVE_DOWNLOADS` | `5` | Maximum active background downloads. |
| `HUB_MAX_TEMPORARY_TORRENTS` | `3` | Maximum temporary torrents (`--max-temporary-torrents`); positive integer, restart required. |
| `HUB_MIN_FREE_SPACE_GB` | `5` | Minimum free disk space required for downloading. |
| `HUB_DOWNLOAD_LIMIT` | `0` | Download limit in bytes per second, `0` means unlimited. |
| `HUB_UPLOAD_LIMIT` | `0` | Upload limit in bytes per second, `0` means unlimited. |
| `HUB_STREAM_CACHE_SIZE` | `209715200` | Streaming sliding-window cache size in bytes. |
| `HUB_LOG_LEVEL` | `debug` | Log level. |
| `HUB_LOG_DIR` | `logs` beside the database | Daily log directory (`--log-dir`). |
| `HUB_LOG_RETENTION_DAYS` | `3` | Previous calendar days to keep, in addition to today (`--log-retention-days`). `0` keeps today only; valid range: 0–36500. |

BitTorrent-related options are also available, including DHT/PEX/UPnP toggles, retracker configuration and swarm watchdog tuning. See `internal/config/config.go` for the full list.

## Temporary Torrents And Automatic Cleanup

**Warning:** TorrServer-compatible `POST /torrents` with `action: "add"` and `save_to_db: false` **or without `save_to_db`** now adds a temporary torrent. Exceeding the configured limit automatically removes the oldest temporary torrent **and its downloaded files**, including `.part` files. This is FIFO by first successful addition, not by last playback. Repeating an add does not move the torrent to the end of the queue.

Temporary torrents are still saved in SQLite and survive restarts. The default limit is **3**, configurable through `HUB_MAX_TEMPORARY_TORRENTS` or `--max-temporary-torrents` (CLI wins). Only positive integers are accepted; `0` does not disable cleanup. Reducing the limit takes effect at startup after restarting the service.

Permanent torrents do not count toward the limit and are never selected for automatic cleanup. All pre-existing records are migrated as permanent. Native Web GUI/API additions and `/torrent/upload` additions are permanent, and also promote an existing temporary torrent. Repeating TorrServer `add` with `save_to_db: true` promotes the same infohash permanently; a later `false` cannot undo that promotion. There is no conversion back to temporary in this version.

The GUI displays **«Временный»** with **«Сделать постоянным»** in desktop/mobile lists and torrent details. Alternatively:

```bash
curl -X POST 'http://localhost:8080/api/v1/torrent/HASH/action' \
  -H 'Content-Type: application/json' \
  -d '{"action":"make_permanent"}'
```

Promotion is idempotent (`200`), with `404` for a missing torrent and `500` if persistence fails. Native REST lists and SSE include `is_temporary` explicitly.

Cleanup runs after a successful **new** temporary addition, at startup and every five seconds for deferred work. Playback (both torrent-reader and direct-file paths), preload and HTTP downloads protect the oldest torrent; protection includes the ten-second debounce after a transfer ends. While protected, it blocks the FIFO queue: newer torrents are not removed instead, and the limit may temporarily be exceeded. No global lifecycle lock is held during video/file transmission or HTTP-link resolution.

Cleanup errors do not roll back a successful addition or stop the service. A failed deletion stays visible, is logged and retried; the candidate is marked paused when deletion intent is saved. Exact payload paths are finalized after stopping the engine and persisted before unlinking, permitting retry after partial deletion/restart. An interrupted, incomplete manifest requires known metadata before retry, rather than guessing that there were no files. Unsafe paths, symlinks, application storage, shared/aliased payload paths, or inability to establish ownership cause cleanup to stop rather than guess. A metadata-pending torrent can therefore postpone deletion of another torrent's files until non-overlap can be established. Only confirmed payload files and empty parent directories are removed; arbitrary orphan files and metainfo caches are not swept.

Keep the service on a trusted LAN or behind an authenticated proxy: there is no built-in authentication, and CORS `*` intentionally permits Smart TV clients.

### File Safety And Upgrade Notes (Phase 34)

Manual deletion and automatic cleanup use the same exact metadata-derived paths, including `.part`, and remove only empty parent directories. They never guess a second path or recursively delete a directory by torrent name. Active transfers and the ten-second debounce also block manual deletion (`409`). A pending deletion is paused and cannot be resumed, re-added or used for a new transfer; retry Delete after fixing the reported storage conflict, or explicitly remove only the record to retain the files. Automatic cleanup retries within its FIFO/limit policy; manual deletion is retried explicitly.

File/ZIP downloads open inside the download root and reject symlinks, service storage, shared/aliased paths and unconfirmed layouts (`409`). Missing files return `404`; storage failures return generic `500` errors. Legacy records without matching metainfo must not be served or deleted by guessing from their name/size. Restore their original metainfo from backup before retrying. Keep a backup before manipulating legacy data. Filesystem operations are not a sandbox against a local administrator or external writer: hardlinks outside registered storage and same-size content changes are not cryptographically detected. Verified completion and isolated payload layout are planned separately.

Removed nonfunctional authentication options are no longer supported: remove old authentication CLI flags (now rejected) and environment entries (no longer read) from deployment scripts. There is no built-in access control; management API and logs are accessible to anyone who can reach the service. Use a trusted network or protected reverse proxy.

SQLite uses WAL for file databases, with foreign keys and a 5000 ms busy timeout on every pooled connection. In-memory databases use a single connection and SQLite's memory journal instead of WAL.

## Saved Logs And REST Access

Application, standard Go log, HTTP access and torrent library logs go to the console and daily files such as `hub-2026-10-20.log`. Library debug tracing is not enabled. Files use the process's local timezone, matching their timestamps; Docker commonly uses UTC. Restarting appends to the existing file.

With the standard database path, files live in `/config/logs` (host directory `./config/logs` in Compose), so they survive container restarts. The service user must be able to write there. New directories use permissions `0700`, new files `0600`; existing permissions are not changed.

Retention `3` keeps today plus the previous three calendar dates: on October 20, October 17–20 are retained. Cleanup runs on startup, on rotation and hourly even without new messages. It only removes matching regular daily files, excluding the active writer and files being downloaded. A completed download releases its retention pin; removal occurs at the next cleanup. Retention does **not** limit the size of today's file. File errors (including a full disk) are reported to stderr at most once a minute; console logging continues and file writes are retried after a minute.

```bash
# Today; optional last N physical lines
curl 'http://localhost:8080/api/v1/logs'
curl 'http://localhost:8080/api/v1/logs?tail=200'

# A single date, or an inclusive date range
curl 'http://localhost:8080/api/v1/logs?date=2026-10-19&tail=200'
curl 'http://localhost:8080/api/v1/logs?from=2026-10-18&to=2026-10-20&tail=500'
```

The response is plain UTF-8 text with `Cache-Control: no-store` and CORS `*`. Without dates it selects today. `date` cannot be combined with `from`/`to`; both range boundaries are required. Dates must strictly match `YYYY-MM-DD`. Duplicate/empty parameters and invalid combinations return `400`. `tail` accepts 1–100000 and selects the last N lines across all selected files, returned oldest first. Stack traces count as multiple lines. Fewer than N available lines returns all of them.

Missing days inside a range are skipped. No matching file returns `404`; an empty existing file returns `200` with an empty body. Other read failures return a generic JSON `500` before transfer starts; a transfer error never appends JSON to the log text.

Responses are finite size-bounded snapshots, not live-follow (`tail -f`). Bytes appended after opening are excluded; an unfinished last line is included as it was captured. When combining files, a newline is inserted between nonempty files if the preceding file lacks one, preventing lines from different dates merging. A trailing newline is not counted as an extra empty line. Reading/tail uses bounded buffers, including for very long lines.

Sensitive query parameters are masked before console/file output. Access logs omit query strings and headers. Handled HTTP panics include their stack trace, but arbitrary direct writes to stdout/stderr and fatal Go runtime dumps are **not** captured in daily files.

**Security:** there is no built-in authentication. Anyone with network access to the service can read its logs. Keep it on a trusted LAN or protect it with an authenticated reverse proxy; permissive CORS is intentional for Smart TV compatibility. Masking known parameters is not a guarantee that all log content is free of sensitive data.

## Build Locally

Requirements:

- Go `1.25.8` or compatible.
- Node.js `22` or compatible.
- npm.

Install frontend dependencies:

```bash
make deps
```

Build Web GUI and Go service:

```bash
make build
```

Run locally with default LAN-oriented settings:

```bash
make run-local
```

Run tests and frontend build:

```bash
make test
```

## Releases

GitHub Actions builds releases when a version tag is pushed:

```bash
git tag v0.1.0
git push origin v0.1.0
```

The release workflow publishes:

- macOS `arm64` binary archive.
- macOS `amd64` binary archive.
- Windows `amd64` binary archive.
- Linux `amd64` binary archive.
- Docker image to `ghcr.io/ivangolenkov/torrent-stream-hub`.

The Docker image receives the version tag and `latest` for tagged releases.
