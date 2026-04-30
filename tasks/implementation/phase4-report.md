# Отчет по реализации: Фаза 4 (HTTP API)

## Что было сделано:
1. **Выбор фреймворка**: В качестве маршрутизатора установлен `go-chi/chi/v5` и `go-chi/cors`, так как они легкие и полностью совместимы со стандартной библиотекой `net/http`.
2. **Слой UseCase (`internal/usecase/torrent_uc.go`)**: Создан бизнес-слой `TorrentUseCase`, объединяющий в себе логику `engine` и `repository`.
3. **Фоновый воркер синхронизации и SSE (`internal/usecase/sync_worker.go`)**:
   - Реализован `SyncWorker`, который каждые 2 секунды забирает состояние активных загрузок из движка и синхронизирует его с базой данных SQLite.
   - Метод рассылает объединенное состояние в формате JSON всем подписанным SSE-клиентам (Web GUI).
4. **REST API для Web GUI (`internal/delivery/http/api/handlers.go`)**:
   - Реализованы эндпоинты `/api/v1/torrents`, `/api/v1/torrent/add`, `/api/v1/torrent/{hash}/action` (с поддержкой pause, resume, delete).
   - Создан обработчик `SSEEvents` (`/api/v1/events`) для поддержания `text/event-stream` соединения.
5. **TorrServer Layer (`internal/delivery/http/torrserver/handlers.go`)**:
   - Реализованы обязательные для ТВ-плееров эндпоинты-заглушки: `/echo`, `/settings`, `/viewed`, возвращающие корректные `status: ok` ответы.
   - Реализован `/torrents` для получения списка и добавления (magnet-ссылок).
   - Реализован генератор `/playlist` для отдачи плейлистов `M3U` с абсолютными ссылками на медиафайлы торрента.
6. **HTTP-стриминг с поддержкой Range**:
   - Внедрены эндпоинты `/stream` и алиас `/play/{hash}/{id}`.
   - Взаимодействие со стриминг-движком: при входе в хендлер вызывается `uc.AddStream(r.Context(), ...)`, что активирует режим эгоиста в движке.
   - Использован стандартный `http.ServeContent` поверх `torrent.File.NewReader()`, что автоматически и корректно обеспечивает HTTP-ответы `206 Partial Content` (поддержка перемотки).
7. **CORS**: В корневом `router.go` настроены правила CORS (`AllowedOrigins: ["*"]`), разрешающие любые источники для возможности загрузки видео прямо в ТВ-приложения из локальной сети.
8. **Важный архитектурный фикс (Отклонение/Исправление)**: Во время тестов выявлен конфликт CGO для `mattn/go-sqlite3` при совместном использовании с транзитивной зависимостью торрент-клиента `zombiezen/go/sqlite`. Проблема решена заменой `mattn/go-sqlite3` на pure-go драйвер `modernc.org/sqlite`. Тесты успешно проходят.

## Статус выполнения DoD
- [x] Реализованы все эндпоинты REST API для Web GUI согласно спецификации OpenAPI.
- [x] Работает эндпоинт SSE (`/api/v1/events`), транслирующий актуальное состояние торрентов.
- [x] Реализованы эндпоинты совместимости с TorrServer (`/echo`, `/torrents`, `/settings`, `/viewed`, `/playlist`).
- [x] Реализован эндпоинт потоковой передачи видео (`/stream` и `/play`) с корректной обработкой `Range` запросов (206 Partial Content).
- [x] CORS настроен на разрешение запросов с любых источников (`*`).

Фаза полностью завершена. Все тесты (`go test ./...`) успешно пройдены.

## Дополнение: исправление добавления торрентов
1. `/api/v1/torrent/add` теперь возвращает `202 Accepted`, так как добавление magnet-ссылки регистрируется сразу, без блокировки HTTP-запроса на ожидании metadata от пиров.
2. Реализован endpoint `/torrent/upload` для загрузки `.torrent` файлов через `multipart/form-data`.

## Дополнение: исправление action и JSON-ошибок
1. Исправлен сценарий, когда torrent есть в SQLite/UI, но отсутствует в in-memory engine: `pause` больше не возвращает `500 torrent not found`, а корректно переводит запись в `Paused`.
2. `resume` для DB-only торрента восстанавливает torrent в engine по info hash и переводит его обратно в очередь запуска.
3. Очередь engine теперь запускается сразу при добавлении/возобновлении, а не только на следующем тике фонового monitor; magnet без metadata переводится в `Downloading` без вызова `DownloadAll()` до получения metadata, чтобы не было panic.
4. REST API и TorrServer-compatible handlers теперь возвращают ошибки в JSON-формате `{"error":"..."}` вместо plain text `http.Error`.
5. Добавлены тесты на JSON-ошибки API/TorrServer и на action-поведение для DB-only торрентов.

## Дополнение: исправление panic при pause и восстановления после рестарта
1. Исправлен panic при `pause` для magnet без metadata: engine больше не вызывает `Torrent.Files()` до появления `Torrent.Info()`.
2. Такие же guards добавлены в streaming/cache/resource-monitor paths, где file list доступен только после metadata.
3. В SQLite добавлена миграция `0002_add_torrent_source_uri.sql`; новые magnet/.torrent добавления сохраняют исходный source URI, включая tracker-ы.
4. Restore после рестарта теперь использует сохраненный source URI, а не только bare info hash, поэтому metadata/download после restart стартуют надежнее для новых записей.
5. HTTP panic recovery заменен на JSON recovery: неожиданные panic теперь возвращают `{"error":"internal server error"}`.
6. Добавлены regression-тесты на pause magnet без metadata и сохранение source URI.
