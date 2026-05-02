# Фаза 16: Non-destructive peer discovery recovery

## Цель

Исправить ошибочную модель recovery, при которой обычная деградация swarm (`low peers`, `low seeds`, `metadata pending`, `low speed`) эскалируется в destructive операции:

- per-torrent `Drop()` + re-add;
- full BitTorrent client recycle;
- потеря runtime metadata;
- повторный fallback в magnet/infohash;
- остановка рабочей загрузки.

Фаза должна заменить эту модель на безопасный peer discovery refresh без рестарта torrent/client. Restart/recycle должен остаться только ручной диагностической операцией или последним fallback при явной поломке runtime engine, а не реакцией на слабый swarm.

## Диагноз

По runtime logs текущий torrent был рабочим:

- metadata была готова;
- было `10-13` connected peers;
- было `8-10` seeds;
- скорость доходила примерно до `10 MB/s`.

Но watchdog пометил torrent degraded из-за абсолютного порога `connected < HUB_BT_SWARM_MIN_CONNECTED_PEERS` и после одного soft refresh запустил full client recycle. После recycle re-add через runtime metainfo упал с `no piece root set for file ...`, fallback ушел в magnet/infohash, и torrent потерял metadata.

Это ошибка архитектуры recovery: peer discovery refresh не должен перезапускать torrent или client.

## Принципы фазы

- Не использовать `Torrent.Drop()` для автоматического восстановления swarm.
- Не использовать full client recycle для `low peers`, `low seeds`, `metadata pending`, `download speed stalled`.
- Сначала пробовать только non-destructive операции:
  - DHT announce/get_peers;
  - tracker refresh/merge;
  - temporary connection boost;
  - `AllowDataDownload()`;
  - `DownloadAll()` после metadata;
  - optional re-add of trackers/sources через `MergeSpec`, без drop.
- Automatic recycle должен быть disabled by default.
- Manual recycle/hard refresh остаются в UI как advanced diagnostics.
- Logs и BT Health остаются aggregate-only, без peer IP/ports.

## Задачи

### 1. Пересмотреть degraded decision

Изменить `decideSwarmHealth`:

- Не считать torrent degraded только из-за `connected < min_connected_peers`, если:
  - metadata ready;
  - есть connected seeds;
  - useful/data download speed выше stalled threshold.
- Для `metadata pending` использовать отдельный grace period перед degraded:
  - torrent должен получить время на DHT/tracker metadata discovery;
  - short-term `known=0 connected=0` сразу после restore/add не должен считаться проблемой.
- Trend-based degradation по recent peak не должен срабатывать, если текущая скорость все еще достаточная для загрузки/стрима.
- Учитывать `active streams`: если stream активен и reader получает данные, не делать destructive recovery.

Требования:

- Рабочая загрузка с `10+` peers, seeds и MB/s speed не считается degraded при любом `min_connected_peers` выше фактического connected count.
- Low peers может давать warning/diagnostic, но не escalation.

### 2. Ввести non-destructive peer refresh

Выделить отдельный метод, например:

```go
func (e *Engine) refreshPeerDiscovery(hash string, mt *ManagedTorrent, reason string, now time.Time)
```

Он должен выполнять только безопасные операции:

- временно поднять `SetMaxEstablishedConns` до `BTSwarmBoostConns`;
- `AllowDataDownload()`;
- `AllowDataUpload()` если upload разрешен;
- если metadata ready и torrent incomplete: `DownloadAll()`;
- `AnnounceToDht()` для всех DHT servers;
- re-apply retrackers через `AddTrackers()` или `MergeSpec()` без `Drop()`;
- обновить diagnostics: last peer refresh time/reason/count.

Требования:

- Refresh не должен терять metadata.
- Refresh не должен менять `managedTorrents` map.
- Refresh не должен вызывать `Drop()` или создавать новый `torrent.Client`.

### 3. Отключить automatic client recycle как часть swarm recovery

Изменить escalation logic:

- `checkSwarms()` не должен ставить `recycleReason` для обычных swarm reasons.
- `BTClientRecycleEnabled` может остаться для manual endpoint, но automatic recycle должен быть отдельным флагом, например:
  - `HUB_BT_CLIENT_AUTO_RECYCLE_ENABLED=false` default.
- В `docker-compose.yml` выключить automatic recycle, если будет добавлен отдельный ENV.
- Existing manual endpoint `POST /api/v1/health/bt/recycle` сохранить.

Требования:

- Low peers/low seeds/metadata pending/stalled speed не вызывают automatic recycle.
- BT Health clearly shows automatic recycle disabled/blocked for swarm recovery.
- Manual recycle продолжает блокироваться active streams.

### 4. Перевести hard refresh в manual-only advanced diagnostic

Текущий hard refresh использует `Torrent.Drop()` + re-add. Это destructive операция.

Изменить поведение:

- automatic hard refresh остается disabled by default;
- `checkSwarms()` не должен планировать hard refresh для обычных swarm reasons, даже если flag случайно включен, без отдельного explicit unsafe flag;
- manual hard refresh оставить, но UI/health label должен явно показывать `advanced/destructive`.

Требования:

- Ни один automatic path не вызывает `hardRefreshTorrent` для обычной деградации swarm.
- Manual hard refresh требует явного пользовательского действия.

### 5. Улучшить tracker refresh без restart

Проверить текущий use of retrackers:

- `augmentTorrentSpec` применяется при add;
- при refresh нужно повторно добавить локальные retrackers через `t.AddTrackers()` или `t.MergeSpec()`.

Требования:

- Если `/config/trackers.txt` изменился, refresh может добавить новые trackers без re-add torrent.
- Duplicate trackers не должны плодиться бесконечно.
- Ошибки отдельных trackers не должны ломать torrent.

### 6. Metadata pending recovery без потери источников

Для torrent без metadata:

- не recycle;
- не hard refresh;
- периодически делать DHT announce;
- re-merge source magnet/retrackers if available;
- сохранять display name/title from DB for UI;
- показывать в BT Health `metadata_pending_duration` и last refresh reason.

Требования:

- Torrent без metadata остается тем же runtime torrent.
- UI не теряет title/poster/data из SQLite.
- `/cache` и `/stream` корректно возвращают unavailable until metadata, без запуска destructive recovery.

### 7. Runtime metainfo safety остается, но не primary recovery

Phase 15 validation оставить, но изменить роль:

- runtime metainfo не должен использоваться как причина для automatic re-add;
- persisted metainfo нужен для startup restore/manual diagnostics;
- bad runtime metainfo не должен ломать рабочий torrent, потому что auto drop/re-add больше не выполняется.

Требования:

- `no piece root set for file ...` не возникает в automatic recovery path.
- Если manual recycle/hard refresh вызывает такую ошибку, old working torrent не должен быть уничтожен без успешного replacement.

### 8. Защитить recycle replacement semantics

Если manual recycle остается:

- не заменять `e.client`/`managedTorrents` на degraded replacement, если re-add critical torrents failed;
- либо preflight build specs до закрытия old client;
- либо clearly mark partial failure and preserve enough source metadata;
- persisted metainfo file should be a candidate before runtime-generated metainfo.

Требования:

- Manual recycle не должен превращать metadata-ready torrent в metadata-less torrent, если можно избежать.
- `last_client_recycle_error` должен быть кратким и sanitized, без огромного списка всех файлов.

### 9. Piece completion diagnostics

Добавить явную диагностику storage completion backend:

- backend type: `bolt`, `sqlite`, `memory`, `unknown`;
- persistent: true/false;
- open error, если был fallback;
- path: sanitized aggregate path, без user secrets.

Требования:

- Silent fallback `storage.NewFile()` на in-memory completion должен быть обнаружим в health/logs.
- Если persistent completion не открылся, пользователь видит это в `/health/bt`.

### 10. Тесты

Backend unit tests:

- `decideSwarmHealth` не degraded для active download speed выше threshold, даже если connected peers ниже configured min.
- `decideSwarmHealth` degraded для real stalled torrent после grace/stalled duration.
- `metadata pending` не escalates to recycle before grace.
- `checkSwarms` для low peers schedules peer refresh, not recycle/hard refresh.
- automatic recycle disabled by default.
- hard refresh automatic path disabled by default/manual-only.
- tracker refresh adds trackers without drop.
- manual recycle error is sanitized/truncated.

Integration/manual:

- Запустить torrent, дождаться metadata and peers.
- Поставить `HUB_BT_SWARM_MIN_CONNECTED_PEERS` выше фактического connected peers.
- Проверить, что torrent не recycle-ится и metadata не теряется.
- Проверить, что peer refresh делает DHT/tracker refresh logs.
- Проверить `/api/v1/health/bt`:
  - no automatic recycle scheduled;
  - last peer refresh visible;
  - metadata remains ready.

## Definition of Done

- Ordinary swarm degradation no longer triggers automatic torrent drop/re-add.
- Ordinary swarm degradation no longer triggers automatic full BT client recycle.
- Peer discovery refresh is non-destructive and uses DHT/tracker/boost operations only.
- Working torrent with metadata and active speed is not classified as unhealthy solely because connected peers are below a fixed threshold.
- Metadata-ready torrent does not become metadata-less because of automatic recovery.
- Manual recycle/hard refresh remain available as advanced diagnostics and are blocked by active streams.
- BT Health explains peer refresh and recycle state without peer IP/ports.
- Piece completion backend status is visible in diagnostics.
- Created report `tasks/implementation/phase16-report.md` after implementation.
- Successful checks:
  - `go test ./...`
  - `(cd web && npm run build)`
  - `CGO_ENABLED=0 go build ./cmd/hub`

## Ручной сценарий проверки

1. Запустить сервис с существующим torrent, который уже имеет metadata.
2. Установить высокий `HUB_BT_SWARM_MIN_CONNECTED_PEERS`, например выше текущего connected count.
3. Дождаться watchdog tick.
4. Проверить logs:
   - есть peer discovery refresh;
   - нет `bt client recycle scheduled`;
   - нет `hard refreshing torrent`;
   - нет `Drop()`/re-add sequence.
5. Проверить `/api/v1/health/bt`:
   - `metadata_ready=true` остается true;
   - `last_readd_source` не меняется из-за automatic recovery;
   - `client_recycle_count` не растет;
   - visible last peer refresh diagnostics.
6. Проверить `/api/v1/torrents`:
   - size/files/progress не исчезают;
   - download speed может меняться, но torrent остается runtime-active.
7. Запустить manual recycle только отдельно и убедиться, что active streams блокируют его.
