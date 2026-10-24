# Фаза 40: Надёжность хранения, ограничения ресурсов и согласованность состояния

## Статус и зависимости

- Статус: **реализация завершена; исправление обнаруженного Windows CI сбоя локально проверено 24 октября 2026 года**. Устранена утечка mmap storage, вызвавшая ошибки удаления файлов в `Native storage (windows-latest)`; успешный повторный нативный прогон после исправления ещё не подтверждён. Результаты и ограничения: `tasks/implementation/phase40-report.md`.
- Основание: полное ревью проекта от **23 октября 2026 года** — 15 дефектов и восемь архитектурных замечаний. Содержание находок и сценарии приёмки закреплены ниже; для выполнения фазы не требуется история чата или временные тесты из `/tmp`.
- Предусловие: завершённая [фаза 39](phase39-logging-state-and-lifecycle-architecture.md) и её [итоговый отчёт](implementation/phase39-report.md). Перед реализацией проверить актуальное состояние репозитория и повторить baseline.
- Это одна фаза с последовательными внутренними блоками и общим DoD. Следующий блок начинается после проверки затронутых инвариантов предыдущего; зелёный отдельный блок не закрывает фазу.
- Отчёт после реализации или значимого блока: `tasks/implementation/phase40-report.md`. Фазу 41 автоматически не начинать.

## Цель и границы

Устранить все перечисленные дефекты, сделать восстановление файлов и применение команд повторяемыми, ограничить нагрузку на NAS и разорвать зависимость публикации состояния от длительных операций одного торрента. Архитектурные изменения должны закрывать конкретные причины ошибок и иметь измеримые критерии приёмки.

В scope входят Go, SQLite, локальный форк `anacrolix/torrent`, Vue, конфигурация, миграции, CI, документация и детерминированные регрессионные тесты. Старые F09/F11/F12 из [аудита 21 октября](draft/codebase-audit-2026-10-21.md), исключённые из фазы 39, здесь входят в scope как F40-04/F40-05/F40-07.

Не входят внедрение auth, смена torrent-библиотеки, обновление зависимостей без связи с находками, actor-модель, отдельный брокер, event sourcing, полная генерация API и переработка дизайна GUI. Исправление границ storage и команд не требует переписывания всего проекта. Удалённые hard refresh/client recycle не возвращать как новый механизм восстановления: привести UI и конфигурацию к поддерживаемому поведению.

## Реестр дефектов

P1 — высокий приоритет исправления; P2 — ошибка отдельного сценария; P3 — несогласованность интерфейса. Номера соответствуют порядку находок в ревью.

| ID | Приоритет | Дефект и исходная точка | Блок |
| --- | --- | --- | --- |
| F40-01 | P1 | V2 multi-file отвергается из-за `Offset == sum(Size)` в `internal/repository/catalog.go`. | 2 |
| F40-02 | P1 | Прерывание `Link → Remove` оставляет два имени, отвергаемых как unsafe hardlinks; `pkg/anacrolix-torrent/storage/file-io-root.go`. | 3 |
| F40-03 | P1 | Duplicate Add с metadata увеличивает revision дважды, а `bindIncrementalRuntime` допускает только один шаг; `internal/usecase/temporary.go`, `observations.go`. | 2 |
| F40-04 | P1 | Private-торренты получают публичные retrackers и явные DHT announces; `internal/engine/trackers.go`, `swarm_watchdog.go`, форк `torrent.go`. | 7 |
| F40-05 | P1 | Неограниченный body `/cache`, multipart spill и время чтения тела; HTTP handlers и `cmd/hub/main.go`. | 8 |
| F40-06 | P1 | `MaxActiveStreams` читается, но не ограничивает admission; `internal/config/config.go`, `internal/engine/stream.go`. | 5 |
| F40-07 | P1 | HTTP resolve позволяет SSRF через исходный URL, DNS и redirects; `internal/delivery/http/torrserver/handlers.go`. | 8 |
| F40-08 | P1 | Worker ждёт record lock без отмены; main ожидает workers до отмены engine; `sync_worker.go`, `torrent_uc.go`, `cmd/hub/main.go`. | 5 |
| F40-09 | P2 | Локальные `confirmedPriorities` перекрывают более новую серверную revision; `web/src/stores/torrentStore.ts`. | 9 |
| F40-10 | P2 | HEAD создаёт playback debounce и блокирует Delete/cleanup; `torrserver/handlers.go`, `internal/usecase/temporary.go`. | 5 |
| F40-11 | P2 | Upload limiter burst равен B/s и отвергает обычный блок 16 KiB при малом лимите; `client_config.go`, форк `peerconn.go`. | 7 |
| F40-12 | P2 | Swarm boost не истекает, `boostUntil` не устанавливается; `swarm_watchdog.go`, `health.go`. | 7 |
| F40-13 | P2 | Имя файла в M3U URL не экранируется, `#` и `?` ломают ссылку; `torrserver/handlers.go`. | 8 |
| F40-14 | P3 | UI/API client/config описывают отсутствующие hard refresh и client recycle; `BTHealthPanel.vue`, `client.ts`, config. | 9 |
| F40-15 | P2 | Форк объявляет Go 1.24, хотя использует отсутствующие там методы `os.Root`; `pkg/anacrolix-torrent/go.mod`. | 1 |

F40-01/02/03/09/10 воспроизведены тестами при ревью. Для F40-08 воспроизведено неотменяемое ожидание worker; зависание полного процесса следует отдельно проверить через lifecycle integration test. Для F40-02 моделировалось состояние файлов между `Link` и `Remove`, физическое отключение питания не проводилось. Остальные находки подтверждены разбором соответствующих путей кода и сборкой для F40-15.

## Реестр архитектурных задач

| ID | Недостаток | Требуемый результат | Блок |
| --- | --- | --- | --- |
| A40-01 | Обход всех verified pieces при записи, повторные проходы при завершении файла, filesystem I/O под client lock. | Инкрементальный учёт, проверки по уникальным файлам и отсутствие массового stat/hash под общим lock. | 4 |
| A40-02 | SSE публикация ждёт последовательного persistence и record locks всех торрентов. | Независимые publication/persistence с ограниченными очередями и безопасным последним snapshot занятой записи. | 6 |
| A40-03 | Двойное полное чтение БД с DeepEqual, повторная запись неизменяемого каталога, тяжёлые полные DTO. | Версии каталога и read model, запись изменений, краткая публикация и отдельная загрузка деталей. | 6 |
| A40-04 | Правила commit/apply/bind/retry распределены между командами. | Единый механизм исполнения с явным результатом и сохранением fencing/reconcile. | 2 |
| A40-05 | Usecase и payload зависят от конкретных объектов и правил storage форка. | Узкие интерфейсы, один владелец layout/ownership/publication и проверки готовности. | 3 |
| A40-06 | Bolt completion cache добавляет вторую БД и mmap при требовании SQLite-only. | SQLite backend completion с ограниченным кэшем, безопасной миграцией и общей политикой lifecycle. | 3 |
| A40-07 | Нет общего бюджета задач и HTTP transfers; cancellation не охватывает все ожидания. | Ограниченный admission, объединение повторных задач и согласованный lifecycle без потери обязательных эффектов. | 5 |
| A40-08 | Makefile/CI пропускают Vitest и отдельный Go-модуль, нет обязательной PR-проверки. | Воспроизводимые проверки обоих модулей, frontend и платформенного storage. | 1, 10 |

## Сохраняемые гарантии

1. SQLite WAL и per-connection PRAGMA, настроенный пул, короткие транзакции. SQLite и filesystem не объявляются одной транзакцией. Фаза не вводит другую БД.
2. Isolated payload roots/aliases, rooted open/write/unlink, no-replace publication, migration/deletion journals и защита чужих и служебных файлов сохраняются.
3. Только проверка текущего открытия разрешает чтение/upload/cache readiness. Persisted completion, имя без `.part`, размер, snapshot или revision не являются доказательством готовности.
4. Sticky DiskFull снимается только явным Resume после необходимых проверок и persistence. Повторный Add, приоритет, discovery, recheck и чтение не снимают блокировку.
5. Сохраняются Normal/High/Skip, FIFO и лимит фоновых загрузок, selected completion без ложного Seeding, reference counting и seek debounce. Лимиты HTTP transfers отделены от фоновых загрузок и QoS overlay.
6. Generation/record identity/revision fencing действует для callbacks, snapshots, reservations и releases. Старое событие не меняет повторно добавленный тот же hash.
7. Endpoint names, TorrServer-совместимость, HTTP Range/HEAD и CORS `*` сохраняются. Ошибка commit, admission или обязательного apply не превращается в успешный ответ.
8. Нельзя закрывать БД/storage под работающей задачей ради формального соблюдения timeout. Непрерываемый syscall явно отделяется от отменяемого ожидания приложения.

## Блок 1 — Baseline, toolchain и регрессии

- Проверить DoD/отчёт фазы 39 и рабочее дерево. Зафиксировать версии Go/Node, команды и результаты исходной приёмки.
- Согласовать минимальный Go root/fork/CI/Docker с используемым API. Текущая рабочая база — Go 1.25.8; самостоятельная сборка форка не должна зависеть от запуска из корневого модуля.
- Перенести шесть воспроизведений в постоянные тесты соответствующих пакетов, проверяющие правильный конечный результат. Не сохранять тесты, которые считают существующий баг ожидаемым успехом. Добавлять и исправлять regression вместе в блоке, не оставляя main suite постоянно красным.
- Подготовить локальные doubles для resolver/dialer/DHT, controllable clock, filesystem fault points и coordinator barriers. Тесты не используют публичные трекеры, случайные сетевые тайминги или реальные секреты.
- Зафиксировать baseline CPU/allocations/числа filesystem и SQL операций для задач A40-01/A40-03. Это исходные измерения для сравнения, а не заранее доказанное замедление на NAS.

**Приёмка:** корневой модуль и проверяемые пакеты форка собираются с объявленным toolchain; каждый дефект имеет конкретный regression-сценарий в этом плане. Ошибки baseline отделены от новых регрессий.

## Блок 2 — Каталог v2 и единый протокол команд

Основные файлы: `internal/models`, `internal/repository/{catalog,intent,control}.go`, `internal/usecase/{temporary,observations,torrent_uc}.go`, `internal/usecase/STATE.md`, engine bindings и тесты.

- Сделать каталог явно описывающим формат/layout metainfo: v1, v2, hybrid; хранить реальные file offsets. Payload size, span адресуемых pieces и padding не смешиваются. Правила проверяются на zero-length, shared boundary, padding, overflow и некорректных offsets.
- Для существующих записей применить безопасную миграцию: формат/offsets восстанавливаются из валидного metainfo либо остаются неизвестными до получения metadata; отсутствие сведений не подменять v1-догадкой. Сохранить индексы, пути, priorities, storage aliases и прогресс с его provenance.
- Объединить составное изменение metadata/permanence при Duplicate Add в одну intent-транзакцию. Семантически неизменяющая команда не создаёт лишнюю revision. Для предыдущего failed apply no-op не считается подтверждением его выполнения.
- Ввести общий механизм `commit intent → apply нужных effects → bind ожидаемую revision → result`. Он сохраняет отличия полной reconciliation и частичного изменения. Не заменять проверку `revision-1` безусловным продвижением до последней версии.
- Перевести существующие пути Add/metadata/permanence/priority/Pause/Resume на этот контракт либо общий явно типизированный результат. Durable delete/migration сохраняют специализированные workflows, согласованные с теми же identity/fencing правилами.
- При commit/apply/bind failure сохранить безопасный pending/error и возможность retry/restart. Ответ API и snapshot не сообщают applied revision, которой runtime не достиг. Старое observation не может затереть более новую команду.

**Приёмка:** многофайловый v2 добавляется файлом и через magnet, переживает restore и сохраняет правильные offsets; v1/hybrid не регрессируют. Duplicate Add с новой metadata не оставляет вечный pending и допускает последующий sync. Тесты покрывают no-op, временный/постоянный torrent, DB-only record, предыдущее failed apply, сбои каждого шага и delete/re-add.

## Блок 3 — Граница storage, восстановление публикации и SQLite completion

Основные файлы: `internal/payload`, engine storage/metainfo/verification adapters, repository migrations, `pkg/anacrolix-torrent/storage`, `cmd/hub/main.go`.

- Определить узкие прикладные интерфейсы каталога, открытия чтения, verification и публикации завершённого файла. Владение rooted layout и проверками filesystem сосредоточить в storage adapter; usecase не получает сырой `*torrent.File`/client для обхода этих гарантий. Уже существующие helpers использовать вместо второго независимого набора проверок.
- Записать протокол завершения `.part`: prerequisites, точка публикации, порядок sync, recovery и конфликт с чужим target. Предпочесть атомарный no-replace primitive там, где он доступен. Для многошагового fallback требуется durable подтверждение ownership и идемпотентное восстановление; одной проверки «имена указывают на один inode» недостаточно для удаления.
- Обработать существующее промежуточное состояние `part + final`: доказанно собственная операция восстанавливается, чужой target/дополнительная внешняя ссылка не перезаписываются и не удаляются. Если старого evidence недостаточно, вернуть понятный конфликт и описать безопасное ручное восстановление без потери обеих копий.
- Проверить поведение filesystem без hardlinks. Поддерживаемый fallback должен сохранять no-replace/ownership; неподдерживаемый вариант диагностируется до разрушительных изменений. Требования к filesystems описать явно.
- Заменить production `NewBoltPieceCompletion` на SQLite completion backend, внедряемый через composition root. Использовать управляемый пул/WAL, корректную identity v1/v2/alias и отдельное пространство cache records. Engine не открывает скрытую вторую БД.
- Persisted completion остаётся hint: старый Bolt можно оставить нетронутым и перестроить cache после проверки payload. Не импортировать старый `complete=true` как разрешение чтения. Удаление legacy cache не является обязательной частью обновления.
- Не переносить per-piece SQLite I/O под общий library lock. Разрешён ограниченный memory cache с объединением записи hints; переполнение/ошибка persistence не превращает непроверенные bytes в готовые и не теряет пользовательский intent. Зафиксировать лимиты памяти, порядок drain/close и диагностику ошибок.
- Новый SQLite schema upgrade повторяем; отказ commit/restart сохраняет intent, mapping и journals. Завершение сервиса закрывает completion adapter до его БД. Описать backup и ограничения downgrade.

**Приёмка:** fault injection до/после каждого шага публикации, ошибка Remove, повторный reopen, conflicting target, symlink и чужой hardlink; данные не теряются, доказанно собственные промежуточные состояния восстанавливаются. Completion backend проходит временную SQLite integration suite, reopen/read-only/full/failure tests; production не открывает Bolt. После любого restart готовность подтверждается новой проверкой.

## Блок 4 — Стоимость verification и короткие library locks

Основные файлы: форк `storage/file-torrent.go`, `storage/file-piece.go`, `torrent.go`, `file.go`, engine verification/resource monitoring.

- Заменить обход всех verified pieces на каждую chunk write индексом затронутых файлов/диапазонов и инкрементальным учётом. Счётчик готовых pieces файла обновляется при verify/invalidate, а не вычисляется заново на каждом MarkComplete.
- Собирать signature по уникальным файлам и использовать поколения/invalidation. Внутренняя запись в missing range не отменяет чужие проверенные ranges; внешнее изменение inode/size/mtime по-прежнему отзывает доверие. Поколение приложения само по себе не доказывает отсутствие внешней записи.
- Вынести массовые stat/hash и ожидания storage из общего client lock: снять bounded snapshot, выполнить I/O, повторно проверить generation и применить результат. Старый результат не делает заменённый файл готовым.
- Сохранить проверки на границах открытия/выдачи данных. Не добиваться производительности отключением fresh verification, чтением непроверенного файла или заменой ошибки на `complete=true`.
- Добавить счётчики операций и benchmarks на 1k/10k/100k pieces. Для последовательного завершения файла bookkeeping должен быть линейным по pieces, а не суммой полных проходов. Один freshness-проход делает число stat, пропорциональное уникальным файлам, а не числу pieces.
- Проверить отдельный torrent B при заблокированном storage A: управление и получение доступной телеметрии B не ждут массовой проверки A.

**Приёмка:** детерминированные тесты сложности и отсутствия глобального ожидания проходят; benchmarks фиксируют CPU/allocations до/после без универсального обещания скорости NAS. Corruption/replacement/truncation и v1/v2/shared boundary тесты сохраняют гарантии verified-only.

## Блок 5 — Lifecycle, ограниченные задачи и HTTP transfers

Основные файлы: `cmd/hub/main.go`, usecase coordinator/temporary/read preparation, engine task/stream lifecycle, API и TorrServer transfer handlers, `internal/engine/CONCURRENCY.md`.

### Отмена и завершение

- Разделить остановку на запрет новой работы, отмену service/generation/request contexts, остановку subscriptions/timers, join принятых задач и закрытие ресурсов. Main не ждёт SyncWorker до отмены engine-owned операций, от которых worker зависит.
- Передавать контекст в получение record/root handles, capture/persistence waits, подготовку скачивания и verification. Убрать дочерний `context.Background()` там, где операция должна прекращаться вместе с запросом или сервисом.
- Ожидание lock/queue/controlled I/O завершается по cancellation; drain не держит lock, нужный завершаемой задаче. Повторные Close/cancel/release безопасны.
- Проверить весь `run`/shutdown путь: занятый record, paused-file verification, SSE, stream и cleanup. Deadline ограничивает ожидания приложения; не закрывать storage под непрерываемым syscall. Ограничение ОС и выбранное поведение после deadline отражаются в отчёте.

### Бюджет задач

- Инвентаризировать `StartTask`, callbacks, watchers и timers. Разделить ограниченные workers для коротких событий, тяжёлой verification и долгоживущих stream watchers; ожидание одного класса не забирает все слоты другого.
- Ограничить pending jobs и retained payload, объединять повторные refresh/stat/observation по generation/key. Значения defaults и смысл насыщения фиксируются в config/документации до изменения вызывающих путей; bounded означает численный предел, а не только WaitGroup.
- Обязательные safety effects не теряются при насыщении: disk/storage error синхронно закрывает gate, последующая запись/повтор имеет учтённый путь. Для принятой команды ошибка очереди возвращается вызывающему коду или сохраняется как pending, а не успех.
- После shutdown/Delete нельзя зарегистрировать новую задачу старого поколения. Никакого goroutine-per-retry или бесконечного overflow fallback.

### Слоты и HEAD

- Применить `MaxActiveStreams` как общий предел активных пользовательских HTTP payload transfers: TorrServer stream/play и native file/archive download, включая direct disk path. Один HTTP response занимает один слот; файлы внутри архива не получают дополнительные слоты. Preload сохраняет собственные ограниченные budgets.
- Default — 4, значение должно быть положительным; ноль/отрицательное значение не превращается в unlimited. Резервирование происходит до дорогой подготовки чтения, атомарно между endpoints.
- Исчерпание лимита возвращает 429 до отправки тела; остановка сервиса — 503. Обработчики не игнорируют отказ регистрации и не обходят лимит fallback-путём. Ошибка подготовки, отмена, EOF, Range failure, Delete и Close освобождают слот ровно один раз.
- Слот живого HTTP transfer освобождается при завершении response; QoS debounce продолжает защищать seek независимо и не удерживает слот десять секунд. Новый Range-запрос при лимите 1 может занять освободившееся место.
- HEAD использует короткую metadata/read lease на время запроса, без payload slot, playback accounting и последующего debounce. Lease сохраняет безопасность относительно concurrent Delete; после её освобождения HEAD не препятствует удалению.

**Приёмка:** лимит соблюдается при конкурентных запросах разных endpoints и обоих источников; ошибки не оставляют reservations. HEAD не изменяет активность/приоритеты и не блокирует последующее Delete. SyncWorker прерывает ожидание record lock по cancel; полный shutdown проходит без задержки до освобождения такого lock. Saturation tests доказывают численные границы и сохранение safety effects.

## Блок 6 — Независимая публикация и экономный read model

Основные файлы: `internal/usecase/{sync_worker,consistent_read,observations}.go`, repository read/catalog/observation methods, REST/SSE projections, frontend API/store.

- Разделить публикацию телеметрии и сохранение observations. Publisher не ждёт последовательного commit или долгого record handle каждого torrent. Занятая запись даёт последний разрешённый snapshot с явной свежестью; остальные записи обновляются.
- Кэш snapshot проверяет record identity, deletion tombstone, desired/applied revision и attachment. Нельзя публиковать удалённую запись или старые намерения только ради доступности. Если безопасной версии нет, использовать явно unavailable/pending состояние; не подменять его пустым списком.
- Persistence выполняется ограниченной очередью с latest-only объединением по записи и fencing. Устаревшая задача не пишет в новый record; ошибка видна, retry ограничен. Очередь observations не поглощает command intent.
- Заменить двойную материализацию БД и DeepEqual проверкой компактных версий/consistent read transaction. Версии intent/catalog/presentation отделены от высокочастотной telemetry; только изменение релевантной версии требует retry.
- Не сохранять неизменяемый каталог каждые две секунды. Catalog version меняется при реальном изменении metadata; observation пишет изменившиеся поля с ограниченной частотой, а не безусловно обновляет все files.
- Ввести краткую модель списка/событий без полного каталога и piece arrays; GUI загружает детали выбранного torrent по версии. Существующие REST/TorrServer ответы сохраняют совместимость; компактный формат SSE вводится явной подпиской/версией, а не молчаливой заменой старого ответа.
- Численно ограничить retained snapshot/queue buffers и subscriber mailboxes. Один медленный SSE-клиент не тормозит публикацию или persistence. Existing heartbeat/deadline/latest-only поведение сохраняется.

**Приёмка:** блокировка record/SQLite write для A не прекращает безопасную публикацию свежего B. Delete/re-add, command во время capture и рестарт с новым epoch не воскрешают старое состояние. SQL counters показывают отсутствие повторных catalog/file writes при неизменных данных; compact payload не растёт с полным каталогом нераскрытых торрентов.

## Блок 7 — Private discovery, upload limiter и swarm boost

Основные файлы: engine trackers/watchdog/client config/health, форк DHT/tracker/PEX paths.

- Ввести единую discovery policy на основании metadata. Для известного private torrent не добавлять публичные retrackers, не запускать DHT/PEX и не заменять исходные private trackers; явный refresh не обходит policy.
- Проверить стандартные и ручные пути библиотеки, metadata arrival и уже запущенные discovery tasks. При обнаружении `private=1` остановить запрещённую активность и не переносить полученные публичные peers автоматически в разрешённый private swarm.
- Для magnet с неизвестным private-флагом описать неизбежную границу: флаг неизвестен до metadata. Не обещать ретроактивное отсутствие announce; предусмотреть явный private-safe режим добавления, если discovery должен быть запрещён с первого запроса. Известный private `.torrent` защищён сразу.
- Отделить burst upload limiter от sustained B/s: burst вмещает поддерживаемый максимальный protocol block, а токены ограничивают среднюю скорость. Низкий лимит не становится протокольной ошибкой и не создаёт обхода общего upload budget.
- Сделать boost ограниченным сроком: установить `boostUntil`, определить продление при повторном trigger, восстановить обычный connection limit при expiry. Pause/Delete/re-add и изменения policy не получают эффект от старого timer. Health показывает фактический режим и срок.

**Приёмка:** fake tracker/DHT/PEX не получают запрещённых вызовов для private; public discovery работает. При 1 KiB/s обычный блок 16 KiB обслуживается с rate limiting без reject/disconnect. Fake-clock тест подтверждает включение/истечение boost и защиту от старого timer.

## Блок 8 — HTTP budgets, безопасное получение ссылок и M3U

Основные файлы: HTTP router/handlers, конфигурация, `resolveHTTPLink`, upload paths и README.

### Входящие тела и временные файлы

- Задать route-specific ограничения до parsing. Начальный контракт: JSON/form control body — 1 MiB; `.torrent` — 32 MiB; multipart envelope — 34 MiB; чтение входящего control/upload body — до 30 секунд. Значения централизованы и уменьшаются в тестах; ошибочный config не отключает лимит.
- Ограничить одновременно принятые upload/remote-metainfo jobs: default 2. Temp spill находится в управляемом каталоге и имеет общий budget, согласованный с лимитом jobs и размером envelope; учитываются заголовки/поля multipart. При отказе admission тело не вычитывается без ограничений.
- Применить `MaxBytesReader` либо эквивалент на всех соответствующих endpoints, включая `/cache`; для `.torrent` проверить фактический размер независимо от `Content-Length`. Too large — 413, slow body — 408, насыщение — 429; CORS сохраняется и на ошибках.
- Гарантировать cleanup temp files после успеха, parse error и cancellation. Ошибка temp-disk write не маскируется и не снимает payload DiskFull. Не считать проверку свободного места заменой byte budget.
- Не вводить глобальный короткий WriteTimeout для больших payload responses/SSE. Deadline чтения служебного тела не ограничивает длительность воспроизведения.

### Исходящие HTTP-ссылки

- Вынести fetcher с context, resolver/dialer policy и ограниченным числом redirects; default — не более 5 переходов и 60 секунд на всю операцию. Принимать только HTTP/HTTPS, проверять каждый переход и ограничивать body `.torrent` до 32 MiB с чтением `limit+1` для обнаружения превышения.
- По умолчанию запрещать loopback, private, link-local, unspecified и multicast назначения, включая IPv6 и IPv4-mapped адреса. Для легитимных LAN-источников добавить явную allowlist host/port/CIDR; доступ к одному разрешённому источнику не разрешает произвольный redirect.
- Проверять адрес фактического соединения и не выполнять повторное непроверенное DNS resolution после проверки. Разрешение hostname само по себе не разрешает смену адреса на запрещённый. TLS сохраняет проверку имени/сертификата.
- Явно определить proxy policy: не наследовать environment proxy, через который target policy обходится, без отдельной настройки и проверки. Не пересылать чувствительные заголовки между origin; не логировать секреты URL.
- Redirect на magnet обрабатывается как проверенная смена типа результата, а не попытка сетевого запроса с произвольной схемой. Отмена исходного запроса завершает fetch; ошибки policy/limit имеют понятный ответ.

### Плейлисты

- Формировать M3U URL через корректное кодирование path/query. Обработать `#`, `?`, `%`, пробелы и Unicode без двойного escaping; удалить CR/LF из отображаемого EXTINF label.
- Сохранить TorrServer индексацию и маршруты; тест должен разобрать сгенерированный URL и выполнить запрос к реальному handler.

**Приёмка:** chunked oversized bodies, ложный/отсутствующий Content-Length, медленное тело, temp write error и cancellation не выходят за budgets и не оставляют файлы. SSRF tests используют local doubles: исходный запрещённый адрес, redirect, DNS rebinding, IPv6, разрешённый LAN host, proxy и magnet redirect. M3U для специальных имён остаётся воспроизводимым.

## Блок 9 — UI, версии команд и поддерживаемые настройки

Основные файлы: `web/src/{stores,api,types,components}`, backend health DTO, config, README/compose/Makefile examples.

- Привязать optimistic/confirmed priority к record identity, command receipt и snapshot version. После более нового авторитетного состояния локальное переопределение снимается даже при отличающемся значении; старая версия его не снимает.
- Согласовать file details cache с compact snapshots блока 6: обновление catalog/selection version инвалидирует нужные детали. Несколько локальных команд, out-of-order REST/SSE, другой клиент, failure, новый epoch и delete/re-add имеют определённую семантику.
- Убрать неработающие hard refresh/recycle кнопки и client methods, неподдерживаемые health fields и вводящие в заблуждение примеры конфигурации. Если legacy env/flags принимаются для совместимости, выдавать явное предупреждение о deprecated/no-effect и документировать срок/правило удаления; они не должны выглядеть действующей настройкой.
- Поддерживаемые health/config contracts проверять через fixture/contract tests backend → frontend. Полная генерация схем не обязательна; ручные типы не содержат фиктивных доступных операций.
- Показывать понятные ошибки 413/429/policy rejection и корректные pending/last-known состояния без бесконечных автоматических повторов.

**Приёмка:** два клиента последовательно меняют приоритет — GUI принимает новое значение без reload. Поздний старый snapshot не откатывает свежую команду. Диагностика содержит только поддерживаемые операции и корректно отображает фактический boost; лимиты и отказы видны пользователю.

## Блок 10 — CI, документация и итоговая приёмка

- Добавить PR workflow с минимальными read permissions, pinned проектными Go/Node и lockfile install. Проверять root и отдельный fork module явно; корневой `go test ./...` не считается покрытием вложенного модуля.
- Makefile должен давать отдельные воспроизводимые targets для root tests, fork suites, Vitest, race и общей проверки. Обычный `make test` включает frontend tests, а не только build. Тесты не требуют публичного swarm.
- Минимальная обязательная матрица: полный root suite и race, fork `.`/`./storage` с race и все дополнительные затронутые пакеты, frontend tests/build, vet/build. Состав fork suites закрепить явно; расширять его при изменении других пакетов, а не объявлять весь fork проверенным по двум пакетам.
- Linux runner выполняет integration suite. Storage publication/reopen/ownership tests выполняются на macOS и Windows для заявленных поддерживаемых реализаций; unsupported возможности отражаются точными условиями, не общим skip всех проверок. Cross-build release targets дополняет runtime tests, но не заменяет их.
- Release использует те же проверяемые targets до публикации binary/image. В этой фазе создаётся код workflow; изменение серверных branch-protection rules или публикация релиза не требуется для локального выполнения задачи.
- Актуализировать README, config examples, `internal/usecase/STATE.md`, `internal/engine/CONCURRENCY.md`: command/recovery contract, SQLite completion, supported filesystems, API budgets, LAN allowlist, stream limits, HEAD, private discovery и shutdown.
- Создать `tasks/implementation/phase40-report.md`: изменения по каждому F40/A40, выполненные команды, результаты, миграции, performance counters/benchmarks, DoD, отклонения и реальные платформенные ограничения. Для CI, которое нельзя запустить локально, различать готовую конфигурацию и фактически полученные результаты runner.

Минимальные команды итоговой проверки; запускать из указанной директории с согласованным toolchain. Сначала собрать frontend, затем проверять встраивающий его backend.

| Директория | Команда | Назначение |
| --- | --- | --- |
| `web` | `npm ci` | Воспроизводимая установка из lockfile. |
| `web` | `npm test` | Все frontend regression tests. |
| `web` | `npm run build` | TypeScript и production assets. |
| Корень | `go test -count=1 ./...` | Полная root suite без cached результата. |
| Корень | `go test -race -count=1 ./...` | Конкурентность root packages. |
| Корень | `go vet ./...` | Статические проверки root packages. |
| Корень | `go build -o bin/torrent-stream-hub ./cmd/hub` | Сборка приложения со встроенным frontend. |
| `pkg/anacrolix-torrent` | `go test -count=1 . ./storage` | Минимальный обязательный fork suite. |
| `pkg/anacrolix-torrent` | `go test -race -count=1 . ./storage` | Fork concurrency regression. |
| `pkg/anacrolix-torrent` | `go vet . ./storage` | Статические проверки затронутого ядра форка. |

Для изменений вне этих двух пакетов форка расширить список команд соответствующими пакетами. Benchmarks A40-01/A40-03 и OS-specific storage suites добавить к этой матрице с точными именами тестов после реализации; общий успешный root run их не заменяет.

## Обязательные сценарии общей приёмки

| Область | Проверяемый сценарий и результат |
| --- | --- |
| Каталог | V1/v2/hybrid multi-file, два v2 файла по 1024 байта с piece 16384, zero-length и padding; file/magnet/restore сохраняют реальные offsets. |
| Команды | Duplicate Add + metadata + permanence, prior failed apply, no-op, concurrent priority/Pause и commit/bind failures; applied revision отражает только фактически выполненное. |
| Файлы | Прерывание публикации на каждой границе, Remove failure, reopen, чужой target и дополнительные hardlinks; нет перезаписи/потери данных и ложной readiness. |
| Completion | SQLite migration/reopen/failure; legacy Bolt не требуется и не авторизует данные; startup verification и corruption revocation сохранены. |
| Стоимость | 1k/10k/100k pieces; нет квадратичного completion bookkeeping, массового stat под client lock и catalog writes на неизменном snapshot. |
| Lifecycle | Отмена record wait и queued task, blocked verification, SSE/stream/cleanup, repeated Close и late callback; shutdown не ждёт ресурс, отмена которого выполняется позже. |
| Admission | N конкурентных transfers разрешены, N+1 отклонён до expensive preparation; все exit paths освобождают слот; HEAD и seek не создают утечек. |
| Publication | Долгая операция/SQLite write A, свежий B, slow subscriber, delete/re-add и command during capture; безопасный B публикуется без ожидания A. |
| Discovery | Private known/metadata arrival/private-safe add; публичные tracker/DHT/PEX запрещены там, где private известен или задан явно. |
| Сеть | Upload 1 KiB/s обслуживает 16 KiB block; boost истекает по fake clock и не действует на новое поколение. |
| HTTP | Oversized/slow/chunked тела, temp exhaustion/cancel, SSRF redirects/DNS/proxy/IPv6 и разрешённый LAN; budgets соблюдены, ошибки наблюдаемы. |
| UI | Два клиента, пропущенный промежуточный snapshot, несколько команд, failure и epoch change; локальная версия не перекрывает более новую. |
| Совместимость | Range/HEAD/416, M3U special names, TorrServer indexing, CORS на успехах/ошибках, FIFO/Skip/DiskFull/delete modes и startup restore. |

Go regression tests используют barriers/channels и controlled clocks вместо длительных sleeps. БД проверяется на временном SQLite, filesystem — в изолированных roots. Test assertions подтверждают наблюдаемое поведение и реальные gates/bytes, а не только внутренние счётчики. Нагрузочные проверки дополняют детерминированные bounds tests; успех race detector не является доказательством отсутствия логических гонок.

## Definition of Done

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
- [x] Полные root tests/race, обязательные fork suites/race, vet, backend build, Vitest и frontend build проходят; regression гарантий фаз 34–39 отсутствует.
- [x] Upgrade/restart/rollback ограничения, API/config/ownership/lifecycle документация актуальны; `tasks/implementation/phase40-report.md` содержит каждый F40/A40 и общий DoD.

Перенос обязательного пункта за пределы фазы является изменением плана и должен быть явно согласован и отражён в отчёте. Непроверенная гарантия не отмечается выполненной из-за отсутствия ошибки в штатных тестах.
