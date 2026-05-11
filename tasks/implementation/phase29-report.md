# Phase 29: Engine Refactoring, Graceful Shutdown и оптимизация Repository (Отчет)

## Что было сделано

### Backend: Engine refactoring

- `internal/engine/engine.go` сокращен до фасада движка: основные типы, constructor/lifecycle, публичные методы управления torrent runtime и базовые операции `Add/Pause/Resume/Delete/Get`.
- Конфигурация torrent client вынесена в `internal/engine/client_config.go`:
  - создание `anacrolix/torrent` client;
  - сборка `torrent.ClientConfig`;
  - qBittorrent profile;
  - public IP discovery;
  - callbacks статусов и peer connection diagnostics.
- Работа с metainfo и piece completion вынесена в `internal/engine/metainfo.go`:
  - `newPieceCompletion`;
  - сохранение `.torrent` metainfo;
  - metadata watcher;
  - sanitizing diagnostic errors.
- Логика retrackers вынесена в `internal/engine/trackers.go`.
- Логика file priorities вынесена в `internal/engine/priority.go`.
- BT health/mapping/runtime stats вынесены в `internal/engine/health.go`.
- Swarm low-speed recovery/watchdog вынесен в `internal/engine/swarm_watchdog.go`.
- Resource management/disk-full/max-active-downloads logic вынесены в `internal/engine/resource_manager.go`.

### Backend: Graceful shutdown

- В `Engine` добавлен lifecycle-контроль:
  - `ctx context.Context`;
  - `cancel context.CancelFunc`;
  - `wg sync.WaitGroup`;
  - `closeOnce sync.Once`.
- `resourceMonitor` и `swarmRefreshMonitor` теперь запускаются через общий helper `startBackground`.
- Оба monitor loop теперь слушают `ctx.Done()` и останавливают свои `time.Ticker` через `defer ticker.Stop()`.
- `Engine.Close()` стал idempotent через `sync.Once`, вызывает `cancel()`, ожидает `wg.Wait()`, затем закрывает torrent client и storage.

### Backend: Repository optimization

- `repository.GetAllTorrents()` больше не вызывает `loadFiles(t)` для каждого торрента.
- Добавлен batch-loader `loadFilesForTorrents`, который загружает файлы фиксированным дополнительным SQL-запросом через `JOIN torrents`.
- Сохранено текущее поведение:
  - порядок торрентов `ORDER BY created_at DESC`;
  - порядок файлов внутри торрента `index ASC`;
  - торренты без файлов возвращаются без ошибки;
  - file priority/downloaded/is_media сохраняются корректно.

### Тесты

- Добавлен `TestEngineCloseStopsBackgroundMonitors` в `internal/engine/engine_lifecycle_test.go`:
  - проверяет shutdown с включенным swarm watchdog;
  - проверяет shutdown с выключенным swarm watchdog;
  - проверяет повторный `Close()` без panic/hang.
- Добавлен `TestGetAllTorrentsBatchLoadsFilesAndPreservesOrder` в `internal/repository/torrent_repo_test.go`:
  - проверяет порядок торрентов;
  - batch-загрузку файлов;
  - порядок файлов по index;
  - torrent без файлов;
  - priority/downloaded/is_media.

## Статус выполнения DoD

- [x] `internal/engine/engine.go` разделен на несколько тематических файлов без изменения внешнего поведения.
- [x] Конфигурация torrent-client вынесена в отдельный файл `client_config.go` или эквивалентный по смыслу файл.
- [x] Resource manager вынесен в отдельный файл `resource_manager.go` или эквивалентный по смыслу файл.
- [x] Swarm watchdog вынесен в отдельный файл `swarm_watchdog.go` или эквивалентный по смыслу файл.
- [x] Логика file priorities вынесена в отдельный файл `priority.go` или эквивалентный по смыслу файл.
- [x] BT health mapping/formatting вынесены в отдельный файл `health.go` или эквивалентный по смыслу файл.
- [x] Metainfo/piece-completion helpers вынесены в отдельный файл `metainfo.go` или эквивалентный по смыслу файл, если это не ухудшает связность.
- [x] `resourceMonitor` останавливается через context/cancel при `Engine.Close()`.
- [x] `swarmRefreshMonitor` останавливается через context/cancel при `Engine.Close()`.
- [x] `Engine.Close()` ожидает завершения monitor goroutine и не оставляет активные tickers.
- [x] `Engine.Close()` безопасен при повторном вызове или явно защищен от повторного закрытия.
- [x] `repository.GetAllTorrents()` больше не делает отдельный SQL-запрос файлов на каждый torrent.
- [x] `GetAllTorrents()` сохраняет текущий порядок торрентов и файлов.
- [x] Добавлены или обновлены тесты repository-слоя для batch-загрузки файлов.
- [x] Добавлены или обновлены тесты engine lifecycle/shutdown.
- [x] `go test ./...` проходит успешно.
- [x] `go build ./cmd/hub` или `make test` проходит успешно.
- [x] Создан отчет `tasks/implementation/phase29-report.md` после фактической реализации.

## Проверки

- Baseline до изменений: `go test ./...` - успешно.
- После изменений: `go test ./internal/engine` - успешно.
- После изменений: `go test ./internal/repository` - успешно.
- После изменений: `go test ./...` - успешно.
- После изменений: `go build ./cmd/hub` - успешно.

## Причины отхода от плана

- Существенных отходов от плана нет.
- Дополнительно выделен файл `trackers.go`, чтобы не смешивать retracker helpers с `client_config.go` и сохранить более четкую ответственность файлов.
