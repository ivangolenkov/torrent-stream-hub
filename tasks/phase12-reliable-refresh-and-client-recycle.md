# Фаза 12: Reliable refresh и client recycle fallback

## Цель

Исправить ситуацию, когда рестарт сервиса по-прежнему дает краткосрочный буст скорости и peers, а runtime hard refresh из Phase 11 не запускается или не дает сопоставимого эффекта.

Фаза 11 добавила trend-based degradation и per-torrent hard refresh, но текущая escalation слишком строгая: краткое восстановление сбрасывает `softRefreshCount`, `hard refresh` блокируется `min age/cooldown/soft fail count`, и в реальном flapping swarm-е он часто не достигает выполнения.

Фаза 12 должна сделать refresh deterministic, наблюдаемым и добавить более сильный fallback: in-process recycle всего `anacrolix/torrent.Client` без рестарта сервиса, без удаления файлов и без потери SQLite metadata.

## Диагноз текущего поведения

По текущим `/api/v1/health/bt` и docker logs видно:

- `hard_refresh_enabled=true`, но `hard_refresh_count=0`.
- `hard_refresh_blocked_reason` часто показывает `torrent too young` или ожидание soft attempts.
- `softRefreshCount` сбрасывается при кратком recovered state.
- Torrent может флапать между degraded/recovered, но реальная скорость остается хуже, чем после полного restart.
- Полный restart сервиса пересоздает не только torrent runtime, но и весь `torrent.Client`:
  - новый peer id;
  - новый DHT/client runtime;
  - tracker/websocket runtime state;
  - dial/backoff state;
  - peer queues/reserves.

## Принятые решения

1. Не сбрасывать escalation progress при кратком recovery.
2. Ввести degradation episode/window вместо простого `softRefreshCount`.
3. Добавить manual hard refresh action для немедленной проверки механизма.
4. Добавить client recycle как последний уровень escalation, максимально близкий к эффекту restart-а сервиса.
5. Сохранять файлы, SQLite записи, poster/title/data/category/source URI и runtime state.
6. Блокировать hard refresh/client recycle при active stream.
7. Сделать причины блокировки и `next_*_at` явно видимыми в BT Health.

## Задачи

### 1. Расширить конфигурацию escalation

Добавить ENV/flags в `internal/config/config.go`:

- `HUB_BT_SWARM_DEGRADATION_EPISODE_TTL_SEC`, default `900`.
- `HUB_BT_SWARM_RECOVERY_GRACE_SEC`, default `180`.
- `HUB_BT_SWARM_HARD_REFRESH_AFTER_SOFT_FAILS`, изменить default `2 -> 1`.
- `HUB_BT_SWARM_HARD_REFRESH_MIN_TORRENT_AGE_SEC`, изменить default `300 -> 60`.
- `HUB_BT_CLIENT_RECYCLE_ENABLED`, default `true`.
- `HUB_BT_CLIENT_RECYCLE_COOLDOWN_SEC`, default `900`.
- `HUB_BT_CLIENT_RECYCLE_AFTER_HARD_FAILS`, default `1`.
- `HUB_BT_CLIENT_RECYCLE_MIN_TORRENTS`, default `1`.
- `HUB_BT_CLIENT_RECYCLE_MAX_PER_HOUR`, default `2`.

Требования:

- Некорректные значения fallback-ятся к defaults.
- Hard refresh cooldown остается не меньше soft refresh cooldown.
- Client recycle cooldown не меньше hard refresh cooldown.
- Все новые настройки логируются на старте engine.
- Обновить `docker-compose.yml` и `torrent-stream-hub-tz-v3.md`.

### 2. Ввести degradation episode state

Расширить `ManagedTorrent` runtime-only полями:

- `degradationEpisodeStartedAt time.Time`.
- `lastDegradedAt time.Time`.
- `lastRecoveredAt time.Time`.
- `lastSoftRefreshAt time.Time`.
- `lastSoftRefreshReason string`.
- `softRefreshAttemptsInEpisode int`.
- `hardRefreshAttemptsInEpisode int`.
- `lastSoftRefreshCountResetReason string`.
- `nextHardRefreshAt time.Time`.
- `nextClientRecycleAt time.Time`.

Требования:

- Краткое recovery меньше `HUB_BT_SWARM_RECOVERY_GRACE_SEC` не сбрасывает episode.
- Episode сбрасывается только после устойчивого healthy state дольше grace или после TTL.
- `softRefreshAttemptsInEpisode` не должен сбрасываться при flapping degraded/recovered.
- Existing `softRefreshCount` можно сохранить как compatibility/display counter, но decision logic должна использовать episode counters.

### 3. Исправить hard refresh eligibility

Изменить hard refresh gating:

- hard refresh allowed после `HUB_BT_SWARM_HARD_REFRESH_AFTER_SOFT_FAILS` soft refresh attempts within episode;
- default soft fails = `1`;
- default min torrent age = `60s`;
- short recovery не сбрасывает eligibility;
- hard refresh может запускаться сразу на следующем watchdog check после failed soft refresh, если cooldown/min age прошли.

Требования:

- `hard_refresh_blocked_reason` должен объяснять точную блокировку.
- `next_hard_refresh_at` должен показывать ближайшее время, когда hard refresh может быть разрешен.
- Если hard refresh blocked только из-за active stream, причина должна быть `active stream`.
- Если blocked из-за нескольких условий, выбрать наиболее важную для пользователя:
  1. active stream;
  2. disabled;
  3. invalid state;
  4. cooldown;
  5. min age;
  6. waiting for soft attempts.

### 4. Добавить manual hard refresh action

Добавить backend action для конкретного torrent-а:

- Web API: `POST /api/v1/torrent/{hash}/action` с `action: "hard_refresh"`.
- TorrServer compatibility action не добавлять, чтобы не ломать совместимость с Lampa.
- UseCase метод: `HardRefresh(hash string) error`.
- Engine method: exported wrapper, например `HardRefresh(hash string, reason string) error`.

Требования:

- Manual hard refresh должен bypass-ить soft attempt count.
- Manual hard refresh не должен bypass-ить active stream/state safety.
- Manual hard refresh может bypass-ить min age, но не global cooldown, если это безопаснее.
- Ошибка должна возвращаться JSON в API.
- Web GUI `/health/bt` должен иметь кнопку `Hard refresh` для torrent-а.
- Кнопка disabled, если `hard_refresh_allowed=false`, кроме случая `waiting for soft refresh attempts`, который manual может bypass-ить.

### 5. Исправить per-torrent hard refresh runtime path

Проверить и доработать `hardRefreshTorrent`:

- Не удалять old runtime из map до успешной подготовки `TorrentSpec`.
- Если `AddTorrentSpec` failed после `Drop()`, не возвращать dropped torrent в active map как usable object.
- В failed case сохранить torrent visible в API через lightweight runtime placeholder или state `Error`, но не использовать dropped torrent для операций.
- После successful hard refresh:
  - reset degraded state только после фактического recovery, не сразу;
  - сохранить `hardRefreshAttemptsInEpisode`;
  - обновить `lastHardRefreshAt/reason/error`;
  - вызвать `manageResourcesLocked`.

Требования:

- Не удерживать engine lock во время `Drop()` и `AddTorrentSpec()`.
- Не удалять файлы.
- Не удалять SQLite записи.
- Source URI должен сохраняться.
- Hash mismatch должен быть невозможен для magnet/infohash, но если произошел, old state должен остаться видимым как `Error`.

### 6. Добавить client recycle fallback

Реализовать метод engine уровня:

```go
func (e *Engine) recycleClient(reason string) error
```

Поведение:

1. Под lock собрать snapshot всех managed torrents:
   - hash;
   - sourceURI/fallback infohash;
   - state;
   - error;
   - runtime counters/diagnostics;
   - metadata flags where safe.
2. Проверить safety:
   - enabled;
   - global cooldown;
   - max per hour;
   - no active streams;
   - at least `HUB_BT_CLIENT_RECYCLE_MIN_TORRENTS` torrents;
   - no torrent in delete operation.
3. Вне lock закрыть старый `torrent.Client`.
4. Создать новый client через существующий `buildClientConfig(e.cfg)`.
5. Re-add torrents через `TorrentSpec` из sourceURI/fallback infohash.
6. Пересоздать `ManagedTorrent` objects с сохраненным state/source/counters.
7. Запустить `watchMetadata` для каждого re-added torrent.
8. Вызвать `manageResourcesLocked`.

Требования:

- Client recycle не перезапускает HTTP server.
- SQLite/files/metadata сохраняются.
- Active streams блокируют recycle.
- Recycle логируется как отдельное событие без peer IP/ports.
- Если часть torrents не удалось re-add, они остаются видимыми в API как `Error` с sanitized reason.
- `StreamManager` должен быть сохранен, но recycle запрещен при active streams.

### 7. Интегрировать client recycle в watchdog escalation

Escalation flow:

1. Degraded detected -> start/continue degradation episode.
2. Soft refresh scheduled if soft cooldown passed.
3. If still degraded on next check and hard refresh allowed -> per-torrent hard refresh.
4. If after hard refresh torrent again degraded within same episode -> mark hard refresh failure.
5. If hard refresh failures exceed `HUB_BT_CLIENT_RECYCLE_AFTER_HARD_FAILS` -> schedule client recycle.

Требования:

- Client recycle должен быть global operation, не запускаться параллельно.
- Если несколько torrents degraded, recycle запускается один раз.
- Recycle blocked reason должен быть виден в BT Health.
- Не выполнять recycle чаще cooldown/max-per-hour.

### 8. Расширить BT Health diagnostics

Расширить `models.BTTorrentHealth`:

- `degradation_episode_started_at`.
- `last_degraded_at`.
- `last_recovered_at`.
- `last_soft_refresh_at`.
- `last_soft_refresh_reason`.
- `soft_refresh_attempts_in_episode`.
- `hard_refresh_attempts_in_episode`.
- `last_soft_refresh_count_reset_reason`.
- `next_hard_refresh_at`.
- `next_client_recycle_at`.

Расширить `models.BTHealth`:

- `client_recycle_enabled`.
- `client_recycle_cooldown_sec`.
- `client_recycle_after_hard_fails`.
- `client_recycle_count`.
- `client_recycle_count_last_hour`.
- `last_client_recycle_at`.
- `last_client_recycle_reason`.
- `last_client_recycle_error`.
- `client_recycle_allowed`.
- `client_recycle_blocked_reason`.
- `next_client_recycle_at`.

Требования:

- Диагностика aggregate-only, без peer IP/ports.
- Error/reason sanitized.
- Health должен явно показывать, почему hard refresh/client recycle не запущен.

### 9. Обновить Web GUI `/health/bt`

Добавить:

- `Hard refresh` button per torrent.
- Global `Recycle client` button, если allowed.
- Degradation episode start/time.
- Soft/hard attempts in episode.
- `next_hard_refresh_at`.
- Client recycle status/counters/reason/error.
- Объяснение: client recycle пересоздает BitTorrent client runtime, но не удаляет файлы и DB.

Требования:

- Сохранить светлый UI стиль.
- Кнопки должны быть disabled с понятной причиной.
- Mobile layout должен оставаться читаемым.
- Не добавлять эту диагностику на Dashboard.

### 10. Тесты

Backend tests:

- Config tests для новых ENV/defaults/clamping.
- Unit tests degradation episode:
  - flapping recovery не сбрасывает episode;
  - stable recovery сбрасывает episode;
  - episode TTL сбрасывает stale episode.
- Unit tests hard refresh gating:
  - manual bypass soft attempts;
  - active stream blocks manual and automatic;
  - next hard refresh time calculated correctly;
  - min age/cooldown reasons are stable.
- Engine tests where feasible:
  - hard refresh action calls engine path;
  - sourceURI preserved;
  - failed hard refresh records sanitized error.
- Client recycle tests:
  - blocked with active stream;
  - blocked by cooldown/max per hour;
  - re-adds torrents from sourceURI/fallback infohash;
  - records partial failures as Error state.
- API tests:
  - `POST /api/v1/torrent/{hash}/action {"action":"hard_refresh"}`;
  - `POST /api/v1/health/bt/recycle` or equivalent recycle endpoint;
  - `GET /api/v1/health/bt` returns new fields;
  - response does not expose peer IP/ports.

Frontend:

- TypeScript types updated.
- `(cd web && npm run build)` passes.

## Definition of Done

- Hard refresh запускается в flapping degraded swarm-е и не теряет eligibility из-за краткого recovery.
- Manual hard refresh доступен через Web API и `/health/bt` UI.
- Client recycle реализован как in-process fallback без рестарта HTTP server-а.
- Client recycle пересоздает `anacrolix/torrent.Client`, re-adds torrents и сохраняет files/SQLite metadata/source state.
- Active streams блокируют hard refresh и client recycle.
- BT Health показывает точную причину блокировки и `next_*_at`.
- Web GUI показывает hard refresh/client recycle controls и diagnostics.
- Diagnostics/logs не раскрывают peer IP/ports.
- `docker-compose.yml` и `torrent-stream-hub-tz-v3.md` обновлены.
- Создан отчет `tasks/implementation/phase12-report.md`.
- Успешно проходят:
  - `go test ./...`
  - `(cd web && npm run build)`
  - `CGO_ENABLED=0 go build ./cmd/hub`

## Ручной сценарий проверки

1. Запустить сервис через Docker Compose.
2. Добавить public magnet с большим swarm.
3. Дождаться initial burst после restore/add.
4. Дождаться деградации peers/speed.
5. Проверить `/health/bt`:
   - degradation episode started;
   - soft refresh attempts растут;
   - `next_hard_refresh_at` понятен;
   - reason не сбрасывается из-за краткого recovery.
6. Нажать `Hard refresh` вручную.
7. Проверить, что runtime torrent пересоздан без удаления файлов и DB.
8. Если hard refresh не помог, дождаться client recycle или нажать manual recycle.
9. Проверить, что после recycle:
   - HTTP server не перезапускался;
   - torrents остались в UI;
   - files/metadata/poster/category/source сохранились;
   - peers/speed получили burst, похожий на restart сервиса.
10. Проверить, что hard refresh/recycle заблокированы при active stream.
11. Проверить, что `/health/bt` и logs не раскрывают IP/ports peers.
