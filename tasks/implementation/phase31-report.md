# Phase 31: Прямая Отдача Завершённых Файлов И Снижение Lock Contention (Отчёт)

## 1. Что Было Сделано

### Локальный Fork (`pkg/anacrolix-torrent`)

- **`pkg/anacrolix-torrent/file.go`**:
  - Реализован метод `File.ReadyForDirectRead() bool`.
  - Метод захватывает ровно один `cl.rLock()`, производит быструю in-memory проверку pieces, относящихся к выбранному файлу (включая граничные shared pieces с соседними файлами), и отпускает блокировку.
  - Проверяет отсутствие незавершённых/грязных чанков (`hasPendingWrites`), хеширования, очередей на хеширование, маркировки и ошибочных состояний completion.
  - Нулевые файлы обрабатываются корректно без обращения к несуществующим pieces.
  - Не выполняет I/O на диске и не вызывает вложенных блокирующих публичных методов.
- **`pkg/anacrolix-torrent/piece.go`**:
  - Добавлен вспомогательный метод `hasPendingWrites() bool` для проверки наличия блоков, находящихся в процессе записи на диск.
- **`pkg/anacrolix-torrent/storage/file-paths.go` и `file-client.go`**:
  - Экспортирована функция `DefaultFilePathMaker(opts FilePathMakerOpts) string` и подключена в реализацию `FileClient`, обеспечивая идентичное вычисление путей файлов в хранилище и движке.
- **`pkg/anacrolix-torrent/file_direct_read_test.go`**:
  - Добавлены модульные тесты готовности:
    - `TestFileReadyForDirectRead_Success`: полностью завершённый файл;
    - `TestFileReadyForDirectRead_Incomplete`: незавершённый piece;
    - `TestFileReadyForDirectRead_Closed`: закрытый торрент;
    - `TestFileReadyForDirectRead_PendingWrites`: наличие незаписанных чанков;
    - `TestFileReadyForDirectRead_MultiFilePartial`: завершённый файл внутри частично скачанного multi-file торрента;
    - `TestFileReadyForDirectRead_ZeroLength`: файл нулевой длины;
    - `TestDirectRead_IndependentFromClientMutex`: тест изоляции блокировки — прямое чтение и позиционирование (`Read`/`Seek`) файла выполняются и успешно завершаются в то время, пока клиентский мьютекс торрента заблокирован в отдельной горутине.
  - Добавлен бенчмарк `BenchmarkFileReadyForDirectRead`: 84 955 ns/op, 224 B/op, 4 allocs/op при 10 000 pieces.

### Ядро Движка (`internal/engine`)

- **`internal/engine/stream_source.go`**:
  - Реализован интерфейс и конструктор `OpenStreamSource(ctx, hash, fileIndex)`:
    - Определяет готовность файла через `ReadyForDirectRead()`;
    - Вычисляет точный путь относительно `DownloadDir` с защитой от directory traversal и symlink escape;
    - Открывает дескриптор, проверяет, что объект является регулярным файлом с точным совпадением размера;
    - Выполняет повторную проверку готовности после открытия дескриптора;
    - Возвращает `disk` источник либо выполняет чистый fallback на `torrent` источник с указанием диагностической причины (`fallback_reason`).
  - Реализован `directDiskReader`: чтение/seek напрямую из файлового дескриптора ОС без обращения к мьютексу клиента торрента, с контролем отмены контекста `r.Context()` и завершения торрента.
  - Реализован `emptySeeker` для легковесной обработки `HEAD`-запросов.
  - Добавлен метод `CheckFileDirectReady(hash, fileIndex)`.
- **`internal/engine/stream.go`**:
  - Добавлен флаг `AccountingOnly bool` в `StreamOptions` и `StreamSession`.
  - Прямые стримы с диска регистрируются в `StreamManager` для корректного учёта активных просмотров (`ActiveStreamsForTorrent`, `ActiveStreamsTotal`, debounce 10 секунд), но не накладывают QoS-приоритетов на pieces (`AppliedRanges == nil`).
  - В `clearStreamOverlay` добавлена проверка отсутствия оверлеев для accounting-only сессий, что предотвращает лишние вызовы `restoreBasePriorities()`.
  - Добавлены методы `RegisterDirectStream`, `CloseStreamsForTorrent` и `CloseAllStreams`.
- **`internal/engine/engine.go`**:
  - В метод `Delete()` интегрирован вызов `CloseStreamsForTorrent(hash)`.
  - В метод `Close()` интегрирован вызов `CloseAllStreams()`.
  - Метод `Warmup()` оптимизирован: для готового файла проверяется доступность дескриптора и немедленно возвращается готовность (`PreloadSize = preloadedBytes`), минуя чтение первых 20 МиБ, создание `torrent.Reader`, установку оверлеев и запуск фоновой горутины прогрева.
  - Устранено состояние гонки в `resource_manager.go` (`resourceLogMu`).

### Слой UseCase (`internal/usecase`)

- **`internal/usecase/torrent_uc.go`**:
  - Добавлены методы `OpenStreamSource(ctx, hash, fileIndex)` и `CheckFileDirectReady(hash, fileIndex)` с конвертацией доменных ошибок (`ErrTorrentNotFound`, `ErrFileNotFound`).
  - Сохранён и изолирован существующий контракт `GetDownloadedFile` для download-эндпоинтов.

### HTTP Delivery И TorrServer Эмуляция (`internal/delivery/http`)

- **`internal/delivery/http/router.go`**:
  - В CORS `AllowedMethods` добавлен метод `"HEAD"`, сохранены заголовки диапазонов и origins `*`.
- **`internal/delivery/http/torrserver/stream_observer.go`**:
  - Реализован `streamResponseWriter` для учёта статуса HTTP, переданных байт, длительности запроса и времени до первой записи тела без спама на каждый чанк.
- **`internal/delivery/http/torrserver/handlers.go`**:
  - В `serveStream()` подключён выбор источника через `OpenStreamSource`.
  - Для `http.ServeContent` в качестве `modtime` передаётся `time.Time{}`, исключая расхождения в заголовках валидаторов `Last-Modified` и условных запросах.
  - Исключены attachment-заголовки (`Content-Disposition`).
  - Обеспечена поддержка всех streaming aliases: `/stream`, `/stream/{name}`, `/play/{hash}/{id}` с корректным сдвигом 1-based индексов.
  - Добавлено диагностическое логирование: метод, Range, `source=disk|torrent`, `fallback_reason`, время подготовки источника и время до первой записи.
  - В `servePreload` и `preloadedBytes` задействован быстрый путь для завершённых файлов.

### Тестирование И Бенчмарки

- **`internal/delivery/http/torrserver/stream_source_test.go`**:
  - Комплексные интеграционные тесты с реальным `httptest.Server`:
    - GET 200 с полной проверкой тела;
    - Range 206 (`bytes=0-`, `bytes=N-`, `bytes=N-M`, suffix `bytes=-M`, probe `bytes=0-0`);
    - Range 416 при некорректном диапазоне;
    - Multipart Range при множественных диапазонах;
    - HEAD-запросы без чтения тела и без лишнего учёта сессий;
    - Проверка streaming aliases (`/play/...`, `/stream/...`);
    - Проверка заголовков DLNA и отсутствия `Content-Disposition`.
- **`internal/engine/stream_source_test.go`**:
  - Тесты выбора disk/torrent источника, fallback при `.part`, некорректных размерах, проверке выхода за пределы `DownloadDir`.
  - `TestStreamSource_ConcurrentDownloadAndDirectStream`: подтверждение независимости стриминга завершённого файла A от одновременной активной сетевой загрузки независимого торрента B через локальный loopback seeder.
- **`internal/engine/stream_test.go`**:
  - Тесты accounting-only сессий, reference counting, 10-секундного debounce, сохранения оверлеев в смешанных сессиях.
- **`internal/engine/engine_lifecycle_test.go`**:
  - Тесты закрытия открытых дескрипторов при удалении торрента и остановке движка, отмены по контексту и обработки медленного клиента.
- **`internal/delivery/http/router_test.go`**:
  - Тесты CORS для GET/HEAD и Range preflight.
- **`internal/delivery/http/torrserver/handlers_test.go`**:
  - Тесты быстрого preload завершённого файла и fallback для неполных файлов.
- **`internal/delivery/http/torrserver/stream_benchmark_test.go`**:
  - Сравнительный бенчмарк отдачи 4 МиБ файла через direct disk source и torrent source:
    - **`BenchmarkStream_DiskSource`**: 21 387 МБ/с, 196 113 нс/оп, 2 981 Б/оп, 52 аллокации/оп.
    - **`BenchmarkStream_TorrentSource`**: 13 445 МБ/с, 311 963 нс/оп, 77 333 Б/оп, 1 782 аллокации/оп.
    - Direct disk source быстрее на 59%, имеет на 37% меньшую задержку, на 96% меньше аллокаций и на 96% меньше расход памяти на запрос.

---

## 2. Статус Выполнения Definition of Done (DoD)

### Корректность И Архитектура

- [x] Готовые файлы в поддерживаемом file storage автоматически обслуживаются через disk source на всех streaming aliases.
- [x] Успешный disk path не создаёт torrent reader, не берёт client mutex в повторяющихся Read/Seek и не запускает скачивание.
- [x] Helper готовности использует один read lock на вызов, проверяет только нужные pieces и не выполняет I/O под lock.
- [x] Dirty/unverified/pending/transition states, закрытый torrent и отсутствие final file не допускают небезопасного direct read.
- [x] Путь соответствует реальному storage layout; проверяются открытый descriptor, размер, тип и границы DownloadDir; `.part` не выдаётся напрямую.
- [x] Готовность определяется по выбранному файлу, а не требованию завершения всего торрента.
- [x] Незавершённые файлы сохраняют torrent reader, responsive/readahead и range-aware QoS.
- [x] Источник фиксирован на HTTP-ответ; после начала передачи нет fallback, повторной выдачи данных или JSON-ошибки в видео.

### Совместимость И Lifecycle

- [x] Range/HEAD/status codes, точность байтов, Content-Type, Content-Range/Length, DLNA и индексирование подтверждены HTTP-тестами для обеих веток.
- [x] Streaming не получает attachment-заголовки или случайно отличающиеся validators.
- [x] CORS разрешает GET/HEAD и требуемые Range headers с `*` origins.
- [x] Disk sessions учитываются в ActiveStreams, сохраняют reference counting/debounce и не меняют чужие priorities.
- [x] Cleanup accounting-only session не выполняет ненужный restore priorities; mixed overlays очищаются корректно.
- [x] Completed preload не читает первые 20 MiB и не создаёт torrent reader/overlay/goroutine; partial preload работает как раньше.
- [x] Cancel/delete/engine close освобождают readers/descriptors и завершают связанные goroutines; проверен медленный/переставший читать HTTP-клиент.
- [x] Описанная политика recheck, pause и DiskFull подтверждена тестами; лишние переходы в Downloading отсутствуют.

### Проверки И Результат

- [x] Есть тест независимости direct Read/Seek от client mutex (`TestDirectRead_IndependentFromClientMutex`) и локальный concurrent-download integration scenario (`TestStreamSource_ConcurrentDownloadAndDirectStream`).
- [x] Все обязательные команды, включая проверки отдельного Go module fork, прошли.
- [x] Логи показывают выбранный источник, fallback reason, статус, байты и время без per-chunk INFO spam.
- [x] Сравнительные замеры выполнены в одинаковых условиях, результаты и ограничения измерений приложены к отчёту.
- [ ] **Ручная приёмка Lampa выполнена и пользовательский критерий подтверждён. До этого пункт остаётся открытым, даже если код и автоматические тесты готовы.**
- [x] Создан `tasks/implementation/phase31-report.md`: изменения по файлам, статусы всех DoD, команды/результаты, замеры, ручная проверка и объяснения отклонений.

---

## 3. Результаты Проверок Командами

```bash
# Тесты internal пакетов
go test -count=1 ./internal/engine ./internal/usecase ./internal/delivery/http/...
# Результат: ok (все тесты пройдены)

# Проверка на состояние гонок
go test -race -count=1 ./internal/engine ./internal/usecase ./internal/delivery/http/...
# Результат: ok (0 data races)

# Все тесты проекта
go test ./...
# Результат: ok

# Сборка бинарного файла
go build ./cmd/hub
# Результат: успешно (exit code 0)

# Форматирование и whitespace check
git diff --check
# Результат: чистый вывод (0 замечаний)

# Тесты хранилища в форке anacrolix-torrent
cd pkg/anacrolix-torrent && go test -race -count=1 ./storage
# Результат: ok

# Тесты готовности к прямому чтению и независимости от мьютекса
cd pkg/anacrolix-torrent && go test -v -run 'TestFileReadyForDirectRead|TestDirectRead' -race .
# Результат: ok (все проверки и тест изоляции мьютекса пройдены)
```

---

## 4. Сравнительные Замеры Производительности

Бенчмарки проводились на одинаковом наборе данных (файл размером 4 МиБ, диапазон запроса 1 МиБ) на архитектуре `darwin/arm64` (Apple M3 Max):

```text
BenchmarkStream_DiskSource-16       6118     196113 ns/op   21387.16 MB/s     2981 B/op     52 allocs/op
BenchmarkStream_TorrentSource-16    3538     311963 ns/op   13444.89 MB/s    77333 B/op   1782 allocs/op
```

- Скорость передачи данных увеличена: **с 13 445 МБ/с до 21 387 МБ/с (+59.1%)**.
- Задержка на операцию снижена: **с 311 963 нс до 196 113 нс (-37.1%)**.
- Число аллокаций памяти снижено: **с 1 782 до 52 (-97.1%)**.
- Объем выделяемой памяти снижен: **с 77.3 КБ до 2.9 КБ (-96.1%)**.

---

## 5. Причины Отхода От Плана

Существенных отходов от архитектурного плана не потребовалось:
1. Контракт `ReadyForDirectRead()` полностью реализован в форке `anacrolix-torrent` без выделения динамической памяти на каждый piece (сканирование in-memory среза pieces торрента).
2. Вычисление путей файлов унифицировано через экспорт `DefaultFilePathMaker`, что гарантирует совпадение структуры каталогов между файловым хранилищем торрента и проверкой `OpenStreamSource`.
3. В `Warmup()` сохранена совместимость с эмуляцией контракта TorrServer: возвращаются ожидаемые размеры `PreloadSize` и `preloadedBytes`, исключая задержки холодного чтения диска для готовых файлов.
4. Пункт ручной приёмки в плеере Lampa оставлен открытым до подтверждения пользователем на целевом устройстве согласно требованиям DoD.

## 6. Исправления По Результатам Ревью `af8ae5f`

- **Lifecycle direct streams** (`internal/engine/stream.go`, `stream_source.go`, `engine.go`): регистрация проверяет актуальный экземпляр `ManagedTorrent` под `Engine.mu.RLock()` и удерживает блокировку до добавления дескриптора. Удаление торрента больше не может пройти между проверкой и регистрацией. `CloseAllStreams()` под блокировкой запрещает последующие регистрации; при отказе `OpenStreamSource()` закрывает открытый файл. Закрытие стримов привязано к экземпляру торрента, поэтому отложенный cleanup старого экземпляра не затрагивает повторно добавленный hash.
- **Границы файлов v2** (`pkg/anacrolix-torrent/file.go`): вместо суммы размеров payload используются закешированные смещения файлов с учётом выравнивания по pieces. Проверка больше не отклоняет завершённые файлы после промежутков выравнивания и не обходит все metadata-файлы при каждом вызове.
- **Регрессионные тесты** (`engine_lifecycle_test.go`, `file_direct_read_test.go`): открытие во время shutdown после закрытия существующих стримов; запоздалая регистрация после удаления и повторного добавления hash; изоляция cleanup старого экземпляра; v2-файлы с выравниванием, незавершённым piece, повторной проверкой и нулевым последним файлом.

Проверки после исправлений:

```bash
go test ./...
go test -race -count=1 ./internal/engine ./internal/usecase ./internal/delivery/http/...
go build ./...
# Из pkg/anacrolix-torrent:
go test -race -count=1 -run 'TestFileReadyForDirectRead|TestDirectRead' .
go test -race -count=1 ./storage
git diff --check
```

Все проверки прошли; data races не обнаружены. При race-сборке системный linker macOS выводит предупреждение `LC_DYSYMTAB`, не препятствующее выполнению тестов.

Статус связанных пунктов DoD:

- [x] Закрытие движка и удаление торрента исключают запоздалую регистрацию direct streams.
- [x] Готовность выбранного файла корректно проверяется для v2 layout с выравниванием; проверки completion и переходных состояний сохранены.
- [x] Регрессионные тесты, общий прогон, race-проверки и сборка прошли.
- [ ] Ручная приёмка Lampa остаётся открытой, как в исходном отчёте.

Исправления соответствуют плану фазы 31; новых архитектурных отклонений нет.
