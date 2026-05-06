SHELL := /bin/sh

APP := torrent-stream-hub
BIN_DIR := bin
BIN := $(BIN_DIR)/$(APP)

PORT ?= 8080
TORRENT_PORT ?= 50007
DOWNLOAD_DIR ?= $(CURDIR)/downloads
CONFIG_DIR ?= $(CURDIR)/config
DB_PATH ?= $(CONFIG_DIR)/hub.db

.PHONY: help deps web build build-go run run-local test clean docker-down

help:
	@printf '%s\n' \
		'Usage:' \
		'  make build        Build Web GUI and Go service' \
		'  make run-local    Build and run locally with docker-compose-like env' \
		'  make run          Alias for run-local' \
		'  make test         Run Go tests and frontend build' \
		'  make docker-down  Stop docker-compose service' \
		'  make clean        Remove local binary' \
		'' \
		'Overrides:' \
		'  make run-local PORT=8080 TORRENT_PORT=50007' \
		'  make run-local DOWNLOAD_DIR=/path/to/downloads CONFIG_DIR=/path/to/config'

deps:
	cd web && npm ci

web:
	cd web && npm run build

build-go:
	mkdir -p "$(BIN_DIR)"
	go build -trimpath -ldflags="-s -w" -o "$(BIN)" ./cmd/hub

build: web build-go

run: run-local

run-local: build
	mkdir -p "$(DOWNLOAD_DIR)" "$(CONFIG_DIR)"
	HUB_PORT="$(PORT)" \
	HUB_TORRENT_PORT="$(TORRENT_PORT)" \
	HUB_DOWNLOAD_DIR="$(DOWNLOAD_DIR)" \
	HUB_DB_PATH="$(DB_PATH)" \
	HUB_MAX_ACTIVE_STREAMS="4" \
	HUB_MAX_ACTIVE_DOWNLOADS="5" \
	HUB_MIN_FREE_SPACE_GB="5" \
	HUB_DOWNLOAD_LIMIT="0" \
	HUB_UPLOAD_LIMIT="0" \
	HUB_STREAM_CACHE_SIZE="209715200" \
	HUB_BT_SEED="true" \
	HUB_BT_NO_UPLOAD="false" \
	HUB_BT_CLIENT_PROFILE="qbittorrent" \
	HUB_BT_DOWNLOAD_PROFILE="balanced" \
	HUB_BT_BENCHMARK_MODE="false" \
	HUB_BT_PUBLIC_IP_DISCOVERY_ENABLED="false" \
	HUB_BT_PUBLIC_IPV4="" \
	HUB_BT_PUBLIC_IPV6="" \
	HUB_BT_RETRACKERS_MODE="append" \
	HUB_BT_RETRACKERS_FILE="$(CONFIG_DIR)/trackers.txt" \
	HUB_BT_DISABLE_DHT="false" \
	HUB_BT_DISABLE_PEX="false" \
	HUB_BT_DISABLE_UPNP="false" \
	HUB_BT_DISABLE_TCP="false" \
	HUB_BT_DISABLE_UTP="true" \
	HUB_BT_DISABLE_IPV6="false" \
	HUB_BT_SWARM_WATCHDOG_ENABLED="true" \
	HUB_BT_SWARM_CHECK_INTERVAL_SEC="60" \
	HUB_BT_SWARM_REFRESH_COOLDOWN_SEC="180" \
	HUB_BT_SWARM_MIN_CONNECTED_PEERS="8" \
	HUB_BT_SWARM_MIN_CONNECTED_SEEDS="2" \
	HUB_BT_SWARM_STALLED_SPEED_BPS="32768" \
	HUB_BT_SWARM_STALLED_DURATION_SEC="180" \
	HUB_BT_SWARM_BOOST_DURATION_SEC="300" \
	HUB_BT_SWARM_PEER_DROP_RATIO="0.45" \
	HUB_BT_SWARM_SEED_DROP_RATIO="0.45" \
	HUB_BT_SWARM_SPEED_DROP_RATIO="0.35" \
	HUB_BT_SWARM_PEAK_TTL_SEC="1800" \
	HUB_BT_SWARM_HARD_REFRESH_ENABLED="true" \
	HUB_BT_SWARM_AUTO_HARD_REFRESH_ENABLED="false" \
	HUB_BT_SWARM_HARD_REFRESH_COOLDOWN_SEC="900" \
	HUB_BT_SWARM_HARD_REFRESH_AFTER_SOFT_FAILS="1" \
	HUB_BT_SWARM_HARD_REFRESH_MIN_TORRENT_AGE_SEC="60" \
	HUB_BT_SWARM_DEGRADATION_EPISODE_TTL_SEC="900" \
	HUB_BT_SWARM_RECOVERY_GRACE_SEC="180" \
	HUB_BT_CLIENT_RECYCLE_ENABLED="true" \
	HUB_BT_CLIENT_RECYCLE_COOLDOWN_SEC="300" \
	HUB_BT_CLIENT_RECYCLE_AFTER_HARD_FAILS="1" \
	HUB_BT_CLIENT_RECYCLE_AFTER_SOFT_FAILS="1" \
	HUB_BT_CLIENT_RECYCLE_MIN_TORRENT_AGE_SEC="60" \
	HUB_BT_CLIENT_RECYCLE_MIN_TORRENTS="1" \
	HUB_BT_CLIENT_RECYCLE_MAX_PER_HOUR="2" \
	HUB_AUTH_ENABLED="false" \
	HUB_AUTH_USER="admin" \
	HUB_AUTH_PASSWORD="admin" \
	"$(BIN)"

test:
	go test ./...
	cd web && npm run build

docker-down:
	docker compose down

clean:
	rm -f "$(BIN)"
