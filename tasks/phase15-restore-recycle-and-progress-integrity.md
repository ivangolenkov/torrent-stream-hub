# Фаза 15: Restore/recycle reliability и integrity прогресса

## Цель

Исправить проблему после рестарта/BT client recycle, когда torrent остается только в SQLite, но не восстанавливается в runtime engine:

- `/api/v1/torrents` показывает DB-only torrent;
- `/api/v1/health/bt` показывает `torrents: []`;
- скачивание не запускается;
- прогресс уже скачанных файлов отображается как `0` или некорректно.

Фаза должна сделать restore/recycle устойчивыми к поврежденному или неполному persisted metainfo, а SQLite progress/files не должны откатываться назад из-за временной runtime ошибки.

## Диагноз

Наблюдаемая ошибка:

```text
last_client_recycle_error:
add torrent <hash>: no piece root set for file ...
```

После этого:

- `BTHealth.torrents=[]`;
- `resource manager torrents=0`;
- SQLite продолжает содержать torrent со state `Downloading`;
- UI отображает запись из DB, но runtime download не идет.

Вероятная причина:

- Phase 14 начала сохранять runtime-generated `t.Metainfo()` после magnet metadata;
- для BEP52/v2/hybrid torrent generated metainfo может быть неполным или невалидным для повторного `AddTorrentSpec`;
- restore/recycle пробует persisted metainfo first;
- если `AddTorrentFile`/`AddTorrentSpec` падает, fallback на magnet/infohash сейчас не выполняется;
- engine остается пустым.

Отдельная проблема:

- `TorrentRepo.GetAllTorrents()` загружает только строки из `torrents`, но не загружает `files`;
- если engine пустой, UI получает DB-only torrent без file list/progress;
- `SaveTorrent()` может перезаписать `downloaded/files.downloaded` меньшими runtime значениями, включая `0`.

## Задачи

### 1. Надежный restore fallback

Изменить `TorrentUseCase.restoreTorrentToEngine`:

Priority:

1. persisted metainfo file `/config/metainfo/<hash>.torrent`;
2. persisted `source_uri` magnet;
3. bare infohash fallback.

Требования:

- Если metainfo restore failed, не возвращать ошибку сразу.
- Логировать sanitized warning и продолжать fallback.
- Если metainfo invalid, пометить его как unusable:
  - либо rename to `<hash>.torrent.invalid`;
  - либо оставить файл, но добавить runtime skip marker на текущий запуск.
- Если fallback magnet/infohash успешен, restore считается успешным.
- В `BTHealth`/logs должно быть видно, какой source реально использован.

### 2. Надежный recycle fallback

Изменить `Engine.recycleClient`/re-add logic:

- Для каждого torrent пробовать sources последовательно:
  1. runtime metainfo;
  2. persisted/source magnet;
  3. bare infohash.
- Ошибка одного source не должна сразу исключать torrent из new engine.
- Если все sources failed, сохранить placeholder/error state и записать partial recycle error.

Требования:

- Recycle не должен оставлять engine пустым, если magnet/infohash fallback возможен.
- `lastReaddSource` должен показывать фактический source:
  - `metainfo_runtime`;
  - `metainfo_file`;
  - `magnet`;
  - `infohash`.
- Active streams продолжают блокировать recycle.
- Logs не раскрывают peer IP/ports.

### 3. Валидация metainfo перед сохранением/использованием

Добавить helper:

```go
func validateMetainfoForReadd(mi *metainfo.MetaInfo) error
```

Проверять минимум:

- `mi != nil`;
- `len(mi.InfoBytes) > 0`;
- `mi.HashInfoBytes()` вычисляется;
- `mi.UnmarshalInfo()` проходит;
- если `info.HasV2()`, проверить piece layers через `metainfo.ValidatePieceLayers` where applicable.

Требования:

- `.torrent` upload сохранять только если metainfo валидный.
- Runtime-generated metainfo сохранять только если валидный.
- Если metainfo file invalid при restore, fallback to magnet/infohash.
- Не перезаписывать существующий valid metainfo runtime-generated metainfo без необходимости.

### 4. Защитить persisted progress от отката

Изменить `TorrentRepo.SaveTorrent`:

- `torrents.downloaded = max(existing.downloaded, excluded.downloaded)`;
- `files.downloaded = max(existing.downloaded, excluded.downloaded)`.

Требования:

- Progress не должен уменьшаться после restart/recycle failure.
- State можно обновлять как раньше.
- Metadata/title/poster/category/source_uri behavior сохранить.
- Если в будущем понадобится explicit reset/recheck/delete progress, это должна быть отдельная операция, не обычный sync.

### 5. Загружать files в `GetAllTorrents`

Изменить `TorrentRepo.GetAllTorrents()`:

- возвращать torrents вместе с persisted files;
- использовать общий helper, чтобы не дублировать код `GetTorrent`.

Требования:

- DB-only torrent в UI показывает file list и persisted file progress.
- Existing tests обновить/добавить.

### 6. Runtime restore state consistency

После успешного restore:

- если persisted state был `Downloading`, runtime должен попасть в `Queued/Downloading`, и `manageResourcesLocked` должен запустить `DownloadAll()` после metadata ready;
- если persisted state был `Seeding`, runtime должен оставаться активным и seed/download state должен быть корректным;
- если persisted state был `Paused`, restore не выполняется.

Требования:

- `RestoreTorrents()` не должен молча оставлять `Downloading` torrent DB-only без warning.
- Если restore полностью failed, state должен быть явно `Error` или diagnostics должны показывать restore failure.

### 7. Диагностика restore/recycle

Добавить diagnostics:

- global `last_restore_error` or per torrent `last_restore_error`;
- `last_restore_source`;
- `invalid_metainfo_count` or `metainfo_invalid_reason` if simple.

Требования:

- BT Health должен отличать:
  - torrent отсутствует в engine;
  - restore failed;
  - restore fallback used magnet/infohash;
  - recycle partial failure.

### 8. Очистка проблемного metainfo

Добавить безопасное поведение:

- если persisted metainfo file invalid, переименовать в `.invalid` или `.bad`;
- не удалять безвозвратно.

Ручная проверка для текущего кейса:

- файл `/config/metainfo/8b623d44863ee2f171ba6901d6b8f0a6ffdc8512.torrent` должен быть проигнорирован/переименован, если validation/add failed;
- restore должен продолжиться через `source_uri` magnet.

### 9. Тесты

Backend tests:

- `GetAllTorrents` returns files.
- `SaveTorrent` does not decrease torrent downloaded.
- `SaveTorrent` does not decrease file downloaded.
- Restore falls back from invalid metainfo to magnet.
- Recycle falls back from invalid runtime metainfo to magnet/infohash.
- Invalid metainfo is renamed/marked and does not block future restore.
- BT Health/API does not expose peer IP/ports.

Integration/manual:

- Start with existing DB and problematic metainfo file.
- Restart service.
- Verify `/health/bt.torrents` contains restored torrent.
- Verify `/api/v1/torrents` shows file list/progress.
- Verify download starts or at least metadata/peers are active.

## Definition of Done

- Restart no longer leaves `Downloading` torrents DB-only when magnet/infohash fallback is available.
- Bad persisted metainfo does not block restore/recycle.
- Client recycle does not leave engine empty if fallback source exists.
- Persisted torrent/file progress does not decrease during normal sync.
- `GetAllTorrents()` includes files for DB-only torrents.
- Restore/recycle diagnostics explain source/failure.
- Existing problematic metainfo is safely ignored/renamed, not deleted silently.
- Created report `tasks/implementation/phase15-report.md`.
- Successful checks:
  - `go test ./...`
  - `(cd web && npm run build)`
  - `CGO_ENABLED=0 go build ./cmd/hub`

## Ручной сценарий проверки

1. Запустить сервис с существующей SQLite DB и `/config/metainfo`.
2. Проверить `/api/v1/health/bt`:
   - `torrents` не пустой для persisted active torrents;
   - `last_restore_source` показывает `metainfo_file` или fallback source.
3. Если metainfo invalid:
   - увидеть warning в logs;
   - увидеть fallback на magnet/infohash;
   - увидеть `.invalid`/`.bad` файл рядом с исходным.
4. Проверить `/api/v1/torrents`:
   - file list присутствует;
   - downloaded не откатился вниз.
5. Перезапустить сервис еще раз.
6. Проверить, что restore снова не блокируется invalid metainfo.
7. Запустить manual `Recycle BT client`.
8. Проверить, что после recycle torrent остается в `/health/bt.torrents` и скачивание не останавливается навсегда.
