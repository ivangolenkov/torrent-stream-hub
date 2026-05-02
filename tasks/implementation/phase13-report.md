# Phase 13 Report: Client recycle как основной recovery

## Что было сделано

- Изменен automatic recovery flow BitTorrent watchdog:
  - degraded torrent сначала получает soft refresh;
  - если torrent остается degraded после soft refresh, планируется client recycle;
  - automatic per-torrent hard refresh отключен по умолчанию через `HUB_BT_SWARM_AUTO_HARD_REFRESH_ENABLED=false`.
- Добавлены config options:
  - `HUB_BT_SWARM_AUTO_HARD_REFRESH_ENABLED`;
  - `HUB_BT_CLIENT_RECYCLE_AFTER_SOFT_FAILS`;
  - `HUB_BT_CLIENT_RECYCLE_MIN_TORRENT_AGE_SEC`.
- Client recycle cooldown сделан независимым от hard refresh cooldown, чтобы диагностический `300s` не поднимался обратно до `900s`.
- Добавлен metainfo-preserving re-add path:
  - `ManagedTorrent` сохраняет runtime `metainfo` после получения metadata;
  - re-add предпочитает `metainfo`, затем magnet/source URI, затем bare infohash fallback;
  - `lastReaddSource` показывает `metainfo`, `magnet` или `infohash`.
- Обновлен `hardRefreshTorrent`:
  - использует общий re-add helper;
  - логирует `readd_source`;
  - не запускает `watchMetadata`, если metadata уже доступна после re-add.
- Обновлен `recycleClient`:
  - snapshot включает runtime metainfo;
  - каждый torrent re-add-ится через metainfo-aware helper;
  - `watchMetadata` запускается только для torrents без metadata.
- Расширены BT Health diagnostics:
  - `metadata_ready`;
  - `last_readd_source`;
  - `auto_hard_refresh_enabled`;
  - `client_recycle_after_soft_fails`;
  - `client_recycle_min_torrent_age_sec`;
  - `recycle_scheduled_reason`.
- Обновлен Web UI `/health/bt`:
  - `Refresh` переименован в `Reload diagnostics`;
  - добавлено пояснение, что reload diagnostics не меняет torrent state;
  - `Recycle BT client` выделен как primary recovery action;
  - `Hard refresh` помечен как advanced;
  - показываются metadata readiness и last re-add source.
- Обновлены `docker-compose.yml` и `torrent-stream-hub-tz-v3.md`:
  - `HUB_BT_SWARM_AUTO_HARD_REFRESH_ENABLED=false`;
  - `HUB_BT_CLIENT_RECYCLE_COOLDOWN_SEC=300` для диагностики;
  - production recommendation: `900`.
- Обновлены tests/default assertions для новых config options.

## Статус DoD

- [x] Automatic recovery path is now soft refresh -> client recycle, not soft refresh -> per-torrent hard refresh.
- [x] Automatic per-torrent hard refresh is disabled by default.
- [x] Client recycle starts automatically after failed soft refresh when gates allow it.
- [x] Re-add prefers metainfo and does not unnecessarily lose metadata.
- [x] Manual hard refresh remains available as advanced diagnostic action.
- [x] UI clearly distinguishes reload diagnostics from recovery actions.
- [x] BT Health shows metadata readiness and last re-add source.
- [x] Active streams block destructive runtime refresh/recycle.
- [x] Diagnostics/logs do not expose peer IP/ports.
- [x] `docker-compose.yml` and `torrent-stream-hub-tz-v3.md` updated.
- [x] Created report `tasks/implementation/phase13-report.md`.
- [x] Successful checks:
  - [x] `go test ./...`
  - [x] `(cd web && npm run build)`
  - [x] `CGO_ENABLED=0 go build ./cmd/hub`

## Причины отхода от плана

- `metainfo` сохраняется runtime-only, без SQLite persistence. Это соответствует фазе и не добавляет migration risk.
- `HUB_BT_CLIENT_RECYCLE_AFTER_HARD_FAILS` оставлен для backward compatibility diagnostics/config, но automatic primary escalation теперь использует `HUB_BT_CLIENT_RECYCLE_AFTER_SOFT_FAILS`.
- Тесты добавлены на config/default behavior; end-to-end проверка actual recycle требует running swarm/Docker scenario и оставлена как ручная проверка по сценарию фазы.
