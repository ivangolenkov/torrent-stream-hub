# Фаза 11: Hard swarm refresh и trend-based degradation

## Цель

Исправить ситуацию, когда после добавления torrent-а или рестарта сервиса скорость и количество peers кратно выше, но затем постепенно падают и не восстанавливаются. Фаза 10 добавила soft refresh, но он не повторяет главный полезный эффект рестарта: полное пересоздание torrent runtime state внутри `anacrolix/torrent` с новым запуском tracker/DHT/websocket announcers и peer acquisition.

Фаза 11 должна добавить безопасный hard refresh torrent-а без удаления файлов и без потери persisted metadata.

## Проблемы, которые должна закрыть фаза

- Connected peers/seeds продолжают постепенно снижаться при длительной работе сервиса.
- Restart сервиса по-прежнему дает временный кратный прирост скорости.
- Текущий watchdog использует слишком простые fixed thresholds и не видит деградацию относительно previous peak.
- Soft refresh (`DownloadAll`, DHT announce, connection boost) недостаточен, если tracker announcers/peer queues внутри `anacrolix/torrent` фактически застыли.
- BT Health пока не показывает peak/trend и количество soft/hard refresh attempts.

## Принятые решения

1. Добавить trend-based degradation поверх fixed thresholds из Phase 10.
2. Сохранять recent peak metrics per torrent: peers, seeds, download speed.
3. Если torrent значительно просел относительно recent peak и soft refresh не помог, выполнить hard refresh.
4. Hard refresh означает: удалить torrent из runtime `anacrolix/torrent` через `Drop()` и заново добавить его через сохраненный `SourceURI` или fallback infohash.
5. Hard refresh не должен удалять файлы, записи SQLite, poster/title/data/category и пользовательские настройки.
6. Hard refresh должен быть rate-limited и безопасен для streaming сценариев.
7. Диагностика остается aggregate-only, без IP/ports peers.

## Задачи

### 1. Расширить конфигурацию

Добавить ENV/flags в `internal/config/config.go`:

- `HUB_BT_SWARM_PEER_DROP_RATIO`, default `0.45`.
- `HUB_BT_SWARM_SEED_DROP_RATIO`, default `0.45`.
- `HUB_BT_SWARM_SPEED_DROP_RATIO`, default `0.35`.
- `HUB_BT_SWARM_PEAK_TTL_SEC`, default `1800`.
- `HUB_BT_SWARM_HARD_REFRESH_ENABLED`, default `true`.
- `HUB_BT_SWARM_HARD_REFRESH_COOLDOWN_SEC`, default `900`.
- `HUB_BT_SWARM_HARD_REFRESH_AFTER_SOFT_FAILS`, default `2`.
- `HUB_BT_SWARM_HARD_REFRESH_MIN_TORRENT_AGE_SEC`, default `300`.

Требования:

- Некорректные значения должны fallback-иться к defaults.
- Ratio значения должны clamp-иться в разумный диапазон `0.05..0.95`.
- Hard refresh cooldown должен быть не меньше soft refresh cooldown.
- Все новые настройки должны логироваться на старте engine в sanitized виде.
- Обновить `docker-compose.yml` и пример ENV в `torrent-stream-hub-tz-v3.md`.

### 2. Добавить peak/trend state в `ManagedTorrent`

Расширить runtime-only поля `ManagedTorrent`:

- `addedAt time.Time`.
- `peakConnected int`.
- `peakSeeds int`.
- `peakDownloadSpeed int64`.
- `peakUpdatedAt time.Time`.
- `softRefreshCount int`.
- `hardRefreshCount int`.
- `lastHardRefreshAt time.Time`.
- `lastHardRefreshReason string`.
- `lastHardRefreshErr string`.
- `pendingHardRefresh bool`.

Требования:

- Поля не сохраняются в SQLite.
- При hard refresh часть runtime state должна переноситься на новый `ManagedTorrent`.
- Peaks должны сбрасываться или decay-иться после `HUB_BT_SWARM_PEAK_TTL_SEC`, чтобы старый пик не делал torrent вечным degraded.

### 3. Реализовать trend-based degradation

Расширить `decideSwarmHealth` или добавить отдельную тестируемую функцию `decideSwarmTrend`.

Torrent считается degraded, если выполняется одно из условий:

- Fixed threshold из Phase 10 срабатывает.
- `connected < peakConnected * HUB_BT_SWARM_PEER_DROP_RATIO` и peak еще не истек.
- `seeds < peakSeeds * HUB_BT_SWARM_SEED_DROP_RATIO` для incomplete torrent-а и peak еще не истек.
- `download_speed < peakDownloadSpeed * HUB_BT_SWARM_SPEED_DROP_RATIO` для incomplete torrent-а и peak еще не истек.
- Soft refresh уже выполнялся `HUB_BT_SWARM_HARD_REFRESH_AFTER_SOFT_FAILS` раз, но torrent не recovered.

Требования:

- Completed seeding torrent не должен считаться degraded из-за нулевой download speed.
- Paused/Error/MissingFiles/DiskFull не участвуют в trend degradation.
- Streaming torrent должен иметь отдельную safety policy: hard refresh запрещен при активном stream, кроме случая длительного stall.

### 4. Сохранить source spec для re-add

Hard refresh должен уметь восстановить torrent без обращения к DB из engine layer.

Возможные подходы:

- Добавить в `ManagedTorrent` поле `sourceURI string` и заполнять его при add/restore.
- Для `.torrent` flow сохранять magnet URI из metainfo, как сейчас делается через `SourceURI`.
- Для bare infohash использовать `magnet:?xt=urn:btih:<hash>` и снова применять retrackers.

Требования:

- `Engine.AddMagnet`, `Engine.AddInfoHash`, `Engine.AddTorrentFile` должны сохранять source в managed runtime state.
- `TorrentUseCase.restoreTorrentToEngine` должен передавать persisted `SourceURI` так, чтобы hard refresh мог использовать тот же source.
- Если source отсутствует, fallback на infohash допустим, но должен логироваться.

### 5. Реализовать hard refresh runtime path

Добавить в `internal/engine/engine.go` метод примерно такого уровня:

```go
func (e *Engine) hardRefreshTorrent(hash string, reason string) error
```

Поведение:

1. Под lock найти `ManagedTorrent`.
2. Проверить safety constraints:
   - hard refresh enabled;
   - cooldown истек;
   - torrent age больше `HUB_BT_SWARM_HARD_REFRESH_MIN_TORRENT_AGE_SEC`;
   - state не `Paused`, `Error`, `MissingFiles`, `DiskFull`;
   - нет активного stream, либо stream явно stalled.
3. Скопировать runtime state, который нужно сохранить:
   - state;
   - error;
   - sourceURI;
   - metadata flags where safe;
   - refresh counters;
   - peak counters if still relevant.
4. Удалить старый torrent из `managedTorrents`.
5. Вне engine lock вызвать `oldTorrent.Drop()`.
6. Заново построить `TorrentSpec` из sourceURI/infohash.
7. Применить retrackers.
8. Вызвать `client.AddTorrentSpec`.
9. Зарегистрировать новый `ManagedTorrent` с перенесенным state.
10. Запустить `watchMetadata`.
11. Вызвать `manageResourcesLocked` или equivalent start logic.

Требования:

- Нельзя удалять файлы.
- Нельзя удалять запись SQLite.
- Hard refresh не должен конфликтовать с обычным delete torrent.
- Hard refresh должен корректно переживать ошибку re-add: old runtime уже может быть dropped, но DB torrent должен остаться видимым, а ошибка должна попасть в BT Health/logs.
- Hard refresh не должен удерживать engine lock во время `Drop()` и `AddTorrentSpec()`.

### 6. Интегрировать hard refresh в watchdog

Расширить `checkSwarms()`:

- Soft refresh остается первой попыткой восстановления.
- Если torrent degraded после `HUB_BT_SWARM_HARD_REFRESH_AFTER_SOFT_FAILS` soft refresh attempts, запланировать hard refresh.
- Hard refresh выполняется после выхода из lock.
- После успешного hard refresh:
  - увеличить `hardRefreshCount`;
  - обновить `lastHardRefreshAt`;
  - сохранить reason;
  - сбросить `softRefreshCount`;
  - сбросить/decay peaks.
- После ошибки hard refresh:
  - записать `lastHardRefreshErr`;
  - не спамить повтором до cooldown.

### 7. Stream safety

Добавить способ узнать, есть ли активный stream по hash:

- Метод в `StreamManager`, например `ActiveStreamsForTorrent(hash string) int`.
- Hard refresh запрещен, если active streams > 0.
- Исключение можно оставить Post-MVP: hard refresh активного stream только при длительном streaming stall.

Требования:

- Seek debounce не должен случайно разрешать hard refresh между краткими reconnect-ами player-а.
- Soft refresh может работать при active stream, hard refresh нет.

### 8. Расширить BT Health diagnostics

Расширить `models.BTTorrentHealth`:

- `peak_connected`.
- `peak_seeds`.
- `peak_download_speed`.
- `peak_updated_at`.
- `soft_refresh_count`.
- `hard_refresh_count`.
- `last_hard_refresh_at`.
- `last_hard_refresh_reason`.
- `last_hard_refresh_error`.
- `hard_refresh_allowed`.
- `hard_refresh_blocked_reason`.
- `active_streams`.

Расширить `models.BTHealth` global fields:

- `hard_refresh_enabled`.
- `hard_refresh_cooldown_sec`.
- `hard_refresh_after_soft_fails`.
- `peer_drop_ratio`.
- `seed_drop_ratio`.
- `speed_drop_ratio`.

Требования:

- JSON не должен содержать IP/ports peers.
- Ошибки должны быть sanitized.
- Поля должны быть понятны в UI.

### 9. Обновить Web GUI `/health/bt`

На странице BT Health добавить:

- Peak connected/seeds/speed.
- Current vs peak indicators.
- Soft refresh count.
- Hard refresh count.
- Last hard refresh time/reason/error.
- Badge `Hard refresh blocked` с причиной, если active stream/cooldown/state блокируют refresh.
- Подсказку: hard refresh пересоздает torrent runtime state, но не удаляет файлы и DB.

Требования:

- Сохранить светлый UI стиль.
- Таблица должна оставаться читаемой на mobile.
- Не возвращать BT Health на Dashboard.

### 10. Тесты

Backend tests:

- Config tests для новых ENV/defaults/clamping.
- Unit tests trend degradation:
  - peers dropped below ratio;
  - seeds dropped below ratio;
  - speed dropped below ratio;
  - expired peak не вызывает degradation;
  - completed seeding ignores download speed ratio;
  - paused/error/diskfull ignored.
- Unit tests hard refresh gating:
  - disabled hard refresh;
  - cooldown not elapsed;
  - torrent too young;
  - active stream blocks hard refresh;
  - soft fail count below threshold.
- Engine tests where possible:
  - sourceURI stored in managed state;
  - hard refresh preserves state/source counters on success;
  - hard refresh records error on re-add failure.
- API test:
  - `GET /api/v1/health/bt` returns new fields;
  - response does not expose peer IP/ports.

Frontend:

- TypeScript types updated.
- `(cd web && npm run build)` passes.

## Definition of Done

- Trend-based degradation detects meaningful peer/seed/speed decay relative to recent peak.
- Soft refresh remains first recovery step.
- Hard refresh safely drops and re-adds runtime torrent without deleting files or SQLite records.
- Hard refresh is rate-limited by cooldown, torrent age and soft fail count.
- Hard refresh is blocked while stream is active.
- Source URI / fallback infohash is available for re-add.
- BT Health exposes peak/trend/soft/hard refresh diagnostics.
- Web GUI shows hard refresh diagnostics on `/health/bt`.
- Diagnostics and logs do not expose peer IP/ports.
- `docker-compose.yml` and `torrent-stream-hub-tz-v3.md` include new settings.
- Added/updated tests cover config, trend decisions, hard refresh gating and API fields.
- Успешно проходят:
  - `go test ./...`
  - `(cd web && npm run build)`
  - `CGO_ENABLED=0 go build ./cmd/hub`
- Создан отчет `tasks/implementation/phase11-report.md`.

## Команды ручной проверки

```bash
curl -s http://localhost:8080/api/v1/health/bt | jq

curl -s http://localhost:8080/api/v1/torrents | jq '.[] | {hash, name, state, download_speed, upload_speed, peer_summary}'

docker compose logs -f torrent-hub
```

## Ручной сценарий проверки

1. Запустить сервис через Docker Compose без ручного port-forward.
2. Добавить public magnet с большим swarm.
3. Зафиксировать initial peaks в `/health/bt`.
4. Оставить сервис работать до падения peers/speed.
5. Проверить, что trend degradation срабатывает до критически низких fixed thresholds.
6. Проверить soft refresh attempts.
7. Если soft refresh не помог, проверить hard refresh:
   - torrent runtime пересоздан;
   - файлы остались на диске;
   - запись в SQLite осталась;
   - poster/title/data/category сохранились;
   - peers/speed получили новый burst без рестарта сервиса.
8. Проверить, что hard refresh не запускается при активном streaming playback.
9. Проверить, что `/health/bt` не раскрывает IP/ports peers.
