# Phase 29: Engine Refactoring, Graceful Shutdown и оптимизация Repository

## Описание

Цель фазы - снизить технический долг в backend-ядре без изменения внешнего поведения сервиса. Основные направления:

- разделить монолитный `internal/engine/engine.go` на логические файлы;
- добавить управляемое завершение фоновых goroutine движка;
- оптимизировать загрузку списка торрентов из SQLite, убрав N+1 запросы к таблице `files`.

Фаза является стабилизационной. В рамках этой фазы не должны добавляться новые пользовательские функции, новые API-контракты или изменения поведения скачивания/стриминга, кроме тех, которые прямо необходимы для безопасного shutdown и сохранения текущей логики.

## Контекст и проблема

Сейчас `internal/engine/engine.go` содержит большую часть логики движка: конфигурацию `anacrolix/torrent`, управление ресурсами, health-снимки, приоритеты файлов, работу с metainfo, watchdog swarm recovery, добавление/удаление/пауза/резюмирование торрентов. Из-за этого файл сложно сопровождать, а изменения в одной области повышают риск регрессий в другой.

Также фоновые процессы `resourceMonitor` и `swarmRefreshMonitor` запускаются как goroutine с `time.Ticker`, но не имеют общего контекста остановки. При shutdown сервиса это не критично для процесса, но усложняет тестирование, lifecycle-контроль и будущие интеграционные сценарии.

В repository-слое `GetAllTorrents()` загружает файлы через отдельный запрос на каждый торрент. Для большой библиотеки это создает N+1 pattern и лишнюю нагрузку на SQLite.

## Цели

- Сделать `internal/engine` более модульным без изменения публичного поведения.
- Сохранить текущие exported API движка для usecase/delivery слоев.
- Добавить явный lifecycle для фоновых monitor goroutine.
- Обеспечить корректное завершение monitor goroutine при `Engine.Close()`.
- Уменьшить количество SQL-запросов при `GetAllTorrents()` с `1 + N` до фиксированного числа запросов.
- Сохранить существующую семантику сортировки торрентов и файлов.
- Покрыть изменения тестами и не допустить регрессий `go test ./...`.

## Не входит в фазу

- Изменение алгоритмов скачивания, приоритизации pieces или streaming QoS.
- Изменение REST API или TorrServer API.
- Изменение схемы SQLite, если оптимизация возможна без миграции.
- Внедрение новых observability endpoints.
- Рефакторинг frontend.
- Реализация `recheck`, `MaxActiveStreams`, Basic Auth или других пунктов из backlog.

## План работ

### 1. Подготовка safety net

- Запустить текущие backend-тесты, чтобы зафиксировать baseline перед рефакторингом.
- Просмотреть существующие тесты вокруг `engine`, `repository`, `usecase` и HTTP handlers.
- Определить минимальный набор тестов, который должен покрыть новую lifecycle-логику и batch-загрузку файлов.

Ожидаемый результат:

- Понятен текущий baseline.
- Есть список тестов, которые нужно добавить или обновить в рамках фазы.

### 2. Разделить `internal/engine/engine.go`

Разделение должно быть механическим и безопасным: переносить функции в новые файлы внутри пакета `engine` без изменения сигнатур и поведения.

Предлагаемая структура:

- `internal/engine/engine.go` - основные типы `Engine`, `ManagedTorrent`, constructor `New`, базовый lifecycle, `Close`, публичные методы высокого уровня.
- `internal/engine/client_config.go` - `newTorrentClient`, `buildClientConfig`, `applyQBittorrentProfile`, `applyPublicIPConfig`, `discoverPublicIP`, `randomPeerID`, download profile/client config helpers.
- `internal/engine/resource_manager.go` - `resourceMonitor`, `manageResources`, `logResourceSummary`, disk-full/resource-slot логика.
- `internal/engine/swarm_watchdog.go` - `swarmRefreshMonitor`, `checkSwarms`, `lowSpeedRefreshReasonLocked`, `updatePeakDownloadSpeedLocked`, `refreshPeerDiscovery`, `refreshDHTAsync`, связанные структуры/decision helpers.
- `internal/engine/priority.go` - `mapToPiecePriority`, `applyFilePrioritiesAndDownload`, `SetFilePriority`, `SetTorrentFilesPriority`, приоритеты при pause/resume если будет уместно без ухудшения читаемости.
- `internal/engine/health.go` - `BTHealth`, `mapManagedTorrent`, `updateSpeedsLocked`, `trackerCounts`, `wasteRatio`, health formatting helpers.
- `internal/engine/metainfo.go` - `saveMetainfo`, `newPieceCompletion`, restore/metainfo-related helpers, если зависимости позволяют сделать файл независимым.

Правила рефакторинга:

- Не менять package name: все файлы остаются в `package engine`.
- Не менять exported signatures без необходимости.
- Не менять порядок вызовов, mutex-семантику и side effects.
- Не смешивать рефакторинг с поведенческими улучшениями.
- После каждого крупного переноса запускать targeted compile/tests при необходимости.

Ожидаемый результат:

- `engine.go` становится существенно меньше и отвечает за ядро lifecycle/публичный фасад.
- Логика разнесена по файлам с понятной ответственностью.
- Все существующие тесты проходят без изменения ожиданий.

### 3. Добавить graceful shutdown для monitor goroutine

Нужно добавить управляемый lifecycle для фоновых процессов, стартующих из `Engine.New()`.

Предлагаемый подход:

- В `Engine` добавить поля lifecycle-контроля:
  - `ctx context.Context`;
  - `cancel context.CancelFunc`;
  - `wg sync.WaitGroup`.
- В `New()` создать context через `context.WithCancel(context.Background())`.
- Запуск `resourceMonitor` и `swarmRefreshMonitor` делать через helper, который увеличивает `wg` и гарантирует `Done()`.
- Изменить monitor functions так, чтобы они принимали `context.Context` или использовали `e.ctx`.
- Внутри monitor loop заменить `for range ticker.C` на `select` по `ticker.C` и `ctx.Done()`.
- В `Close()` вызвать `cancel()`, дождаться `wg.Wait()`, затем закрывать torrent client/storage.
- `Close()` должен быть idempotent насколько это возможно: повторный вызов не должен паниковать.

Важные детали:

- Не держать `Engine.mu` во время `wg.Wait()`.
- Не блокировать shutdown на долгих сетевых операциях, если monitor уже запустил отдельные goroutine для refresh.
- Проверить, что `resourceMonitor` успевает сделать первичный `manageResources()` при старте, как сейчас.
- Если `swarmRefreshMonitor` отключен конфигом, `wg` не должен ждать несуществующую goroutine.

Ожидаемый результат:

- `Engine.Close()` корректно останавливает monitor goroutine.
- Тесты могут создавать и закрывать `Engine` без утечек ticker/goroutine.

### 4. Оптимизировать `GetAllTorrents()` в repository

Сейчас `GetAllTorrents()` вызывает `loadFiles(t)` для каждого торрента. Нужно заменить это на batch-загрузку файлов.

Предлагаемый подход:

- Оставить `GetTorrent(hash)` как есть или минимально затронуть его, потому что одиночная загрузка не страдает от N+1.
- Для `GetAllTorrents()`:
  - загрузить все строки из `torrents` одним запросом с текущей сортировкой `ORDER BY created_at DESC`;
  - собрать `[]*models.Torrent` и `map[string]*models.Torrent`;
  - одним запросом загрузить все строки из `files` для этих hash;
  - разложить файлы по соответствующим torrent через map;
  - сохранить сортировку файлов по `hash`, `index` или выполнить сортировку в Go, если запрос не гарантирует нужный порядок.

Варианты SQL:

- Если число торрентов небольшое/среднее, можно использовать `WHERE hash IN (?, ?, ...)`.
- Если нужно избежать слишком длинного IN-list, можно загрузить все files через JOIN с torrents:
  - `SELECT f.hash, f."index", f.path, f.size, f.downloaded, f.priority, f.is_media FROM files f JOIN torrents t ON t.hash = f.hash ORDER BY t.created_at DESC, f."index" ASC`.
- Предпочтительный вариант для простоты и стабильности: JOIN без динамического IN-list.

Правила:

- Сохранить текущий порядок торрентов: `created_at DESC`.
- Сохранить текущий порядок файлов внутри торрента: `index ASC`.
- Сохранить поведение для пустой БД.
- Сохранить поведение для торрентов без файлов.
- Не менять миграции, если индексы уже достаточны: `files` имеет primary key `(hash, index)`, что подходит для загрузки файлов по hash/index.

Ожидаемый результат:

- `GetAllTorrents()` выполняет фиксированное число SQL-запросов.
- Поведение API/usecase не меняется.

### 5. Тестирование repository-оптимизации

Добавить или обновить тесты в `internal/repository/torrent_repo_test.go`.

Проверить:

- `GetAllTorrents()` возвращает все торренты.
- Порядок торрентов соответствует `created_at DESC`.
- Файлы каждого торрента загружены полностью.
- Файлы отсортированы по `index ASC`.
- Торрент без файлов не ломает результат.
- Приоритеты файлов, `downloaded`, `is_media` сохраняются корректно.

Если есть возможность без усложнения тестов, добавить проверку количества SQL-запросов через легкую обертку/инструментацию не требуется. Достаточно структурного теста результата, так как реализация будет видна в коде.

### 6. Тестирование graceful shutdown

Добавить или обновить тесты в `internal/engine`.

Проверить:

- `Engine.New()` запускает monitor goroutine без блокировок.
- `Engine.Close()` завершается за ограниченное время.
- Повторный `Close()` не приводит к panic.
- При выключенном `BTSwarmWatchdogEnabled` shutdown все равно корректен.
- `resourceMonitor` сохраняет первичный вызов `manageResources()` или эквивалентное поведение старта.

Тест не должен быть flaky:

- Не завязываться на точное количество goroutine в runtime.
- Использовать timeout через context/channel.
- Не ждать реального interval ticker дольше минимально необходимого.

### 7. Проверка интеграции

После рефакторинга и оптимизации выполнить:

- `go test ./...`;
- `go build ./cmd/hub` или существующую build-команду проекта;
- при необходимости `make test`, если изменения затрагивают общий lifecycle и нужно подтвердить frontend build тоже.

Ожидаемый результат:

- Все backend-тесты проходят.
- Сервис компилируется.
- Поведение API и Web GUI не меняется.

### 8. Отчет по фазе

После реализации фазы создать `tasks/implementation/phase29-report.md`.

В отчете отразить:

- какие файлы были выделены из `engine.go`;
- как реализован lifecycle/shutdown фоновых goroutine;
- как оптимизирован `GetAllTorrents()`;
- какие тесты добавлены/изменены;
- результаты проверок;
- любые отклонения от плана.

## Definition of Done

- [ ] `internal/engine/engine.go` разделен на несколько тематических файлов без изменения внешнего поведения.
- [ ] Конфигурация torrent-client вынесена в отдельный файл `client_config.go` или эквивалентный по смыслу файл.
- [ ] Resource manager вынесен в отдельный файл `resource_manager.go` или эквивалентный по смыслу файл.
- [ ] Swarm watchdog вынесен в отдельный файл `swarm_watchdog.go` или эквивалентный по смыслу файл.
- [ ] Логика file priorities вынесена в отдельный файл `priority.go` или эквивалентный по смыслу файл.
- [ ] BT health mapping/formatting вынесены в отдельный файл `health.go` или эквивалентный по смыслу файл.
- [ ] Metainfo/piece-completion helpers вынесены в отдельный файл `metainfo.go` или эквивалентный по смыслу файл, если это не ухудшает связность.
- [ ] `resourceMonitor` останавливается через context/cancel при `Engine.Close()`.
- [ ] `swarmRefreshMonitor` останавливается через context/cancel при `Engine.Close()`.
- [ ] `Engine.Close()` ожидает завершения monitor goroutine и не оставляет активные tickers.
- [ ] `Engine.Close()` безопасен при повторном вызове или явно защищен от повторного закрытия.
- [ ] `repository.GetAllTorrents()` больше не делает отдельный SQL-запрос файлов на каждый torrent.
- [ ] `GetAllTorrents()` сохраняет текущий порядок торрентов и файлов.
- [ ] Добавлены или обновлены тесты repository-слоя для batch-загрузки файлов.
- [ ] Добавлены или обновлены тесты engine lifecycle/shutdown.
- [ ] `go test ./...` проходит успешно.
- [ ] `go build ./cmd/hub` или `make test` проходит успешно.
- [ ] Создан отчет `tasks/implementation/phase29-report.md` после фактической реализации.

## Риски и ограничения

- Рефакторинг большого файла может случайно изменить порядок lock/unlock или side effects. Поэтому перенос должен быть максимально механическим.
- Shutdown с `WaitGroup` может зависнуть, если monitor loop или вложенная операция не проверяет context. Нужно избегать ожидания долгих операций внутри monitor goroutine.
- Оптимизация repository не должна нарушить восстановление persisted metadata и file priorities.
- В тестах lifecycle нельзя полагаться на точное количество goroutine, так как `anacrolix/torrent` и SQLite могут запускать свои фоновые процессы.

## Критерии отказа от части плана

- Если выделение `metainfo.go` создает циклическую или искусственную связность внутри `engine`, допустимо оставить часть metainfo helpers рядом с lifecycle-кодом, но это нужно описать в отчете.
- Если `Engine.Close()` нельзя сделать полностью idempotent без существенного усложнения, допустимо защитить повторный вызов минимальным `sync.Once` или документированным guard, но без изменения публичной сигнатуры.
- Если batch-загрузка файлов через JOIN ухудшит читаемость или поведение сортировки, допустимо использовать `IN`-запрос с placeholders, но N+1 pattern должен быть устранен.
