# Предложения по улучшению сервиса

Этот файл фиксирует список возможных улучшений для `Torrent-Stream-Hub`. Это backlog/черновик будущих фаз, а не план к немедленной реализации.

## P0: Сначала починить

- Убрать рассинхрон `BT Health`: после phase24 `HardRefresh` и `RecycleClient` удалены, но `web/src/components/BTHealthPanel.vue` и `web/src/api/client.ts` все еще показывают/вызывают `hard_refresh` и `/api/v1/health/bt/recycle`.
- Реализовать или убрать `recheck`: в `internal/delivery/http/api/handlers.go` action уже доступен, но обработчик пока не реализован.
- Реально применить `HUB_MAX_ACTIVE_STREAMS`: конфиг есть, `StreamManager.ActiveStreamsTotal()` есть, но лимит нигде не enforced.
- Сделать `SyncWorker` менее шумным: сейчас каждые 2 секунды он сохраняет все активные торренты в SQLite и рассылает весь список в SSE.
- Реализовать Basic Auth или убрать параметры: `HUB_AUTH_ENABLED`, `HUB_AUTH_USER`, `HUB_AUTH_PASSWORD` есть в конфиге/README, но middleware авторизации не видно.

## Backend

- Разделить `internal/engine/engine.go`: сейчас это большой файл примерно на 1700 строк, лучше вынести `client_config`, `resource_manager`, `swarm_watchdog`, `priority`, `health`, `metainfo`.
- Добавить graceful shutdown для фоновых goroutine `resourceMonitor` и `swarmRefreshMonitor`, сейчас они живут на ticker без контекста остановки.
- Оптимизировать `GetAllTorrents`: сейчас файлы грузятся отдельным запросом на каждый торрент, для большой библиотеки лучше одним батч-запросом.
- Добавить dirty-state sync: сохранять torrent в БД только если изменились `state`, `downloaded`, `files`, `speeds`, а не на каждом тике.
- Добавить `pprof`/runtime metrics за флагом: goroutines, heap, open fds, DB write latency, active streams, active torrents.

## Скачивание торрентов

- Сделать range-aware streaming QoS: при `Range` запросе приоритизировать окно вокруг текущей позиции, а не только весь файл целиком `High`.
- Добавить adaptive prebuffer: первые N MB, последние pieces для MP4/MOV metadata, потом sliding window впереди playhead.
- Восстанавливать пользовательские file priorities после стрима точнее: сейчас stream mode может временно менять приоритеты других файлов.
- Добавить tracker scoring: не просто добавлять retrackers, а отслеживать успешность/ошибки и не дергать мертвые трекеры слишком часто.
- Добавить NAT/port diagnostics: проверка доступности TCP/UDP torrent port, статус UPnP, понятная подсказка в UI.
- Добавить отдельный `NAS-safe` профиль: меньше half-open, ниже dial rate, меньше hasher workers, более мягкий watchdog.
- Добавить seed policy: seeding ratio/time limit, auto-stop seeding, per-torrent upload limit.

## Диск и память

- Сделать configurable `PieceHashersPerTorrent`, сейчас значение захардкожено как `4`.
- Добавить disk health panel: свободное место, скорость записи, состояние `DiskFull`, путь downloads/config.
- Добавить manual rescan/recheck файлов на диске с восстановлением прогресса после ручного переноса.
- Добавить cleanup orphan files/metainfo: показать файлы на диске, которых нет в БД.
- Рассмотреть SQLite-backed piece completion вместо fallback в memory при ошибке Bolt.

## Web GUI

- Почистить `BTHealthPanel`: оставить только реально существующие поля backend-модели или вернуть backend-поля осознанно.
- Добавить глобальные toast-уведомления вместо `console.error`.
- Добавить поиск, сортировку и фильтры по статусу, скорости, прогрессу, размеру.
- Добавить bulk actions: pause/resume/delete/set priority для выбранных торрентов.
- Добавить страницу Settings: лимиты скорости, max downloads, max streams, DHT/PEX/UPnP, profiles, с пометкой `needs restart` там, где нужно.
- Добавить dark mode, так как в phase22 он был отложен.
- Улучшить player UX: кнопка `Copy stream URL`, `Open in VLC`, M3U/playlist link, QR-код для ТВ/телефона.
- Добавить viewed history: продолжить просмотр с позиции, последние просмотренные файлы, mark as watched.

## Совместимость

- Расширить TorrServer API compatibility под Lampa/NUM: проверить реальные вызовы `cache`, `settings`, `viewed`, `playlist`, `stat`.
- Добавить e2e smoke-тесты TorrServer API, чтобы не ломать Smart TV сценарии.
- Добавить более понятный M3U playlist с абсолютными URL и названиями файлов.

## Рекомендуемая следующая фаза

Предлагаемая фаза: `phase29-stabilization-and-api-ui-consistency`.

Возможный объем:

- Почистить остатки `HardRefresh`/`RecycleClient` во frontend/config/docs.
- Реализовать `recheck`.
- Enforce `MaxActiveStreams`.
- Оптимизировать `SyncWorker` и SSE.
- Добавить Basic Auth middleware или удалить неработающие auth-настройки.
- Обновить отчет и прогнать `go test ./...` + `npm run build`.

После этого логично делать отдельную фазу про streaming QoS и оптимизацию скачивания.
