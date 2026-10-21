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
      HUB_AUTH_ENABLED: "false"
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
| `HUB_AUTH_ENABLED` | `false` | Enable Basic Auth. |
| `HUB_AUTH_USER` | `admin` | Basic Auth user. |
| `HUB_AUTH_PASSWORD` | `admin` | Basic Auth password. |
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

Keep the service on a trusted LAN or behind an authenticated proxy: the existing management API does not enforce Basic Auth, and CORS `*` intentionally permits Smart TV clients.

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

**Security:** management routes currently do not enforce Basic Auth, even when `HUB_AUTH_ENABLED` is set. Anyone with network access to the service can read its logs. Keep it on a trusted LAN or protect it with an authenticated reverse proxy; permissive CORS is intentional for Smart TV compatibility. Masking known parameters is not a guarantee that all log content is free of sensitive data.

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
