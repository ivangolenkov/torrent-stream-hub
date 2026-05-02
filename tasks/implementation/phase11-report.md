# Отчет по фазе 11: Hard swarm refresh и trend-based degradation

## Что было сделано

- Добавлены настройки Phase 11 в `internal/config/config.go`:
  - `HUB_BT_SWARM_PEER_DROP_RATIO`;
  - `HUB_BT_SWARM_SEED_DROP_RATIO`;
  - `HUB_BT_SWARM_SPEED_DROP_RATIO`;
  - `HUB_BT_SWARM_PEAK_TTL_SEC`;
  - `HUB_BT_SWARM_HARD_REFRESH_ENABLED`;
  - `HUB_BT_SWARM_HARD_REFRESH_COOLDOWN_SEC`;
  - `HUB_BT_SWARM_HARD_REFRESH_AFTER_SOFT_FAILS`;
  - `HUB_BT_SWARM_HARD_REFRESH_MIN_TORRENT_AGE_SEC`.
- Добавлены defaults/clamping для ratios и cooldown constraints.
- Расширен `ManagedTorrent` runtime-only состоянием:
  - source URI;
  - added time;
  - recent peaks для connected peers, seeds, download speed;
  - soft/hard refresh counters;
  - last hard refresh diagnostics.
- Добавлена trend-based degradation в swarm watchdog:
  - падение connected peers относительно recent peak;
  - падение seeds относительно recent peak;
  - падение download speed относительно recent peak;
  - TTL для peak metrics.
- Soft refresh оставлен первой попыткой восстановления.
- Добавлен hard refresh path в `internal/engine/engine.go`:
  - строит `TorrentSpec` из сохраненного source URI или fallback infohash;
  - вызывает `Drop()` для старого runtime torrent-а;
  - заново добавляет torrent через `AddTorrentSpec()`;
  - переносит runtime state, source URI и diagnostics;
  - не удаляет файлы и SQLite записи.
- Добавлен hard refresh gating:
  - disabled flag;
  - torrent age;
  - cooldown;
  - soft refresh attempts;
  - active stream/debounce block;
  - paused/error/missing/diskfull block.
- Добавлен `StreamManager.ActiveStreamsForTorrent()` и убран риск lock inversion при включении/выключении sequential mode.
- Расширены `models.BTHealth` и `models.BTTorrentHealth` новыми diagnostics полями.
- Обновлен Web GUI `/health/bt`:
  - hard refresh status/config;
  - peak connected/seeds/speed;
  - soft/hard refresh counters;
  - last hard refresh reason/error;
  - blocked reason;
  - active stream count.
- Обновлены `docker-compose.yml` и `torrent-stream-hub-tz-v3.md` с новыми ENV.
- Добавлены/обновлены тесты:
  - config defaults/clamping;
  - trend degradation;
  - expired peaks;
  - completed seeding ignores speed trend;
  - hard refresh gating.

## Статус выполнения DoD

- [x] Trend-based degradation detects meaningful peer/seed/speed decay relative to recent peak.
- [x] Soft refresh remains first recovery step.
- [x] Hard refresh safely drops and re-adds runtime torrent without deleting files or SQLite records.
- [x] Hard refresh is rate-limited by cooldown, torrent age and soft fail count.
- [x] Hard refresh is blocked while stream is active.
- [x] Source URI / fallback infohash is available for re-add.
- [x] BT Health exposes peak/trend/soft/hard refresh diagnostics.
- [x] Web GUI shows hard refresh diagnostics on `/health/bt`.
- [x] Diagnostics and logs do not expose peer IP/ports.
- [x] `docker-compose.yml` and `torrent-stream-hub-tz-v3.md` include new settings.
- [x] Added/updated tests cover config, trend decisions, hard refresh gating and API fields.
- [x] `go test ./...`.
- [x] `(cd web && npm run build)`.
- [x] `CGO_ENABLED=0 go build ./cmd/hub`.

## Причины отхода от плана

- Hard refresh активного stream-а полностью блокируется, включая debounce window. Исключение для длительного streaming stall оставлено вне этой фазы, потому что сейчас нет отдельного надежного stall detector-а на уровне stream playback.
- Hard refresh для `.torrent` runtime использует сохраненный magnet URI из metainfo, как и restore flow. Повторная загрузка исходного `.torrent` файла не требуется.
- Принудительный tracker announce через private API `anacrolix/torrent` не добавлялся. Hard refresh использует публичные `Drop()` и `AddTorrentSpec()`.
