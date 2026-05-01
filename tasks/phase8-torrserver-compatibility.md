# Фаза 8: Укрепление слоя совместимости TorrServer

## Цель

Максимально приблизить слой совместимости TorrServer к оригинальному API и поведению, необходимому для Lampa и похожих клиентов, не превращая основной сервис в TorrServer-клон.

Основная модель проекта остается прежней: сервис является torrent-клиентом, который скачивает торренты и сохраняет файлы на диск. Слой TorrServer должен оставаться изолированным compatibility layer поверх существующей логики. Нельзя вводить долгоживущий RAM cache как основное хранилище данных.

## Принятые решения

1. Preload реализуется как disk-backed read-ahead/priority warmup без попытки полностью повторить RAM cache TorrServer.
2. `POST /settings` с `action=set` и `action=def` реализуется как no-op compatibility response и не меняет реальные настройки engine.
3. TorrServer metadata `category` и `poster` должны сохраняться в SQLite и переживать restart.
4. Poster должен отображаться в Web GUI torrent-клиента, если он задан.
5. Web GUI должен позволять опционально задать `poster` при добавлении torrent-а.
6. `category` пока не нужно показывать или редактировать в Web GUI; достаточно сохранять и возвращать ее через TorrServer API.

## Задачи

### 1. TorrServer-compatible DTO

- Ввести отдельные request/response structs только в `internal/delivery/http/torrserver`.
- Не отдавать внутренний `models.Torrent` напрямую из TorrServer endpoints.
- Приблизить status response к оригинальному `state.TorrentStatus` TorrServer:
  - `title`
  - `category`
  - `poster`
  - `data`
  - `timestamp`
  - `name`
  - `hash`
  - `stat`
  - `stat_string`
  - `loaded_size`
  - `torrent_size`
  - `preloaded_bytes`
  - `preload_size`
  - `download_speed`
  - `upload_speed`
  - `total_peers`
  - `pending_peers`
  - `active_peers`
  - `connected_seeders`
  - `half_open_peers`
  - `file_stats`
- `file_stats` должны иметь TorrServer-compatible поля:
  - `id`
  - `path`
  - `length`

### 2. File ID compatibility

- Для TorrServer API использовать 1-based `file_stats[].id`, как в оригинальном TorrServer.
- Внутри сервиса оставить 0-based `models.File.Index`.
- На входе `/stream`, `/play`, `/cache` переводить TorrServer id в internal index: `internalIndex = torrserverID - 1`.
- Не менять основной Web/API contract `/api/v1`, если это не требуется для GUI.

### 3. `/torrents` compatibility

- Поддержать действия:
  - `add`
  - `get`
  - `set`
  - `rem`
  - `list`
  - `drop`
  - `wipe`
- `add` должен принимать и сохранять metadata:
  - `title`
  - `poster`
  - `data`
  - `category`
  - `save_to_db`
- `set` должен обновлять TorrServer metadata без изменения основной torrent-логики.
- `get`, `list`, `add` должны возвращать TorrServer-compatible status.
- `rem`, `drop`, `wipe` должны вести себя максимально близко к TorrServer в рамках текущей архитектуры.

### 4. `/settings` compatibility

- `POST /settings` должен читать JSON request с `action`.
- `action=get` должен возвращать TorrServer-like settings object.
- Минимальные поля:
  - `CacheSize`
  - `ReaderReadAHead`
  - `PreloadCache`
  - `TorrentDisconnectTimeout`
  - `ResponsiveMode`
- `action=set` и `action=def` должны возвращать успешный no-op response.
- Не позволять Lampa менять реальные настройки engine через TorrServer compatibility layer.

### 5. `/cache` compatibility

- Добавить `POST /cache`.
- Поддержать body JSON даже при `Content-Type: application/x-www-form-urlencoded`, как это делает Lampa.
- `action=get` должен возвращать TorrServer-like cache state:
  - `Hash`
  - `Capacity`
  - `Filled`
  - `PiecesLength`
  - `PiecesCount`
  - `Torrent`
  - `Pieces`
  - `Readers`
- Если cache state недоступен, вернуть `{}`, как оригинальный TorrServer.
- Реализация должна быть derived view поверх disk-backed torrent state и текущих piece states, а не долгоживущий RAM cache.

### 6. `/stream` compatibility

- Поддержать `GET` и `HEAD`:
  - `/stream`
  - `/stream/{fname}`
- `link` должен принимать hash/magnet, насколько возможно в текущей архитектуре.
- `hash` можно оставить как дополнительный shortcut для нашего GUI.
- `stat` должен возвращать полный TorrServer-compatible status.
- `m3u` должен возвращать playlist в стиле TorrServer.
- `play` должен запускать реальный stream.
- `preload` должен инициировать disk-backed read-ahead/priority warmup без долгоживущего RAM cache.
- Для `preload/stat` не возвращать fake full-size progress, если данные фактически не доступны.

### 7. Preload behavior

- Реализовать compatibility preload как disk-backed priority/read-ahead:
  - повысить приоритет выбранного файла;
  - создать краткоживущий torrent reader;
  - прочитать небольшой стартовый диапазон файла;
  - при необходимости отдельно прогреть хвост файла для контейнеров, которым это полезно;
  - закрывать reader после warmup;
  - хранить только runtime progress counters.
- Не хранить крупные данные в памяти.
- `preloaded_bytes` должен отражать фактическую доступность/готовность, а не всегда равняться размеру файла.

### 8. Streaming headers

- Сверить headers с оригинальным TorrServer и добавить недостающие, если это не ломает `http.ServeContent`:
  - `Accept-Ranges`
  - `Content-Range`
  - `Content-Type`
  - `ETag`
  - `Connection`
  - `transferMode.dlna.org`
  - `contentFeatures.dlna.org` при DLNA request headers.
- Reader должен закрываться при завершении request context.
- Не ломать range requests и disk-backed storage behavior.

### 9. TorrServer metadata persistence

- Добавить в доменную модель torrent поля:
  - `Poster`
  - `Category`
  - при необходимости `Title`/compat title отдельно от `Name`, если текущего `Name` недостаточно.
- Добавить SQLite migration, например `0003_add_torrent_metadata.sql`.
- Поля должны быть совместимы с существующими DB: `TEXT NOT NULL DEFAULT ''` или nullable.
- Repository должен сохранять `poster` и `category`.
- Runtime sync не должен затирать непустые persisted metadata пустыми runtime значениями, аналогично `source_uri`.
- `RestoreTorrents()` должен сохранять metadata при восстановлении.

### 10. Web GUI poster support

- Расширить основной API добавления torrent-а, чтобы он принимал optional `poster`.
- В форме добавления torrent через Web GUI добавить optional поле `Poster URL`.
- Показывать poster в Web GUI, если он задан:
  - в таблице или карточке torrent-а;
  - в inspector header или рядом с деталями torrent-а.
- Если poster отсутствует, оставить текущий fallback UI.
- `category` пока не показывать и не редактировать в Web GUI.

### 11. Тесты

- Repository:
  - `poster` и `category` сохраняются;
  - `poster` и `category` не затираются пустыми runtime updates;
  - metadata переживает reload из SQLite.
- TorrServer API:
  - `/torrents action=get/list/add` возвращает TorrServer-compatible status;
  - `file_stats[].id` является 1-based;
  - `/torrents action=set` обновляет `poster/category/data/title`;
  - `/settings action=get` содержит `CacheSize` и другие минимальные поля;
  - `/settings action=set/def` возвращает успешный no-op response;
  - `/cache` принимает JSON body с `Content-Type: application/x-www-form-urlencoded`;
  - `/stream/{fname}?link=<hash>&index=1&stat` возвращает TorrServer-compatible status;
  - `/stream/{fname}?link=<hash>&index=1&play` мапит `index=1` в internal file 0;
  - `/play/{hash}/1` мапит id в internal file 0.
- CORS:
  - preflight для TorrServer endpoints проходит с wildcard origin.
- Frontend:
  - TypeScript build проходит после добавления `poster`.

## Definition of Done

- Lampa открывает список файлов torrent-а без JS ошибок.
- Lampa получает TorrServer-compatible 1-based file ids и успешно строит playback URLs.
- `/cache` существует и возвращает совместимый JSON.
- `/stream?...&stat` возвращает TorrServer-like torrent status.
- `/stream?...&play` возвращает valid range stream для 1-based TorrServer file id.
- `/settings action=get` возвращает TorrServer-like settings object.
- `/settings action=set/def` не меняет реальные настройки engine и отвечает успешно.
- Poster/category сохраняются в SQLite и возвращаются через TorrServer API.
- Poster можно опционально задать через Web GUI при добавлении torrent-а.
- Poster отображается в Web GUI, если он задан.
- Compatibility layer остается изолированным в TorrServer delivery/usecase glue.
- Не введен долгоживущий RAM cache для torrent data.
- Основная логика torrent-клиента остается disk-backed download/seeding.
- Добавлены или обновлены релевантные тесты.
- Успешно проходят:
  - `go test ./...`
  - `(cd web && npm run build)`
  - `CGO_ENABLED=0 go build ./cmd/hub`
- Создан отчет `tasks/implementation/phase8-report.md`.

## Команды ручной проверки

```bash
curl -i -X POST http://localhost:8080/settings \
  -H 'Content-Type: application/json' \
  --data-raw '{"action":"get"}'

curl -i -X POST http://localhost:8080/torrents \
  -H 'Content-Type: application/json' \
  --data-raw '{"action":"get","hash":"<hash>"}'

curl -i -X POST http://localhost:8080/cache \
  -H 'Content-Type: application/x-www-form-urlencoded; charset=UTF-8' \
  --data-raw '{"action":"get","hash":"<hash>"}'

curl -i 'http://localhost:8080/stream/file.avi?link=<hash>&index=1&stat'

curl -i -H 'Range: bytes=0-0' \
  'http://localhost:8080/stream/file.avi?link=<hash>&index=1&play'
```

Также необходимо проверить Lampa с включенным и выключенным `torrserver_preload`.
