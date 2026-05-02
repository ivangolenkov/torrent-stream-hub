# Фаза 13: Client recycle как основной recovery и metainfo-preserving re-add

## Цель

Исправить ситуацию, когда рестарт сервиса дает краткосрочный буст скорости/peers, а runtime refresh в приложении не дает сопоставимого эффекта.

По итогам Phase 12 выяснилось:

- per-torrent hard refresh действительно запускается;
- после `Drop()` + `AddTorrentSpec(magnet)` torrent часто теряет metadata и уходит в `metadata_ready=false`;
- в деградировавшем swarm-е повторное metadata discovery может не восстановиться быстро;
- client recycle был реализован, но не стал основным automatic recovery path;
- кнопка `Refresh` в BT Health только перечитывает diagnostics и не запускает recovery.

Фаза 13 должна сделать client recycle основным recovery mechanism после failed soft refresh, а per-torrent hard refresh оставить диагностическим/ручным действием или использовать только при наличии metainfo.

## Диагноз по логам

Наблюдаемое поведение:

- После полного restart сервиса:
  - metadata быстро готова;
  - появляются tracker peers;
  - скорость временно растет.
- После per-torrent hard refresh:
  - логируется `hard refreshing torrent`;
  - старый torrent закрывается;
  - новый torrent добавляется из magnet;
  - `metadata_ready=false`;
  - peers/скорость не восстанавливаются.
- Client recycle не запускается автоматически, хотя `client_recycle_allowed=true`.

Вывод: per-torrent hard refresh через magnet не эквивалентен restart-у и может ухудшать состояние, потому что теряется metadata. Основной fallback должен пересоздавать весь `anacrolix/torrent.Client` и re-add torrents без потери metadata, если metadata уже была получена.

## Принятые решения

1. Automatic per-torrent hard refresh отключить по умолчанию как recovery path.
2. Manual hard refresh оставить как diagnostic/debug action.
3. Client recycle сделать основным automatic recovery после failed soft refresh.
4. Re-add при hard refresh/recycle должен использовать metainfo, если она уже доступна.
5. Magnet/infohash fallback использовать только если metainfo недоступна.
6. UI должен явно различать:
   - reload diagnostics;
   - hard refresh torrent;
   - recycle BT client.

## Задачи

### 1. Изменить escalation strategy

Текущий flow Phase 12:

1. degraded;
2. soft refresh;
3. hard refresh;
4. client recycle after hard refresh failure.

Новый flow:

1. degraded detected -> start/continue degradation episode;
2. soft refresh scheduled once;
3. if still degraded on next watchdog tick -> schedule client recycle;
4. per-torrent hard refresh не запускается автоматически, если явно не включен.

Добавить config:

- `HUB_BT_SWARM_AUTO_HARD_REFRESH_ENABLED`, default `false`.
- `HUB_BT_CLIENT_RECYCLE_AFTER_SOFT_FAILS`, default `1`.
- `HUB_BT_CLIENT_RECYCLE_MIN_TORRENT_AGE_SEC`, default `60`.

Требования:

- Existing `HUB_BT_SWARM_HARD_REFRESH_ENABLED` продолжает управлять manual hard refresh availability.
- Automatic hard refresh может быть включен только через `HUB_BT_SWARM_AUTO_HARD_REFRESH_ENABLED=true`.
- Client recycle должен запускаться после failed soft refresh без ожидания hard refresh.
- Active stream блокирует client recycle.
- Cooldown/max-per-hour остаются обязательными.

### 2. Сохранить metainfo в runtime state

Расширить `ManagedTorrent` runtime-only полями:

- `metainfoBytes []byte` или `metainfo *metainfo.MetaInfo`;
- `lastReaddSource string` with values: `metainfo`, `magnet`, `infohash`;
- `metadataReady bool` для diagnostics.

Требования:

- Когда `watchMetadata` получает info, сохранить metainfo/source representation, если `anacrolix/torrent` позволяет получить metainfo публичным API.
- Если полного `.torrent` metainfo получить нельзя, сохранить хотя бы info bytes/Info dictionary и trackers from current spec/source where possible.
- Поля runtime-only, в SQLite не сохранять в этой фазе.
- Не хранить peer IP/ports.

### 3. Re-add через metainfo, если возможно

Добавить helper:

```go
func (e *Engine) torrentSpecForReadd(mt *ManagedTorrent, hash string) (*torrent.TorrentSpec, string, error)
```

Priority:

1. metainfo from runtime -> `TorrentSpecFromMetaInfoErr` -> source `metainfo`;
2. sourceURI magnet -> `TorrentSpecFromMagnetUri` -> source `magnet`;
3. fallback infohash magnet -> source `infohash`.

Требования:

- `augmentTorrentSpec` применяется во всех случаях.
- `lastReaddSource` обновляется после hard refresh/client recycle.
- Если re-add из metainfo возможен, metadata не должна уходить в pending state.
- Если re-add из magnet, diagnostics должны явно показывать `last_readd_source=magnet`.

### 4. Переделать client recycle на metainfo-preserving re-add

Обновить `recycleClient`:

- snapshot должен включать metainfo runtime data;
- re-add каждого torrent через `torrentSpecForReadd`;
- после recycle `watchMetadata` запускается только если metadata еще не готова;
- если metainfo source использован, `metadataLogged` и `downloadAllStarted` должны быть восстановлены безопасно.

Требования:

- HTTP server не перезапускается.
- Files/SQLite metadata/sourceURI сохраняются.
- Poster/title/data/category сохраняются через существующий repository/usecase merge.
- State `Downloading/Queued/Seeding/Paused` переносится корректно.
- Paused torrents не должны начать скачивание после recycle.
- Client recycle логируется без peer IP/ports.

### 5. Ограничить automatic per-torrent hard refresh

Изменить watchdog:

- если `HUB_BT_SWARM_AUTO_HARD_REFRESH_ENABLED=false`, не schedule hard refresh автоматически;
- manual API action `hard_refresh` остается;
- если auto hard refresh enabled, использовать только metainfo-preserving re-add;
- если metadata unavailable и source только magnet, лучше schedule client recycle вместо hard refresh.

Требования:

- Default path для degraded torrent: soft refresh -> client recycle.
- Per-torrent hard refresh не должен ухудшать metadata state в default config.

### 6. Добавить explicit manual recycle action в UI

В `/health/bt`:

- Переименовать кнопку `Refresh` в `Reload diagnostics`.
- Добавить текст: `Reload diagnostics does not change torrent state`.
- Кнопку `Recycle client` сделать визуально primary recovery action.
- Кнопку `Hard refresh` пометить как diagnostic/advanced.
- Добавить warning, если `last_readd_source=magnet`: metadata может быть заново requested from swarm.

Требования:

- UI должен ясно показывать, что именно запускает recovery.
- Mobile layout остается читаемым.
- Не добавлять эти controls на Dashboard.

### 7. Расширить BT Health diagnostics

Расширить `models.BTTorrentHealth`:

- `metadata_ready bool`.
- `last_readd_source string`.
- `auto_hard_refresh_enabled bool`.
- `client_recycle_after_soft_fails int`.
- `client_recycle_min_torrent_age_sec int`.
- `recycle_scheduled_reason string`.
- `last_recycle_generation int` or similar counter if useful.

Расширить `models.BTHealth`:

- `auto_hard_refresh_enabled`.
- `client_recycle_after_soft_fails`.
- `client_recycle_min_torrent_age_sec`.
- `recycle_scheduled_reason`.

Требования:

- Health должен объяснять, почему automatic recycle не запущен.
- Health должен показывать, какой source использовался при последнем re-add.
- Diagnostics/logs не раскрывают peer IP/ports.

### 8. Уменьшить cooldown для проверки в compose

Для practical feedback loop обновить `docker-compose.yml` defaults:

- `HUB_BT_CLIENT_RECYCLE_COOLDOWN_SEC=300`.
- `HUB_BT_CLIENT_RECYCLE_AFTER_SOFT_FAILS=1`.
- `HUB_BT_CLIENT_RECYCLE_MIN_TORRENT_AGE_SEC=60`.
- `HUB_BT_SWARM_AUTO_HARD_REFRESH_ENABLED=false`.

В `torrent-stream-hub-tz-v3.md` указать production-safe recommendation:

- cooldown `900` для стабильного окружения;
- cooldown `300` для диагностики/домашнего usage.

### 9. Тесты

Backend tests:

- Config tests:
  - auto hard refresh default false;
  - recycle after soft fails default 1;
  - min torrent age default 60;
  - recycle cooldown constraints.
- Escalation tests:
  - degraded -> soft refresh;
  - still degraded after soft refresh -> client recycle scheduled;
  - auto hard refresh disabled prevents automatic hard refresh;
  - active stream blocks recycle;
  - cooldown blocks recycle.
- Re-add source tests:
  - metainfo source preferred when available;
  - magnet fallback when metainfo unavailable;
  - infohash fallback when sourceURI empty;
  - `lastReaddSource` updated.
- API tests:
  - manual recycle endpoint works;
  - hard refresh action still exists;
  - BT Health returns new diagnostics;
  - response does not expose peer IP/ports.

Frontend:

- TypeScript types updated.
- `(cd web && npm run build)` passes.

## Definition of Done

- Automatic recovery path is now soft refresh -> client recycle, not soft refresh -> per-torrent hard refresh.
- Automatic per-torrent hard refresh is disabled by default.
- Client recycle starts automatically after failed soft refresh when gates allow it.
- Re-add prefers metainfo and does not unnecessarily lose metadata.
- Manual hard refresh remains available as advanced diagnostic action.
- UI clearly distinguishes reload diagnostics from recovery actions.
- BT Health shows metadata readiness and last re-add source.
- Active streams block destructive runtime refresh/recycle.
- Diagnostics/logs do not expose peer IP/ports.
- `docker-compose.yml` and `torrent-stream-hub-tz-v3.md` updated.
- Created report `tasks/implementation/phase13-report.md`.
- Successful checks:
  - `go test ./...`
  - `(cd web && npm run build)`
  - `CGO_ENABLED=0 go build ./cmd/hub`

## Ручной сценарий проверки

1. Запустить сервис через Docker Compose.
2. Добавить public magnet с большим swarm.
3. Дождаться initial burst.
4. Дождаться деградации speed/peers.
5. Проверить `/health/bt`:
   - `metadata_ready=true`;
   - soft refresh attempts increase;
   - automatic hard refresh disabled;
   - client recycle scheduled/allowed reason visible.
6. Дождаться automatic client recycle или нажать `Recycle client`.
7. Проверить, что HTTP server не перезапускался.
8. Проверить, что torrent остался в UI, файлы/metadata/poster/category сохранились.
9. Проверить `last_readd_source`:
   - желательно `metainfo`;
   - если `magnet`, metadata может быть pending.
10. Сравнить peers/speed с эффектом полного restart-а.
11. Проверить, что active stream блокирует recycle.
