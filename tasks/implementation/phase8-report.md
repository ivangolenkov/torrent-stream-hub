# Отчет по фазе 8: Укрепление слоя совместимости TorrServer

## Что было сделано

- Добавлен изолированный TorrServer-compatible response shape для `/torrents`, `/stream?...&stat` и связанных ответов.
- Внешние TorrServer file ids переведены на 1-based формат, при этом внутренняя модель остается 0-based.
- Добавлен `POST /cache`, включая поддержку JSON body при `Content-Type: application/x-www-form-urlencoded`.
- Расширен `POST /settings`: `action=get` возвращает TorrServer-like настройки, `action=set` и `action=def` отвечают успешным no-op.
- Добавлены `HEAD` маршруты для `/stream`, `/stream/{name}` и `/play/{hash}/{id}`.
- `preload` реализован как disk-backed warmup: повышается приоритет файла, создается короткоживущий reader и читается небольшой стартовый диапазон без долгоживущего RAM cache.
- Добавлены TorrServer metadata поля `title`, `data`, `poster`, `category` в модель, SQLite и repository.
- Добавлена миграция `0003_add_torrent_metadata.sql`.
- Runtime sync/restore не затирает непустые persisted metadata пустыми runtime значениями.
- Основной Web API `/api/v1/torrent/add` принимает optional `poster`.
- Web GUI позволяет задать optional `Poster URL` при добавлении magnet/hash.
- Web GUI показывает poster в таблице torrent-ов и в inspector header, если он задан.
- Добавлены и обновлены тесты repository, API и TorrServer compatibility layer.

## Измененные файлы

- `internal/models/torrent.go`
- `internal/repository/migrations/0003_add_torrent_metadata.sql`
- `internal/repository/torrent_repo.go`
- `internal/repository/torrent_repo_test.go`
- `internal/usecase/torrent_uc.go`
- `internal/engine/engine.go`
- `internal/delivery/http/api/handlers.go`
- `internal/delivery/http/api/handlers_test.go`
- `internal/delivery/http/torrserver/handlers.go`
- `internal/delivery/http/torrserver/handlers_test.go`
- `web/src/api/client.ts`
- `web/src/types/index.ts`
- `web/src/components/AddTorrentModal.vue`
- `web/src/components/TorrentTable.vue`
- `web/src/components/TorrentInspector.vue`

## Статус выполнения DoD

- [x] Lampa получает TorrServer-compatible 1-based file ids и может строить playback URLs.
- [x] `/cache` существует и возвращает совместимый JSON или `{}` при недоступном runtime cache state.
- [x] `/stream?...&stat` возвращает TorrServer-like torrent status.
- [x] `/stream?...&play` использует valid range stream и поддерживает 1-based TorrServer file id при `link` query.
- [x] `/play/{hash}/1` мапит id в internal file 0.
- [x] `/settings action=get` возвращает TorrServer-like settings object.
- [x] `/settings action=set/def` не меняет реальные настройки engine и отвечает успешно.
- [x] Poster/category сохраняются в SQLite и возвращаются через TorrServer API.
- [x] Poster можно опционально задать через Web GUI при добавлении torrent-а.
- [x] Poster отображается в Web GUI, если он задан.
- [x] Compatibility layer остается изолированным в TorrServer delivery/usecase glue.
- [x] Не введен долгоживущий RAM cache для torrent data.
- [x] Основная логика torrent-клиента остается disk-backed download/seeding.
- [x] Добавлены или обновлены релевантные тесты.
- [x] `go test ./...` проходит.
- [x] `(cd web && npm run build)` проходит.
- [x] `CGO_ENABLED=0 go build ./cmd/hub` проходит.

## Причины отхода от плана

- Поле `category` пока только сохраняется и возвращается через TorrServer/API-модель, без UI-редактирования, как было согласовано.
- `/cache` реализован как compatibility view поверх текущего disk-backed состояния. Если torrent runtime state недоступен или metadata еще не готова, endpoint возвращает `{}`, что соответствует совместимому fallback поведению.
- `preloaded_bytes` не имитирует полный RAM cache TorrServer и отражает фактический downloaded/warmup progress, чтобы не показывать ложную готовность данных.

## Проверки

- `go test ./...`
- `(cd web && npm run build)`
- `CGO_ENABLED=0 go build ./cmd/hub`
