# Phase 31: Прямая Отдача Завершённых Файлов И Снижение Lock Contention

## Статус И Предпосылки

- Статус: запланировано; этот документ не является отчётом о реализации.
- Реализацию начинать после проверки DoD Phase 30 и её отчёта `tasks/implementation/phase30-report.md`.
- Пользовательский сценарий: просмотр полностью скачанного файла в браузерном плеере Lampa начинает буферизоваться при загрузке другого, независимого торрента. По наблюдению пользователя, диск и CPU не перегружены.
- Цель: дать пользователю сборку с оптимизированной отдачей для повторной проверки, без обязательного предварительного сбора профилей с его устройства.
- Наличие общей блокировки подтверждено исходниками. Её роль в конкретном зависании остаётся гипотезой до сравнительных измерений и проверки в Lampa.

## Описание

Добавить в существующие streaming endpoints выбор источника данных:

1. Готовый файл в поддерживаемом файловом storage: открытый файловый дескриптор и `http.ServeContent`.
2. Незавершённый файл или недоступный direct path: существующий `torrent.Reader` с контекстом, readahead и range-aware QoS.

Оптимизация блокировок в этой фазе означает устранение обращений к torrent client mutex из повторяющихся `Read`/`Seek` при передаче готового файла. Первичная проверка готовности и регистрация stream могут кратковременно обращаться к движку. Полностью lock-free обработка HTTP-запроса не требуется.

## Основания В Текущем Коде

### Сервер

- `internal/delivery/http/torrserver/handlers.go`: `serveStream()` всегда создаёт `file.NewReader()`, затем вызывает `http.ServeContent`; отдельной ветки для готового файла нет.
- `pkg/anacrolix-torrent/t.go`: `newReader()` устанавливает `reader.mu = t.cl.locker()`.
- `pkg/anacrolix-torrent/reader.go`: `readContext()` берёт этот mutex до и после чтения; `waitAvailable()` берёт read lock клиента. `Seek()` также использует общий mutex.
- `pkg/anacrolix-torrent/peerconn.go`, `peer.go`: обработка сообщений пиров и обновление состояния загрузки используют ту же блокировку. Дисковые операции выполняются с её отпусканием; нельзя описывать весь I/O как выполняющийся под client lock.
- `pkg/anacrolix-torrent/file.go`: `BytesCompleted()` складывает completed pieces и dirty chunks незавершённых pieces. Равенство размеру файла не гарантирует завершение проверки и записи.
- `internal/engine/engine.go`: `Warmup()` всегда создаёт torrent reader и читает первые байты, включая уже завершённый файл.
- `internal/engine/stream.go`: completed file не получает новый piece overlay, но сессия всё равно регистрируется; cleanup вызывает `restoreBasePriorities()` даже при отсутствии изменений priorities.
- `internal/delivery/http/api/download_handlers.go`: `serveDiskFile()` уже отдаёт файлы с диска и поддерживает Range, однако устанавливает attachment-заголовки, не подходящие для переноса в streaming handler целиком.
- `internal/usecase/torrent_uc.go`: `GetDownloadedFile()` опирается на модель прогресса и поиск кандидатов пути. Это не готовый контракт для строгого выбора streaming fast path.
- `internal/engine/client_config.go`: используется `storage.NewFileWithCompletion()`; фактическое размещение определяют `storage/file-client.go`, `file-paths.go`, `file-torrent.go`. По умолчанию включены `.part`-файлы.
- Корневой `go.mod` использует локальный fork через `replace github.com/anacrolix/torrent => ./pkg/anacrolix-torrent`.

### Lampa

Изученные исходники находятся в соседнем checkout `/Users/ivangolenkov/Documents/my/my/lampa-source`; он нужен как источник сведений, а не как обязательная зависимость тестов Hub.

- `src/interaction/torrent.js`, `preload()`: запрос `&preload`, затем `&stat` раз в секунду; запуск при достижении 95% предзагрузки.
- `src/interaction/player.js`, `preload()`: замена `&preload` на `&play`.
- `src/interaction/player/video.js`, `load()`: URL передаётся в `video.src`; Range-запросы обычного файла формирует браузер.
- `src/interaction/player/video.js`, `bind()`: индикатор ожидания включается по `waiting`, скрывается по `playing`.
- `src/interaction/player/info.js`: `/cache` используется для визуализации pieces, а не для остановки уже идущего видео.

Серверный preload и буфер `<video>` — разные сущности: ответ о готовности не означает передачу этих байтов браузеру.

## Границы Фазы

Входит:

- direct streaming для завершённого файла, в том числе внутри незавершённого multi-file торрента;
- проверка готовности под одной блокировкой на вызов helper;
- сохранение HTTP-контракта, учёта stream и корректного cleanup;
- fast path для preload;
- диагностические логи выбора источника и результата передачи;
- тесты корректности и независимости чтения от client mutex;
- сравнение производительности и ручная проверка в Lampa.

Не входит:

- замена общей модели синхронизации anacrolix, удаление locks из существующего reader;
- изменение BitTorrent wire protocol, resource manager, download/upload limits;
- новые настройки Lampa, переписывание браузерного плеера, HLS/transcoding;
- автоматическое переключение уже открытого torrent reader на disk в середине HTTP-ответа;
- новый RAM-кеш, mmap, чтение целого видео в память;
- изменение схемы SQLite и полная переработка эмуляции `/cache`;
- публичный диагностический endpoint или постоянное профилирование.

## План Работ По Файлам

### 1. Проверка Готовности Файла В Локальном Fork

**Изменить:** `pkg/anacrolix-torrent/file.go`.

Добавить метод с явным контрактом, например `File.ReadyForDirectRead() bool`:

- Взять один `cl.rLock()`, выполнить проверку в памяти, отпустить lock.
- Проверить, что torrent не закрыт, metadata доступны, границы файла корректны.
- Проверить все pieces, пересекающие файл, включая общие с соседними файлами first/last pieces.
- Разрешать direct read только при известном completed-состоянии без ошибки completion, pending writes, hashing, queued hash и marking для затрагиваемых pieces.
- Отдельно проверить отсутствующие v2 piece-layer hashes, если они препятствуют подтверждению готовности в текущем fork.
- Нулевой файл обработать без обращения к несуществующему piece: готовность metadata/lifecycle плюс проверка реального файла на этапе открытия.
- Не вызывать публичные методы `Piece.State()`, `BytesCompleted()` и другие повторно блокирующие методы внутри взятого lock; использовать locked/internal state.
- Не выполнять storage I/O, повторное хеширование или создание reader внутри helper.
- Не создавать массив состояния всех pieces торрента. Допустим проход по pieces выбранного файла с ранним выходом; оптимизация bitmap должна сохранять проверки переходных состояний.

**Семантика completion:** это готовность по принятому движком completion state. В текущем storage состояние при восстановлении может выводиться из окончательных имён файлов. Метод не обещает повторную криптографическую проверку всего файла при каждом HTTP-запросе и не меняет существующую политику restore/recheck.

**Добавить:** `pkg/anacrolix-torrent/file_direct_read_test.go` — unit-тесты helper и benchmark проверки большого файла.

### 2. Открытие Direct Reader И Владение Ресурсами

**Добавить:** `internal/engine/stream_source.go`.

Выделить engine API подготовки источника, например `OpenStreamSource(ctx, hash, index)`, возвращающий:

- тип источника: `disk` или `torrent`;
- `io.ReadSeekCloser`, длину, display name;
- типизированную причину отказа от fast path для диагностики;
- idempotent cleanup и связь с контекстом запроса/закрытием соответствующего torrent.

Точная форма структуры может уточняться при реализации, но HTTP-слой не должен самостоятельно реконструировать storage layout или запускать несколько независимых проверок модели прогресса.

Алгоритм подготовки:

1. Получить текущий managed torrent и выбранный файл, освободить `Engine.mu` до дальнейших операций.
2. Проверить runtime readiness через новый helper.
3. Для текущего поддерживаемого file storage вычислить точный путь из engine metadata и действующего storage layout: в частности проверить соответствие `File.Path()` фактическому `BestName`/`BestPath` layout.
4. Выполнить безопасное открытие внутри `DownloadDir`: запретить traversal и выход через symlink, использовать подходящий root-relative open либо эквивалент с защищённым открытием. Не использовать имя из URL или пользовательский `Title` как путь.
5. Открывать только окончательный файл. `.part`, отсутствие final file или незавершённый rename означают отказ от direct path. Не выбирать первый существующий файл из нескольких кандидатов с совпавшим размером.
6. На уже открытом дескрипторе проверить regular file и точный размер. При неуспехе закрыть его до fallback.
7. Повторно проверить lifecycle/readiness после открытия, чтобы исключить наблюдаемые изменения во время подготовки. Каждый вызов helper берёт один lock; файловые операции остаются вне него.
8. Подготовить direct reader без создания `torrent.Reader`. Для fallback создать и настроить torrent reader по текущим правилам.

Политика ошибок:

- Not-ready, `.part`, завершившийся не до конца rename, неподдерживаемое storage: явный fallback `torrent` до начала ответа.
- Неизвестный hash, неверный index, отменённый context, закрытый torrent: вернуть соответствующую ошибку, не скрывать её как успешную отдачу.
- Ошибка открытия/несовпадение размера: записать причину; допустим fallback к штатному torrent storage, но не выдача произвольного disk file.
- После начала HTTP-ответа запрещены переключение источника, повторная выдача диапазона и добавление JSON-ошибки в тело видео.

**Изменить:** `internal/engine/engine.go` — подключить подготовку источника и необходимую привязку к lifecycle; `Warmup()` переработать в шаге 5.

**Изменить при необходимости:** `internal/engine/client_config.go` — явно связать fast path с используемым file storage/layout. Если точное отображение пути невозможно обеспечить на уровне engine, минимально расширить `pkg/anacrolix-torrent/storage/file-client.go` / `file-torrent.go` и публичный storage-контракт; описать это решение и тесты в отчёте. Не угадывать путь при изменении backend.

### 3. UseCase И HTTP Streaming

**Изменить:** `internal/usecase/torrent_uc.go`.

- Добавить проксирование нового source API и преобразование ошибок в существующий контракт usecase.
- Не использовать `GetTorrent()`/`GetDownloadedFile()` как единственное доказательство readiness: модель может включать непроверенные байты, а чтение всей статистики добавляет ненужную работу.
- Сохранить существующие attachment/download API; переиспользовать только действительно общие проверки с одинаковой семантикой.

**Изменить:** `internal/delivery/http/torrserver/handlers.go`.

- В `serveStream()` подготовить источник до отправки заголовков/тела, закрывать его во всех ветках.
- Обе ветки передавать в `http.ServeContent`; сохранить единый способ вычисления Content-Type и DLNA-заголовков.
- Оставить `parseStreamRange()` источником параметров QoS, а фактическую обработку HTTP Range — `http.ServeContent`, включая multi-range и invalid range.
- Сохранить URL `/stream`, `/stream/{name}`, `/play/{hash}/{id}` и правила индексов: `link`/play alias — 1-based, legacy `hash` — текущая семантика.
- Не переносить `Content-Disposition: attachment` из `serveDiskFile()`.
- Сохранить текущую политику HTTP validators: disk path не должен случайно добавить иной Last-Modified/условный кеш только из-за `os.File.ModTime()`.
- Для `HEAD` не создавать torrent reader, не читать тело и не включать QoS. Метаданные должны давать корректные заголовки.
- Не ограничивать disk path только состоянием всего торрента `Seeding`: достаточно готовности конкретного файла.

**Изменить:** `internal/delivery/http/router.go` — добавить `HEAD` в CORS `AllowedMethods`, сохранив `*` origins и exposed Range headers. Проверить preflight для HEAD и запросов с Range.

**Файлы для сверки контракта:** `internal/delivery/http/api/download_handlers.go`, `internal/delivery/http/torrserver/stream_range.go`. Их изменение не требуется, если общий контракт можно сохранить без дублирования или переработки этих обработчиков.

### 4. Учёт Просмотров Без Лишних Priority Mutations

**Изменить:** `internal/engine/stream.go`.

- Разделить учёт HTTP stream и применение download overlay. Disk stream учитывается как просмотр, хотя overlay ему не нужен.
- Добавить внутренний режим сессии, например `AccountingOnly`; назначать его по результату source selection, не по флагу из HTTP-запроса.
- Не использовать существующий `SkipQoS` для disk stream: сейчас он отключает саму регистрацию и счётчик.
- Для accounting-only session не вызывать расчёт/применение piece priorities и `restoreBasePriorities()` на cleanup, если эта сессия ничего не изменяла.
- При совместных disk/torrent/preload sessions не снимать чужие AppliedRanges. Cleanup ранее применённого overlay должен остаться корректным независимо от типа последней сессии.
- Сохранить reference counting, 10-секундный debounce, отмену таймера при новом запросе и видимость в `ActiveStreamsForTorrent()`/`ActiveStreamsTotal()`.
- `HEAD` и запросы с отключённой QoS-регистрацией по текущей политике не должны создавать фиктивные просмотры.
- Paused/DiskFull не должны переходить в Downloading из-за чтения готового файла или завершения disk session; пользовательские file priorities не меняются.

### 5. Preload Завершённого Файла

**Изменить:** `internal/engine/engine.go`, `Warmup()`; `internal/delivery/http/torrserver/handlers.go`, `servePreload()`, `serveTorrentStatus()`, `preloadedBytes()`.

- Использовать тот же контракт готовности и проверки final file, что и для disk source.
- Для готового файла подтвердить доступность открытием/проверкой, закрыть handle и вернуть готовность без чтения первых 20 MiB, torrent reader, preload overlay и фоновой warmup goroutine.
- Ответ: `preload_size = min(defaultPreloadSize, file.Length())`, `preloaded_bytes = preload_size`. Это доступный preload, а не счётчик фактически прочитанных байтов; сохранить эту семантику и в именах/комментариях результата Warmup.
- Нулевой размер обработать явно; повторные preload/stat не должны выдавать устаревшую готовность по одному лишь сохранённому target или старой модели прогресса.
- Незавершённый файл продолжает обычный асинхронный warmup; доступные данные учитывать по текущему contiguous-cache контракту.
- Не считать быстрый preload доказательством готовности навсегда: каждый новый playback request заново выбирает источник.

**Файл для сверки:** `internal/engine/cache_emul.go`. Не превращать эту фазу в изменение формата `/cache`; при переиспользовании helper не менять смысл `VirtualCacheBytes` без отдельного теста и объяснения.

### 6. Lifecycle И Отмена

**Основные файлы:** новый `internal/engine/stream_source.go`, `internal/engine/stream.go`, существующий `internal/engine/engine.go`.

- Владельцем открытого источника является один HTTP-запрос; нормальное завершение, ошибка и cancel приводят к однократному закрытию и cleanup.
- Привязать отмену к `r.Context()`, остановке engine и `Torrent.Closed()`: direct reader не должен зависеть от наличия reader в самом anacrolix.
- Проверять context на границах чтений/seek без обращения к client mutex. Любая обёртка должна сохранять корректный `Read/Seek/Close` и не позволять optional I/O interfaces обходить отмену или диагностику.
- При удалении torrent или закрытии engine завершать связанный stream, не оставляя descriptor и goroutine до произвольного следующего просмотра. Проверить также клиента, переставшего читать ответ; при необходимости завершать HTTP-запись через response deadline/существующий shutdown-механизм.
- Закрытие disk descriptor и освобождение счётчиков не выполняются под client lock и не должны бесконечно ждать завершения блокирующего сетевого I/O; отмена должна разблокировать соответствующий HTTP-запрос.
- Если изменения нужны в `Engine.Delete()`/`Close()` для синхронизации завершения прямых readers, сделать их здесь; проверить поведение удаления файлов, в том числе платформенные отличия открытого файла на Windows.
- Readiness — снимок на старте запроса, а не постоянная блокировка файла на всё время просмотра. Новый запрос во время recheck выбирает fallback; для уже открытого direct stream допускается чтение существующего дескриптора до завершения/ошибки/отмены. Внешняя перезапись файла не получает гарантию snapshot isolation.

### 7. Диагностика И Воспроизводимое Сравнение

**Изменить:** `internal/delivery/http/torrserver/handlers.go`.

**Добавить при необходимости для компактности handler:** `internal/delivery/http/torrserver/stream_observer.go`.

На начало/завершение запроса записывать:

- request ID, hash, file index, HTTP method, raw Range;
- `source=disk|torrent`, причину fallback;
- статус ответа, число реально переданных байтов и общую длительность;
- отдельно время подготовки источника и время до первой записи тела;
- ошибку чтения/записи или отмену, если она наблюдалась.

Не логировать каждый Read на INFO, не читать статистику всех торрентов на каждый чанк. Не трактовать длительность длинного Range как длительность зависания. Время первой записи на сервере не называть клиентским TTFB.

**Добавить:** `internal/delivery/http/torrserver/stream_benchmark_test.go` — управляемое сравнение disk/torrent источников с одинаковым файлом и объёмом данных. Для тестов допустим внутренний способ принудительно выбрать ветку; публичный флаг/endpoint для этого не нужен.

Сравнение на клиенте дополнить существующим download endpoint, учитывая разницу индексов и attachment headers. Он обходит torrent reader в теле ответа, но начальная проверка всё ещё обращается к engine/usecase.

## Необходимые Тесты По Файлам

### `pkg/anacrolix-torrent/file_direct_read_test.go` — новый

- Полностью completed файл; отсутствующий/incomplete piece; все dirty chunks получены, но piece ещё не проверен.
- Pending writes, Hashing, QueuedForHash, Marking, неизвестный/ошибочный completion, закрытый torrent.
- Metadata недоступны, нулевой файл, общий граничный piece двух файлов, последний короткий piece, v2 layout при поддержке текущим fork.
- Готов один файл, остальные не готовы: true только если все затрагиваемые им pieces готовы.
- Проверка не создаёт reader, не меняет priorities и не вызывает storage I/O. Benchmark размера файла/piece count с фиксацией allocations.

### `internal/engine/stream_source_test.go` — новый

- Выбор disk/torrent по фактической готовности; успешный disk не создаёт anacrolix reader.
- Single-file/multi-file, Unicode, одинаковые basename, `.part`, отсутствующий/укороченный/увеличенный файл, неправильный тип объекта, выход из DownloadDir.
- Metadata path не заменяется URL name, Title или случайным кандидатом одинакового размера.
- Проверка состояния после открытия, отменённый context, закрытие torrent во время подготовки, fallback закрывает неиспользованный handle.
- Read/Seek direct source не обращаются к engine/client state после подготовки.

### `internal/engine/stream_test.go` / `stream_window_test.go` — изменить

- Реальная регистрация disk session увеличивает счётчик и корректно снимается по context с debounce.
- Быстрые повторные Range, несколько одновременных readers, mixed disk/torrent/preload sessions.
- Accounting-only cleanup не вызывает лишнего восстановления priorities; настоящий overlay очищается без потери чужих ranges.
- Состояния Paused/DiskFull и пользовательские priorities сохраняются.

### `internal/engine/engine_lifecycle_test.go` — изменить

- Engine close / torrent delete при открытой прямой отдаче: закрытие descriptor, завершение goroutine, отсутствие deadlock.
- Cancel во время чтения и клиент, переставший читать; явные bounded deadlines в тесте вместо бесконечного ожидания.
- Recheck после открытия: проверить описанную семантику уже открытого дескриптора и новых запросов.

### `internal/usecase/torrent_uc_test.go` — изменить

- Преобразование not-found/invalid-index/readiness ошибок и корректное владение источником.
- Старый прогресс из SQLite и округлённые 100% не разрешают disk path.
- Существующий HTTP-download usecase сохраняет свой контракт.

### `internal/delivery/http/torrserver/stream_source_test.go` — новый

Использовать `httptest` server, реальные metadata и локальные deterministic fixtures; проверять байты, а не только вызов метода.

- `GET` без Range: 200 и точное тело; `bytes=0-`, `bytes=N-`, `bytes=N-M`, suffix, `bytes=0-0`: корректные 206, Content-Range/Length и тело.
- Invalid/out-of-bounds Range: корректный 416, без утечек session; multi-range: фактический стандартный multipart-ответ и правильные части.
- `HEAD` с/без Range: без тела и torrent reader, без счётчика; заголовки по контракту Go.
- Все streaming aliases и преобразования индексов, Content-Type, DLNA, отсутствие attachment и случайного изменения validators.
- Обе ветки дают одинаковое содержимое/HTTP-семантику для одинаковых данных.
- Открытый torrent reader остаётся тем же источником при завершении загрузки посреди ответа; следующий запрос может выбрать disk.
- Ошибка после начала ответа не приводит к JSON в видео или повторному HTTP-ответу.

### `internal/delivery/http/torrserver/handlers_test.go` — изменить

- Completed preload возвращает готовность без warmup goroutine/чтения/overlay; повторный stat согласован с выбранным файлом.
- Прогресс 100% при непроверенных pieces и отсутствие final file не включают completed fast path.
- Partial preload сохраняет обычное поведение; файл меньше 20 MiB, нулевой файл, устаревший preload state.

### `internal/delivery/http/router_test.go` — добавить или дополнить

- CORS для GET/HEAD и Range preflight через настоящий router: `Access-Control-Allow-Origin: *`, разрешённый HEAD, exposed Content-Range/Length.

### Проверка Независимости От Общей Блокировки

В тестовом harness подготовить disk source, затем удерживать client mutex и продолжать читать этот источник в другой goroutine. Чтение/seek должны завершиться до освобождения mutex. Не удерживать lock на этапе подготовки или cleanup: эти этапы вправе обращаться к engine.

Организовать доступ к lock только внутри тестов локального fork либо test-only harness; не добавлять production API для захвата client mutex. Если удобнее, разделить доказательство на тест fork для plain direct read и instrumented engine/HTTP test, подтверждающий выбор именно этого источника.

Дополнительно выполнить локальный integration scenario: файл A завершён, независимый torrent B получает данные от loopback seed. Проверить точность потока A, фактическое продвижение B и отсутствие зависаний. Тест должен обходиться без внешних trackers/пиров, иметь фиксированные fixtures и timeout.

## Последовательность Реализации

1. Проверить prerequisites Phase 30 и зафиксировать исходные результаты существующих тестов.
2. Реализовать readiness helper и его unit-тесты в fork.
3. Реализовать source selection, точное открытие final file, cleanup/lifecycle и тесты engine/usecase.
4. Разделить accounting-only sessions и overlays; проверить debounce и mixed sessions.
5. Подключить source selection к HTTP, быстрый preload, CORS и диагностику.
6. Добавить HTTP integration, lock-isolation test и concurrent-download scenario.
7. Выполнить обязательные проверки и сравнительные замеры; подготовить сборку для Lampa.
8. Зафиксировать результаты и незавершённую ручную проверку в отчёте; закрыть пользовательский критерий после фактического результата.

## Команды Проверки

Из корня проекта:

```bash
go test ./internal/engine ./internal/usecase ./internal/delivery/http/...
go test -race ./internal/engine ./internal/usecase ./internal/delivery/http/...
go test ./...
go build ./cmd/hub
git diff --check
```

Из `pkg/anacrolix-torrent` (отдельный Go module, не обходится корневым `go test ./...`):

```bash
go test .
go test -race .
```

Если изменялся storage-контракт, дополнительно из этого же каталога:

```bash
go test ./storage
go test -race ./storage
```

Запустить добавленные benchmarks с `-benchmem`, указать фактические имена и команды в отчёте. На CI/локальной машине не задавать универсальный порог MiB/s: оценивать корректность, независимость от mutex и сравнительные данные в одинаковых условиях.

## Ручная Приёмка В Lampa

1. Зафиксировать версию Hub, браузер, размер/формат файла, подключение клиента и настройки фоновой загрузки.
2. Открыть полностью скачанный фильм A, убедиться по логам в `source=disk`.
3. Сравнить три режима: без загрузки B, с активной загрузкой B, после её остановки. Выполнить не менее трёх переключений, дать каждому режиму не менее двух минут; дополнительно просмотреть минимум десять минут непрерывно при активной загрузке.
4. Проверить старт, паузу/продолжение, несколько быстрых перемоток, середину и конец фильма. Проверить preload включённым и выключенным.
5. Повторить с готовой серией внутри ещё незавершённого торрента и с частично скачанным видео (`source=torrent`).
6. Для браузера записать Range/response status и события `waiting`/`playing`, по возможности длительность клиентских пауз получения байтов. `Pending` и штатная буферизация сразу после seek сами по себе не являются дефектом.

Пользовательский критерий: на исходной конфигурации включение независимой фоновой загрузки не вызывает повторяемых остановок уже идущего готового фильма в проверочном интервале. Если буферизация сохраняется, фазу нельзя объявить устранившей исходный симптом; сохранить trace/логи, проверить источник и локализовать оставшийся участок.

## Definition Of Done / Критерии Приёмки

### Корректность И Архитектура

- [ ] Готовые файлы в поддерживаемом file storage автоматически обслуживаются через disk source на всех streaming aliases.
- [ ] Успешный disk path не создаёт torrent reader, не берёт client mutex в повторяющихся Read/Seek и не запускает скачивание.
- [ ] Helper готовности использует один read lock на вызов, проверяет только нужные pieces и не выполняет I/O под lock.
- [ ] Dirty/unverified/pending/transition states, закрытый torrent и отсутствие final file не допускают небезопасного direct read.
- [ ] Путь соответствует реальному storage layout; проверяются открытый descriptor, размер, тип и границы DownloadDir; `.part` не выдаётся напрямую.
- [ ] Готовность определяется по выбранному файлу, а не требованию завершения всего торрента.
- [ ] Незавершённые файлы сохраняют torrent reader, responsive/readahead и range-aware QoS.
- [ ] Источник фиксирован на HTTP-ответ; после начала передачи нет fallback, повторной выдачи данных или JSON-ошибки в видео.

### Совместимость И Lifecycle

- [ ] Range/HEAD/status codes, точность байтов, Content-Type, Content-Range/Length, DLNA и индексирование подтверждены HTTP-тестами для обеих веток.
- [ ] Streaming не получает attachment-заголовки или случайно отличающиеся validators.
- [ ] CORS разрешает GET/HEAD и требуемые Range headers с `*` origins.
- [ ] Disk sessions учитываются в ActiveStreams, сохраняют reference counting/debounce и не меняют чужие priorities.
- [ ] Cleanup accounting-only session не выполняет ненужный restore priorities; mixed overlays очищаются корректно.
- [ ] Completed preload не читает первые 20 MiB и не создаёт torrent reader/overlay/goroutine; partial preload работает как раньше.
- [ ] Cancel/delete/engine close освобождают readers/descriptors и завершают связанные goroutines; проверен медленный/переставший читать HTTP-клиент.
- [ ] Описанная политика recheck, pause и DiskFull подтверждена тестами; лишние переходы в Downloading отсутствуют.

### Проверки И Результат

- [ ] Есть тест независимости direct Read/Seek от client mutex и локальный concurrent-download integration scenario.
- [ ] Все обязательные команды, включая проверки отдельного Go module fork, прошли; baseline/environment failures явно расследованы и отражены, а не обозначены успешным запуском.
- [ ] Логи показывают выбранный источник, fallback reason, статус, байты и время без per-chunk INFO spam.
- [ ] Сравнительные замеры выполнены в одинаковых условиях, результаты и ограничения измерений приложены к отчёту.
- [ ] Ручная приёмка Lampa выполнена и пользовательский критерий подтверждён. До этого пункт остаётся открытым, даже если код и автоматические тесты готовы.
- [ ] Создан `tasks/implementation/phase31-report.md`: изменения по файлам, статусы всех DoD, команды/результаты, замеры, ручная проверка и объяснения отклонений.

## Ожидаемый Результат

Готовый фильм передаётся как обычный файл по HTTP после короткой подготовки. Активная загрузка другого торрента не заставляет каждое чтение этого фильма конкурировать за общий mutex торрент-клиента. Пользователь проверяет результат тем же способом в Lampa; серверные логи позволяют однозначно установить, применился ли новый путь отдачи.
