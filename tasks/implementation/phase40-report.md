# Фаза 40 — Отчёт об имплементации

Статус: **реализация и локальные исправления выполнены; приёмка CI не завершена**. Последний разобранный запуск `40137410282` остановился на Linux `race-root`; исправления теста readiness и ошибок, воспроизведённых в Linux, описаны ниже. Предыдущие Windows-исправления также требуют подтверждённой CI-приёмки. Основание: [план фазы 40](../phase40-review-fixes-and-architecture-hardening.md). Фаза 41 не начата.

## Linux race-root — запуск 40137410282

[Job 121001767404](https://github.com/ivangolenkov/torrent-stream-hub/actions/runs/40137410282/job/121001767404), commit `4649c3334708a8f608f17f144b13d9b024e8601a`. Публичная страница подтверждает падение `Run make check`, exit 2. Полный лог требует входа, а GitHub API вернул rate limit. Пользователь предоставил фрагменты: ошибку `TestProcessShutdownWithOpenSSEAndBodyWait` (`add: 503 ... restoring persisted torrents`) и отдельный `FAIL torrent-stream-hub/internal/engine` в `race-root`. Имя упавшего engine-теста в предоставленных фрагментах отсутствует; точное совпадение с локальными engine-сбоями не утверждается.

Подтверждённые причины и изменения:

- `cmd/hub/shutdown_test.go`: старый process test считал ответ `/echo` готовностью всего API. После раннего запуска HTTP echo отвечает ещё во время восстановления, а добавление корректно получает 503. Тест теперь ждёт 200 от `/api/v1/torrents`, повторяя запрос при 503; неожиданные статусы остаются ошибкой. Проверки открытого SSE, незавершённого body и SIGTERM сохранены.
- Linux/arm64 повтор с `GOMAXPROCS=2`, Go 1.25.8 и race detector воспроизвёл ошибки состояния/слотов в engine-тестах, ожидания probe и конкретную гонку: запись `e.cfg.MaxActiveDownloads` в `TestStreamSource_ConcurrentDownloadAndDirectStream` против чтения в `manageResources`. В `internal/engine/stream_source_test.go` запись защищена `admissionMu`, как и чтение.
- `internal/engine/resource_manager.go`: удалён дублирующий начальный фоновый `manageResources`. Первый проход уже синхронно выполняется при регистрации торрента; отложенный старт фоновой горутины мог занять health probe раньше регистрации или следующей команды, заставив их собственный проход пропустить эту запись. Монитор начинает периодические проверки по прежнему пятисекундному таймеру. Admission, проверки диска, coalescing и запрет ожидания чужого syscall сохранены; FIFO/selection/DiskFull assertions не ослаблялись.
- Последующий полный `make check` на macOS воспроизвёл отдельную production-панику `panic: true` в `PeerConn.servePeerRequest`: чтение upload-запроса возвращалось после закрытия торрента, error path не удалял запрос, а deferred assertion обнаруживал его в `unreadPeerRequests`. В `pkg/anacrolix-torrent/peerconn.go` закрытие peer теперь очищает непрочитанные запросы и готовые буферы. `peerconn_shutdown_test.go` блокирует настоящий upload refresh вне client lock, выполняет `Torrent.Drop`, проверяет освобождение обеих очередей и завершение worker; это детерминированная регрессия найденной паники. Существующий assertion сохранён.
- Контракт начального прохода и синхронизация тестового изменения конфигурации описаны в `internal/engine/CONCURRENCY.md`.

Проверки:

| Среда / команда | Результат |
| --- | --- |
| macOS, исходный `make check` | До правок полностью прошёл, что не воспроизводило ошибку CI. |
| macOS, `go test -race -count=20 -timeout=120s ./cmd/hub -run 'TestProcessShutdownWithOpenSSEAndBodyWait\|TestHTTPAvailableDuringPersistedRestore'` | 20 повторов исправленного process test и регрессий раннего HTTP прошли. |
| macOS, `go test -race -count=10 -timeout=180s ./internal/engine` | 10 полных повторов после исправлений прошли. |
| Linux/arm64, два CPU, `GOMAXPROCS=2`, `go test -race -count=5 -timeout=180s ./internal/engine ./cmd/hub` | Оба пакета прошли по пять раз: engine 47.289 s, cmd/hub 11.464 s. |
| macOS, fork `go test -race -count=20 -timeout=60s . -run '^TestServePeerRequestDuringTorrentClose$'` | 20 повторов новой регрессии прошли после исправления. |
| macOS, `go test -race -count=20 -timeout=120s ./internal/engine -run '^TestStreamSource_ConcurrentDownloadAndDirectStream$'` | 20 повторов loopback download/direct-stream сценария прошли после исправления panic. |
| Linux/arm64, root `go test -race -count=1 -timeout=180s ./...`, затем fork `go test -race -count=1 -timeout=180s . ./storage` | Полные root и core/storage fork suites прошли с окончательным кодом. |
| macOS, окончательный `make check` | Успешно: root/fork tests и race, оба vet, 27 Vitest-тестов, frontend build и backend build. Лог: `/tmp/tsh-actions-40137410282-acceptance.log`. |
| `git diff --check` | Успешно. |

Первый Linux stress запуск с `-count=15 -timeout=180s` завершился общим timeout после множества полных проходов, уже выведя самостоятельные ошибки assertions и DATA RACE. Этот общий timeout не объявляется доказательством зависания конкретного теста. Логи до/после: `/tmp/tsh-ci-40085220944-linux/{engine,fixed}-40137410282.log`. Контейнер подключал исходники только для чтения; это Alpine/Linux arm64, а не Ubuntu/amd64 runner GitHub.

- [x] Устранено ошибочное ожидание readiness в process test; повторные startup/shutdown regression tests проходят.
- [x] Исправлены воспроизведённая test data race и конкурирующий начальный scheduler pass; повторные engine suites проходят на macOS и Linux.
- [x] Исправлена воспроизведённая panic при закрытии peer во время upload; очереди очищаются, новый regression test и loopback integration прошли по 20 повторов.
- [x] Итоговый локальный `make check` и полные Linux root/fork race suites прошли. Платформа контейнера и границы сравнения с CI указаны выше.
- [ ] Подтвердить успешный удалённый `make check`; исходное имя engine-теста в GitHub по имеющимся фрагментам не установлено.

Работа остаётся в рамках lifecycle/concurrency и CI-приёмки фазы 40. Общий DoD не закрыт локальными повторами.

## Запуск поверх сохранённых данных — 25 октября 2026

Пользователь сообщил об остановке на `restoring persisted torrents` и недоступности HTTP. Подтверждена причина недоступности HTTP: `main` синхронно выполнял restore, startup cleanup и первый snapshot до открытия HTTP listener и установки обработчиков сигналов. Поэтому задержка любой записи блокировала весь HTTP, включая диагностику. Конкретная причина задержки на данных пользователя пока не установлена: его ОС, логи дальнейших стадий и набор сохранённых данных не получены.

- `cmd/hub/main.go`: сигналы подключены в начале запуска; HTTP listener открывается перед restore. Startup и периодические workers имеют одного владельца: при сигнале или ошибке Serve отменяются контексты, закрываются SSE, останавливается HTTP и ожидается завершение workers до закрытия engine/storage/SQLite. Порядок restore → temporary cleanup → initial snapshot сохранён.
- `internal/delivery/http/router.go`: до завершения startup операции с торрентами возвращают 503 с `Retry-After: 1`; заголовок доступен через CORS. Статика, `/echo`, logs и logs/status отвечают во время восстановления. `GET /api/v1/events` принимает ограниченные существующим лимитом SSE subscriptions без обращения к runtime.
- `internal/delivery/http/api/handlers.go`: SSE сразу отправляет комментарий и flush, затем получает первый реальный snapshot. Пустой список не подставляется вместо восстанавливаемых данных. Открытая во время запуска веб-страница получает данные без перезагрузки; frontend-код не изменён.
- `internal/usecase/torrent_uc.go`, `storage_migration.go`: добавлен `RestoreTorrentsContext`, контекст передаётся чтению persisted snapshots и ожиданию record handles. Логи показывают storage-init, количество записей, начало/окончание/ошибку/отложенное восстановление каждой записи с hash и длительностью; main логирует cleanup/snapshot и готовность API. Ошибки отдельных записей собираются и логируются; обслуживание восстановленных и DB-only записей сохраняется.
- `cmd/hub/startup_test.go`: интеграционный тест использует SQLite, сохранённый `.torrent`, настоящий engine и намеренно блокирует storage resolver под library lock. Проверяются доступность HTTP/страницы/логов/CORS, 503 для torrent API, подписка SSE до завершения restore, публикация восстановленного торрента в той же подписке, отмена и ожидание startup при shutdown. Отдельный тест проверяет отмену/ожидание startup при ошибке listener. Тест SSE snapshot version учитывает допустимые комментарии перед data frame.
- README, `internal/engine/CONCURRENCY.md` и `internal/usecase/STATE.md` описывают новый контракт запуска. Изменение продолжает lifecycle/HTTP работы фазы 40; задачи следующей фазы не выполнялись.

Проверки на macOS arm64 / Go 1.25.8:

| Команда | Результат |
| --- | --- |
| `go test -count=1 -timeout=180s ./...` | Полный набор тестов корневого модуля прошёл. |
| `go test -race -count=1 -timeout=180s ./cmd/hub ./internal/usecase ./internal/delivery/http/...` | Все тесты затронутых lifecycle/restore/HTTP пакетов прошли. Линкер macOS выводит предупреждение LC_DYSYMTAB, итоговый exit code 0. |
| `go vet ./...` | Успешно. |
| `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/tsh-phase40-linux-amd64 ./cmd/hub` | Успешно; это сборка, а не выполнение на Linux. |
| `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/tsh-phase40-windows-amd64.exe ./cmd/hub` | Успешно; нативное выполнение на Windows не проводилось. |
| `git diff --check` | Успешно. |

- [x] Медленное восстановление не препятствует доступу к HTTP-диагностике; команды не обходят startup readiness, CORS `*` сохранён.
- [x] SIGINT/SIGTERM и ошибки HTTP отменяют startup; отменяемые ожидания завершаются до закрытия ресурсов. SSE продолжает работу после восстановления.
- [x] Регрессии, root tests, целевые race tests, vet, cross-build и документация выполнены.
- [ ] Подтвердить запуск на исходных данных пользователя и установить причину первоначальной задержки по новым progress logs/диагностике. Данные пользователя не изменялись.

Снята зависимость HTTP от завершения restore, но произвольный зависший filesystem syscall не объявляется исправленным или прерываемым. Правила ownership, миграции payload, hash verification и DiskFull не ослаблены. Общий DoD фазы остаётся открытым до удалённой CI-приёмки, описанной ниже.

## Исправление Windows CI — закрытие mmap storage

Пользователь предоставил сбой очистки `TempDir` в `TestFileIoImplementationsSeekDataDetection/mmap` и `TestShortFile`: Windows не мог удалить открытые файлы. Кэш `mmapFileIo` удерживал собственную ссылку на mapping после закрытия отдельных reader/writer, а `fileTorrentImpl.Close()` не освобождал ресурсы backend. В `TestShortFile` дополнительно отсутствовал вызов `TorrentImpl.Close()`. На macOS успешное удаление открытого файла скрывало утечку.

- В `storage/file-io-mmap.go` добавлен идемпотентный `Close`: освобождаются ссылки кэша, отклоняются новые операции. Открытые handles завершают работу со своими ссылками; последний handle выполняет unmap и закрывает файловый дескриптор. Закрытие кэша не ждёт удерживаемый reader lock и не инвалидирует активный mapping.
- В `storage/file-torrent.go` закрытие торрента освобождает backend, реализующий `io.Closer`, и сохраняет результат для повторного `Close`. Контракт описан в `storage/file-io.go`. Общий `RootFS` по-прежнему принадлежит клиенту.
- В двух упавших тестах исправлен порядок освобождения ресурсов до удаления временного каталога. В `storage/file-io-mmap-close_test.go` добавлены регрессии для read-only/writable кэша, активных handles и закрытия через `TorrentImpl`. Проверяются фактически закрытый дескриптор, освобождённый mapping, удаление файла и запрет операций после закрытия.
- Workflow и набор Windows-тестов сохранены. Все пути выше находятся в `pkg/anacrolix-torrent/`.

Проверки после исправления на macOS:

| Директория | Команда | Результат |
| --- | --- | --- |
| `pkg/anacrolix-torrent` | `go test -race -count=3 ./storage -run 'TestFileIoImplementationsSeekDataDetection\|TestShortFile\|TestMmapFileIoClose\|TestFileTorrentClose\|TestFileCloseWaits'` | Три прогона выбранных регрессий прошли. |
| `pkg/anacrolix-torrent` | `go test -race -count=1 . ./storage` | Полные suites ядра форка и storage прошли. |
| Корень | `go test -count=1 ./...` | Полный набор тестов приложения прошёл. |
| `pkg/anacrolix-torrent` | `go vet . ./storage` | Успешно. |
| `pkg/anacrolix-torrent` | `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -c -o /tmp/tsh-storage-windows.test.exe ./storage` | Тестовый бинарник собран; это не выполнение тестов на Windows. |

- [x] Устранена утечка кэша; локальные регрессии, fork race, root tests, vet и Windows cross-compile прошли.
- [ ] Подтвердить успешный повторный `Native storage (windows-latest)` после включения исправления в CI.

Исправление относится к storage/lifecycle и платформенной приёмке фазы 40; расширения scope или переноса задач не потребовалось. Frontend не изменялся; его результаты ниже относятся к предыдущей полной локальной приёмке.

### Повторный Windows CI — проверка закрытого дескриптора

В следующем предоставленном логе упали три новые регрессии только на ожидании `os.ErrClosed` после `mapping.f.Stat()` в `checkMmapReleased`. Ошибок удаления файлов и освобождения mapping в этом логе нет. Ошибка была в добавленной проверке: в исходниках используемого Go 1.25.8 Windows-реализация `File.Stat` вызывает `statHandle` напрямую и возвращает системную ошибку без преобразования в `os.ErrClosed`.

- В `pkg/anacrolix-torrent/storage/file-io-mmap-close_test.go` проверка заменена чтением одного байта через underlying `os.File.ReadAt`. Этот путь проверяет закрытое состояние в `internal/poll` и преобразует `ErrFileClosing` в `os.ErrClosed`; проверены локальные исходники Go для Windows. Непустой буфер обязателен, чтобы чтение действительно проверяло состояние файла.
- Проверяются ноль прочитанных байт и `qt.ErrorIs(err, os.ErrClosed)`, поэтому при сбое выводится сама ошибка. Проверки освобождения mapping и удаления файла сохранены; production-код и workflow не изменены.
- После этой правки на macOS прошли fork `go test -race -count=1 ./storage`, `go vet . ./storage`, root `go test -count=1 ./...` и Windows cross-compile тестового бинарника той же командой, что в таблице выше.
- [x] Устранено платформенное предположение теста; локальные проверки прошли.
- [ ] Получить успешный нативный Windows CI с исправленной проверкой. Cross-compile его не заменяет.

Изменение остаётся в рамках платформенной приёмки фазы 40; отклонений от плана нет.

## Разбор Release #11 — запуск 40085220944

Источник: [GitHub Actions run](https://github.com/ivangolenkov/torrent-stream-hub/actions/runs/40085220944), tag `v0.1.10`, commit `d5d09e60d0d0f8d74cbe2bdafa0fdeeefa2a33b0`, 24 октября 2026 года. Прочитана публичная HTML-сводка со всеми annotations: **2 errors, 3 warnings, 2 notices**. Полные логи требуют авторизации; анонимный API вернул rate limit, GitHub HTTPS credential helper не предоставил сохранённых credentials. Разбор недоступных строк лога не объявляется выполненным.

| Сигнал | Подтверждённое состояние и действие |
| --- | --- |
| Windows storage, exit 1 | Три упавших теста из предоставленного пользователем лога сводятся к непереносимой проверке `Stat` в одном helper. Локальная замена на `ReadAt` описана выше и отсутствует в коммите запуска `d5d09e6`. |
| Linux checks, exit 2 | Ошибка шага `make check`, [job 120846661390](https://github.com/ivangolenkov/torrent-stream-hub/actions/runs/40085220944/job/120846661390#step:6:1908). Это итоговый код `make`; конкретный тест, сборка или target по публичной сводке не определяются. Полный текст шага запрошен у пользователя. |
| Три предупреждения Node.js 20 | `checkout@v4`, `setup-go@v5`, `setup-node@v4` запускаются GitHub через принудительную замену runtime на Node.js 24. В обоих workflows обновлены соответствующие ссылки на `checkout@v5`, `setup-go@v6`, `setup-node@v6`. |
| Уведомление Ubuntu | GitHub сообщает о миграции `ubuntu-latest` на Ubuntu 26. Связь уведомления с падением тестов не доказана. |
| Уведомление macOS | Ограниченная доступность arm64 runners и возможные очереди; это не сообщение об ошибке приложения. |
| Пропущенные release jobs | Binaries, Docker и публикация релиза зависят от checks; их пропуск после падения checks ожидаем. Это не отдельные подтверждённые ошибки сборки/публикации. |

Node.js 24 подтверждён в официальных `action.yml`: [checkout v5](https://github.com/actions/checkout/blob/v5/action.yml), [setup-go v6](https://github.com/actions/setup-go/blob/v6/action.yml), [setup-node v6](https://github.com/actions/setup-node/blob/v6/action.yml). Версия Node.js для сборки frontend остаётся 22.15.0; версия Go — 1.25.8. YAML обоих workflows успешно разобран через Ruby YAML; успешное выполнение обновлённых actions на GitHub ещё не подтверждено.

Для дополнительной диагностики использован локальный Linux/arm64 контейнер с Go 1.25.8; исходники подключены только для чтения. При `CGO_ENABLED=0` прошли root `go test -count=1 ./...` и fork `go test -count=1 . ./storage`. Затем при `CGO_ENABLED=1` с C-компилятором, установленным в одноразовом контейнере, прошли root `go test -race -count=1 ./...` и fork `go test -race -count=1 . ./storage`. Логи race-прогонов: `/tmp/tsh-ci-40085220944-linux/{root-race,fork-race}.log`. Это Alpine/Linux arm64, а не полный Ubuntu GitHub job; результаты не устанавливают причину сбоя конкретного CI run. Frontend, vet/build и GitHub Actions runtime в этом Linux-контейнере не запускались.

- [x] Все публичные annotations разобраны; известный Windows helper исправлен, предупреждения runtime учтены в workflows.
- [ ] Получить полный Linux `make check` log и установить исходную причину exit 2.
- [ ] Подтвердить успешные Windows/Linux CI jobs с исправлениями; общая CI-приёмка остаётся открытой.

## Блок 1 — Исходное состояние

- Рабочая база: завершённая фаза 39; до начала изменений в рабочем дереве находился только новый план фазы 40.
- В `pkg/anacrolix-torrent/go.mod` минимальная версия Go согласована с корнем и используемыми методами `os.Root`: 1.25.8.
- Регрессионные сценарии добавляются вместе с исправлениями в соответствующих блоках.

## Проверки

- Исходные `go test -count=1 ./...`, fork `go test -race -count=1 . ./storage`, Vitest (22 теста) и frontend build прошли на Go 1.25.8 / Node 22.15.0. Sandbox не разрешал запись Go cache; Go suites выполнены с разрешённым расширенным доступом.
- Baseline `BenchmarkVerifiedWriteBookkeeping` (Apple M3 Max, 20 iterations): 1000 pieces — 13748 ns/op; 10000 — 135019 ns/op; 100000 — 1382375 ns/op; 0 allocations. Это стоимость bookkeeping одного write, а не throughput загрузки.
- После блока 2 повторный полный `go test -count=1 ./...` прошёл.

## Блок 2 — Каталог и команды

- F40-01: добавлен format v1/v2/hybrid/unknown, миграция `0011_catalog_format.sql`, проверка реальных aligned offsets отдельно от payload size, piece count и overflow. Legacy format не угадывается; корректируется только по новому metainfo-каталогу.
- F40-03: metadata/permanence сохраняются одной транзакцией и одной revision; одинаковая metadata/permanence не меняет revision. No-op не подтверждает предыдущую неприменённую команду.
- A40-04: введены общие `runIntentCommand`/`applyCommittedIntent` для commit/application/bind failures; Add, metadata/permanence, Pause/Resume и priority используют общую границу применения. Полная reconciliation и incremental apply остаются различными.
- Добавлены regression tests composite rollback/no-op/prior failed intent, v2 multi-file Add/sync/restore и v1/v2/hybrid offset/zero-length/overflow. Churn fixture теперь действительно меняет metadata на каждом проходе.

## Блок 3 — Storage

- Публикация `.part` переведена на native atomic no-replace для Linux/macOS/Windows; Link+Remove fallback удалён. Неизвестные legacy hardlink pairs сохраняются для безопасного ручного восстановления.
- Добавлен bounded SQLite completion adapter с неблокирующими Get/Set, ограниченным кэшем и очередью hints. В composition root используется основная SQLite; самостоятельный engine без injected backend использует memory cache. Старый Bolt не открывается и не удаляется.
- Rooted-open проверки объединены в storage port; usecase больше не выдаёт raw torrent.File. HTTP fixtures проверяют данные через публичный Recheck. Добавлены тесты atomic publication/foreign target/legacy hardlinks, SQLite reopen/coalescing/backpressure/failure. Нативные Windows/Linux прогоны подготовлены в CI; локально выполнены macOS suites и cross-builds, см. итоговую матрицу.

## Блок 4 — Verification

- Общие signatures файлов и инкрементальные counters заменяют обход всех verified pieces на каждую запись и завершение piece. Freshness-проход делает stat по уникальным файлам. Read/upload/direct-open/handshake/persistence проверяют свежесть вне client lock, затем применяют результат только к тому же backend.
- Неизвестная completion остаётся unknown: начальная verification после миграции не отключается. Перед persistence внешняя замена отзывает verified progress.
- Добавлены тесты линейного числа stat и доступности client lock при заблокированном storage; полные root tests и fork `-race . ./storage` прошли.
- Повторный `BenchmarkVerifiedWriteBookkeeping`, 20 iterations: 1000/10000/100000 pieces — 70.85/64.55/62.50 ns/op, 0 allocations. Это локальная стоимость bookkeeping, не измерение NAS throughput.

## Блок 5 — Lifecycle и бюджеты

- Добавлены ранняя отмена engine перед join workers, контексты download preparation и capture, общий HTTP admission, отдельная HEAD lease без debounce.
- Бюджеты: 64 service tasks, 256 metadata watchers, 2 verification tasks, `MaxActiveStreams + 4` stream watchers, 2 event workers/128 coalesced pending keys, 256 QoS file states. Saturation команд возвращает ошибку; storage safety сохраняет библиотечную синхронную latch и повторяется обслуживающим worker.
- Полные root tests и race suites engine/usecase/HTTP прошли. Проверены saturation, idempotent release, HEAD без debounce, отмена подготовки download и record wait. Непрерываемый filesystem syscall по-прежнему требуется дождаться перед закрытием storage.

## Блок 6 — Публикация и read model

- Publisher больше не получает record handles и не ждёт observation commits. Два persistence workers объединяют pending работу по hash (128 ключей), сохраняют fencing/identity и пропускают занятые записи до следующего sampling. SQLite writer wait использует request/service context и короткий busy timeout на выделенном соединении.
- Вместо второго полного DB read/DeepEqual используются global/per-record durable versions. Telemetry bytes/timestamps не меняют эти версии. Immutable catalog digest исключает повторные catalog writes; file observations обновляют только изменённые значения.
- Добавлены явный `?view=compact` для REST/SSE, catalog version и загрузка деталей выбранного torrent с проверкой identity/revision. Полные ответы сохраняются. Retained buffers: full 16 MiB, compact 2 MiB; до 128 SSE subscribers с latest-only mailbox.
- SQL baseline (1000 файлов, 20 iterations): 25.52 ms/op, 2000 file writes/op, 1091392 B/op, 61336 allocs/op. После: 1.58 ms/op, 0 file writes/op, 198676 B/op, 3840 allocs/op.
- Root tests и race engine/usecase/HTTP прошли; frontend 22 tests/build прошли на этом шаге; итоговые 27 тестов включают compact-details regression. Проверены publication при занятом record и SQLite writer, версии при ABA/telemetry и компактный payload на 10000 файлов.

## Блок 7 — Discovery и сеть

- Публичные retrackers добавляются только после получения public metadata. Known-private и явно private-safe торренты не получают DHT/PEX/public retrackers; при обнаружении private metadata отменяется текущий DHT announce и отбрасываются прежние peer connections/reserve. Private-safe сохраняется в SourceURI и восстанавливается для magnet и `.torrent`.
- Неизвестный magnet без private-safe использует только явно переданные trackers и обычные DHT/PEX до metadata. Это документированная граница: private флаг из ещё неизвестного info нельзя предсказать. При отключённом DHT для metadata нужен исходный tracker/peer.
- Upload burst не менее 64 KiB при сохранении заданного sustained rate. Boost хранит deadline, продлевается и восстанавливает исходный connection limit; fake clock проверяет expiry и защиту нового поколения.
- Полные root и fork race suites прошли. Добавлены deterministic DHT doubles и тест сохранения private-safe после file Add/restore.

## Блок 8 — HTTP

- Control body 1 MiB/30 s, upload 32 MiB внутри 34 MiB multipart envelope, fields 64 KiB/16 parts; максимум два общих upload/fetch jobs. Spill идёт в управляемый каталог и удаляется на выходе. Oversize/timeout/storage/policy/overload возвращают 413/408/507/403/429.
- Выделенный metainfo fetcher: 60 s на операцию, пять redirects, 32 MiB body, проверка каждого адреса, DNS pinning на фактическом dial, TLS по исходному hostname, proxy environment игнорируется. LAN разрешается только явной host:port/CIDR allowlist.
- M3U URL собирается через URL/path/query encoder; CR/LF в label удаляются. Regression выполняет запрос по сгенерированной ссылке с `#?%`, пробелами и Unicode.
- Root и race HTTP/metafetch suites прошли. Реальный slow HTTP/1 тест выявил auto-drain после снятия deadline; error response теперь закрывает незавершённое соединение. HTTP BaseContext связан с engine shutdown.

## Блок 9 — GUI и контракты

- Подтверждённые priorities ограничены command receipt/record identity. Более новый server snapshot снимает override независимо от значения другого клиента; старые snapshots и details responses не откатывают команду.
- Удалены hard refresh/recycle controls/client methods и отсутствующие поля диагностики. Ручные TypeScript health interfaces проверяются против Go JSON tags, типов и optionality. Boost отображает фактическое состояние engine.
- Legacy hard-refresh/recycle/episode/benchmark env/flags принимаются, но явно диагностируются как deprecated/no-effect; из активных примеров и defaults они удалены. Удаление парсинга допускается только отдельным версионированным изменением конфигурации.
- Root tests, Vitest (27 tests, включая компонент diagnostics) и production build прошли.

## Блок 10 — Проверки и завершающие исправления

- Добавлен reusable `.github/workflows/checks.yml` с read-only permissions: Linux root/fork tests/race/vet/build и Vitest; macOS/Windows native storage race/vet. Release binary/image jobs зависят от него. Go 1.25.8 и Node 22.15.0 согласованы с Docker.
- `make test` включает оба Go-модуля и Vitest; `make check` дополнительно запускает race, vet и build. Fork package list явно ограничен `.` и `./storage`; это не заявление о тестировании всех пакетов upstream-форка.
- Первая полная приёмка обнаружила старые предупреждения fork vet: позиционные литералы внешних типов, копирование mutex в PEX fixture и reflect.SliceHeader при сериализации диагностического peer identity. Литералы именованы, fixture копирует только данные, trace codec хранит числовую identity без восстановления указателя в память другого процесса. Добавлен round-trip/invalid-length тест. Пакеты за пределами core/storage не менялись.
- Процессный SIGTERM regression запускает реальный `run`, открывает SSE и подтверждённо блокирует body reader через Expect: 100-continue. Выявлен auto-drain `net/http` после снятия deadline; незавершённое тело теперь закрывается под expired deadline. Shutdown прошёл без ожидания HTTP drain timeout.
- Общий HTTP budget проверяется одновременно на archive/file/TorrServer endpoints; HEAD проходит без slot, завершение archive освобождает его. 32 control slots удерживаются до завершения handler, ограничивая также накопление уже прочитанных bodies у заблокированных команд.
- Добавлены реальные SQLite query_only/max_page_count failures, cancellation ожидания занятого connection pool, malformed multipart cleanup, flush failure перед publication, потеря acknowledgement после atomic rename/reopen и гонка двух публикаций за один target.
- Последний аудит private-safe охватывает hybrid aliases: повторное добавление по другому hash не может временно изменить discovery policy без её durable SourceURI.

## Итоговые проверки

Среда: macOS arm64 / Apple M3 Max, Go **1.25.8**, Node **22.15.0**. Сначала выполнен `npm ci` по lockfile, затем проверки со свежей frontend сборкой.

| Проверка | Результат |
| --- | --- |
| `make check` | Успех, exit 0: root и fork tests/race, Vitest, TypeScript/Vite, оба vet и backend build. |
| Root `go test -count=1 ./...` | Успех, включая SQLite integration, compatibility и процессный SIGTERM regression. |
| Root `go test -race -count=1 ./...` | Успех; предупреждений race detector нет. |
| Fork `go test -count=1 . ./storage` и `-race` | Успех. Это два изменённых пакета, не весь upstream module. |
| `go vet ./...` / fork `go vet . ./storage` | Успех без отключения анализаторов. |
| `npm test` / `npm run build` | 27 тестов в шести файлах, TypeScript и production build прошли. |
| Backend macOS arm64 build | Успех; `bin/torrent-stream-hub`. |
| `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build ... ./cmd/hub` | Успех cross-build. |
| `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ... ./cmd/hub` | Успех cross-build. |
| Windows `go test -c ... ./storage` | Успех cross-compile; не выполнение Windows binary. |
| Native no-replace с однобуквенным target, `-race` | Успех на macOS; Windows ABI сверена с локальным Go runtime и кросс-компилируется. |
| YAML Checks/Release/Compose и `git diff --check` | Успех. |

Локальный macOS linker при race-сборках печатает `malformed LC_DYSYMTAB`; сборка и тесты завершаются с exit 0. Предупреждение не скрывалось. `npm ci` также сообщает о deprecated транзитивных пакетах; зависимости вне scope фазы не обновлялись.

## Покрытие находок

| ID | Реализация и свидетельство |
| --- | --- |
| F40-01 | Format-aware catalog и migration 0011; repository offsets/overflow и usecase v2 Add/sync/restore tests. |
| F40-02 | Atomic no-replace; flush/acknowledgement/конкурирующий target/reopen tests, сохранение legacy hardlinks. Удалённого шага Remove больше нет. |
| F40-03 | Один metadata/permanence commit; duplicate/no-op/prior-failed-intent regressions. |
| F40-04 | Общая private discovery policy; fake DHT/PEX, deferred retrackers, private-safe file/restore/hybrid-alias tests. |
| F40-05 | Численные body/job/spill budgets; chunked/slow/malformed/temp-failure и retained-body bounds tests. |
| F40-06 | Общий атомарный transfer admission; concurrent slot test и реальные archive/file/TorrServer запросы. |
| F40-07 | Отдельный DNS-pinned fetcher; initial/redirect/IPv6/rebinding/proxy/allowlist/magnet/cancel tests. |
| F40-08 | Cancel до join, HTTP BaseContext, body interruption, cancelable SQLite reads/writes; process SIGTERM и coordinator cancellation tests. |
| F40-09 | Receipt/record/version ограничивают priority overrides; два клиента, пропущенный snapshot и late details regressions. |
| F40-10 | HEAD lease без slot/debounce; HEAD/Delete и HTTP cross-endpoint tests. |
| F40-11 | Burst >= 64 KiB; 1 KiB/s reservation обычного 16 KiB block подтверждает задержку без отказа. |
| F40-12 | Deadline/extension/expiry/restore limit; fake-clock generation test и GUI diagnostics test. |
| F40-13 | URL encoding и CR/LF labels; GET по настоящему M3U URL с особыми именами. |
| F40-14 | Удалены мёртвые controls/types; Go→TS contract test, warnings legacy options, обновлённые примеры. |
| F40-15 | Go 1.25.8 root/fork/CI/Docker; самостоятельные fork suites и release cross-builds. |
| A40-01 | Инкрементальные file signatures/counters, уникальные stat вне client lock; complexity tests и benchmarks выше. |
| A40-02 | Независимый publisher и два bounded persistence workers; blocked record/SQLite writer A не блокирует B. |
| A40-03 | Read/catalog versions, delta file writes, compact subscription/details; SQL benchmark и 10000-file compact test. |
| A40-04 | Общие commit/apply/bind/failure helpers; regression suites intent/reconcile/fencing. |
| A40-05 | Storage metadata handle, единый rooted-open adapter, PieceStatus и verified source APIs. |
| A40-06 | SQLite completion composition root, bounded cache/queue, close/reopen/full/read-only/failure tests; Bolt не открывается production. |
| A40-07 | Численные task/event/transfer/control-body budgets, coalescing и retained safety effects; saturation/cancel/release tests. |
| A40-08 | Make targets, reusable PR checks и release dependency; локальные исполнения отделены от будущих native CI runs. |

## Статус DoD

Чек-лист соответствует исходному плану. Отметки относятся к реализованным контрактам и локальным проверкам; они не подтверждают успешное выполнение удалённых Windows/Linux runners или аппаратный power-cut test. Известный сбой Windows CI, его исправление и оставшаяся платформенная проверка описаны в начале отчёта.

- [x] F40-01: валидные v2 multi-file и hybrid каталоги принимаются, offsets/размеры корректны после Add и restore.
- [x] F40-02: публикация `.part` восстанавливается после промежуточного сбоя без потери данных и ослабления ownership/no-replace.
- [x] F40-03, A40-04: составные команды и общий commit/apply/bind/retry protocol не оставляют ложный success или вечный pending.
- [x] F40-04: private discovery policy действует во всех автоматических и явных путях; граница неизвестной metadata описана и проверена.
- [x] F40-05: входящие bodies, temp spill, длительность чтения и metainfo jobs имеют проверенные численные лимиты.
- [x] F40-06: общий transfer admission соблюдает MaxActiveStreams для torrent/disk/archive paths и освобождает slots на всех выходах.
- [x] F40-07: HTTP fetch защищён от запрещённых initial/redirect/DNS/proxy targets и поддерживает явно разрешённые LAN-источники.
- [x] F40-08: worker/record waits отменяются, полный shutdown соблюдает порядок cancel/join/close; пределы непрерываемого I/O описаны честно.
- [x] F40-09: более новый server state снимает устаревший local priority override; старые snapshots не откатывают команды.
- [x] F40-10: HEAD не создаёт playback debounce, stream slot и последующую блокировку удаления.
- [x] F40-11: низкий upload rate не отвергает нормальные protocol blocks и не обходит rate limiting.
- [x] F40-12: boost имеет срок, возвращает базовый connection limit и корректно отображается health/UI.
- [x] F40-13: M3U экранирует URL и labels; специальные имена проигрываются через handler.
- [x] F40-14: UI/types/config описывают только поддерживаемое поведение; legacy no-effect options явно диагностируются.
- [x] F40-15: root/fork/CI/toolchain согласованы, самостоятельная сборка форка воспроизводима.
- [x] A40-01: устранены полные per-write/per-completion обходы и массовый filesystem I/O под client lock; счётчики сложности и benchmarks приложены.
- [x] A40-02: publication не ждёт persistence/долгой операции другой записи, сохраняя identity/revision/tombstone safety.
- [x] A40-03: каталог версионируется, неизменные данные не переписываются, full DeepEqual snapshots заменены компактной проверкой; GUI использует краткую модель и детали по версии.
- [x] A40-05: storage adapter владеет layout/open/verification/publication; прикладные пути не обходят его через сырые library объекты.
- [x] A40-06: production completion использует SQLite, Bolt не открывается; migration/reopen/failure и bounded cache lifecycle проверены.
- [x] A40-07: task queues/workers/retained bytes имеют численные budgets и overload policy, обязательные safety effects не теряются.
- [x] A40-08: Makefile/PR/release checks охватывают оба Go-модуля, frontend и заявленные platform storage suites; выполненное отделено от неподтверждённого.
- [ ] Полные root tests/race, обязательные fork suites/race, vet, backend build, Vitest и frontend build проходят; regression гарантий фаз 34–39 отсутствует. Локальные результаты приложены; ошибки Windows/Linux CI требуют успешной повторной приёмки.
- [x] Upgrade/restart/rollback ограничения, API/config/ownership/lifecycle документация актуальны; `tasks/implementation/phase40-report.md` содержит каждый F40/A40 и общий DoD.


## Принятые решения и ограничения

- Обязательные исправления не перенесены в другую фазу. Фаза 41 не начата; публикация релиза и изменение branch protection не требовались.
- Legacy hardlink pairs не имеют достаточного durable ownership evidence. Согласно предусмотренному планом консервативному варианту обе копии сохраняются; процедура ручного восстановления описана в README. Новый протокол не создаёт эту пару.
- Для неизвестного magnet retrackers отложены до public metadata. В deferred `replace` первоначальные bootstrap trackers сохраняются; известный public `.torrent` заменяет их до admission. Это сознательная граница совместимости, подробно описанная в README. Private-safe требуется до первого discovery, его нельзя обещать ретроактивно.
- Проверки выполнялись на локальной macOS filesystem. Windows/Linux runtime-поведение зависит от подготовленного CI; cross-build не заменяет native execution. Directory sync на Windows не гарантируется этим adapter. Реальное отключение питания и испытания на NAS/сетевых filesystems не проводились.
- SQLite hints допускают потери при overload/full/read-only, но не предоставляют readiness. Legacy Bolt сохраняется; downgrade требует восстановления целого offline backup, включая payload, metainfo и SQLite.
- Application cancellation завершает waits, но не делает любой OS syscall прерываемым. Storage/SQLite не закрываются под активной owned task. Полные snapshots ограничены 16 MiB, compact 2 MiB на публикацию; превышение сохраняет предыдущую публикацию и логируется. Эти sampled DTO не являются доказательством готовности данных.
- Дополнительная очистка старых fork vet warnings и отмена pool wait нужны для обязательной приёмки. Trace serialization больше не создаёт Go pointer из сохранённого адреса; BitTorrent wire format не изменён.
