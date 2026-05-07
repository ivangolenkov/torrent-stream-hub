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
| `HUB_MIN_FREE_SPACE_GB` | `5` | Minimum free disk space required for downloading. |
| `HUB_DOWNLOAD_LIMIT` | `0` | Download limit in bytes per second, `0` means unlimited. |
| `HUB_UPLOAD_LIMIT` | `0` | Upload limit in bytes per second, `0` means unlimited. |
| `HUB_STREAM_CACHE_SIZE` | `209715200` | Streaming sliding-window cache size in bytes. |
| `HUB_AUTH_ENABLED` | `false` | Enable Basic Auth. |
| `HUB_AUTH_USER` | `admin` | Basic Auth user. |
| `HUB_AUTH_PASSWORD` | `admin` | Basic Auth password. |
| `HUB_LOG_LEVEL` | `debug` | Log level. |

BitTorrent-related options are also available, including DHT/PEX/UPnP toggles, retracker configuration and swarm watchdog tuning. See `internal/config/config.go` for the full list.

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
