# Фаза 42: Человекочитаемое хранение, состояния файлов и восстановление

## Статус и зависимости

- Статус: **фаза завершена**. Реализация разрешена пользователем 25 октября 2026 года. Полный macOS `make check` и native Linux suites прошли; успешный native Windows CI подтверждён пользователем. Результаты всех блоков — в [отчёте](implementation/phase42-report.md).
- Предыдущая [фаза 41](phase41-storage-recovery-and-migration-observability.md) завершена по её [отчёту](implementation/phase41-report.md). Её гарантии хранения, recovery и совместимости являются исходными требованиями этой фазы.
- Основание: [сравнение архитектуры с qBittorrent](implementation/qbittorrent-storage-architecture-review.md). В отчёте зафиксированы ревизии и ссылки на исходники; план не зависит от истории чата или файлов в `/tmp`.
- Начинать реализацию по отдельной команде пользователя. Блоки выполнять последовательно; следующий блок начинать после проверки критериев приёмки предыдущего.
- После значимого блока и завершения фазы обновлять `tasks/implementation/phase42-report.md`: выполненные задачи, DoD, миграции, проверки, измерения и отклонения. Создание этого плана не означает выполнения DoD реализации. Следующую фазу автоматически не начинать.

## Цель и ожидаемое поведение

Пользователь видит понятные каталоги загрузок, может безопасно перемещать торрент или указывать существующие данные, различает скачивание и проверку, понимает состояние каждого файла после рестарта. Идентичность торрента, его отображаемое имя и расположение файлов независимы.

Пример нового размещения:

```text
downloads/
  payload/
    Interstellar (2014)/
      Interstellar.2014.1080p.mkv
    Example Series - Season 01/
      S01E01.mkv
      S01E02.mkv
    Example Series - Season 01 (2)/
      S01E01.mkv
```

Hash остаётся ключом torrent identity и служебного metainfo. SQLite хранит фактический путь и правила layout. Изменение `title` в GUI не запускает filesystem rename. Если выбран один из двух равных файлов и он загружен, GUI показывает «Выбрано: 100%; весь торрент: 50%». После рестарта сохранённый прогресс отображается отдельно от текущей проверки и не разрешает чтение сам по себе.

## Границы фазы

Входят Go, SQLite migrations, локальный fork `anacrolix/torrent`, REST/SSE, Vue, compatibility adapters TorrServer, документация и тесты. Операции выполняются внутри управляемого `HUB_DOWNLOAD_DIR`; источники и цели проходят rooted validation, проверку владения и проверку доступности файловой системы. Между файловыми системами внутри этого пространства поддерживается безопасный copy fallback.

В эту фазу не входят:

- fast resume с доверием сохранённому completion вместо нового hash-прохода; итог фазы содержит измерения и предложение для отдельного решения;
- смена torrent engine, новая БД или несвязанные обновления зависимостей;
- автоматический перенос всей существующей библиотеки при обновлении;
- совместная запись разных торрентов в одни файлы, deduplication, hardlink/symlink-витрина;
- индивидуальное переименование файлов, автоматическое распределение по категориям, отдельные incomplete/completed directories и управление произвольными внешними storage roots;
- возврат автоматического `.part`, выбор конфликтующей копии по имени/размеру или автоматическое объединение неполных копий;
- применение миграции к рабочему NAS пользователя в рамках разработки и тестов.

## Исходное состояние

| Наблюдение | Точки изменения |
| --- | --- |
| Mapping уже хранится в `torrent_storage`, но разрешён только `payload/<40 hex>` | `internal/payload/files.go`, `internal/engine/storage_roots.go`, repository storage methods |
| Часть путей вычисляется из hash в обход сохранённого mapping; версия layout сравнивается с единственной константой | `verify_storage.go`, `storage_migration.go`, `command_coordination.go`, `temporary_files.go`, `ownership.go`, `legacy_recovery.go` |
| v1/v2 aliases могут разделять root; простой `UNIQUE(root)` на нынешней таблице нарушит это поведение | `internal/repository/storage.go`, Add/alias reservation, hybrid fixtures |
| В DTO файла нет самостоятельной диагностики присутствия, проверки и готовности чтения | `internal/models/torrent.go`, `internal/engine/health.go`, read models, `web/src/components/BottomPanel.vue` |
| Общий progress относится ко всему торренту; готовая выборка может выглядеть как `Queued` и 50% | `runtime_policy.go`, `phase38_runtime_test.go`, `web/src/progress.ts` |
| MissingFiles опирается на подтверждённое ожидание текущего runtime; нет полного inventory ранее существовавших данных после рестарта | `missing_payload.go`, startup/restore usecases |
| Каждое новое открытие требует verification; уже действуют два активных hashers глобально и один на торрент | `client_config.go`, fork verification/storage, `internal/completion/sqlite.go` |
| Есть durable migration/deletion/recovery journals, record/version fencing и наблюдаемый прогресс | [Контракт фазы 41](phase41-recovery-contract.md), `internal/usecase/STATE.md`, `internal/engine/CONCURRENCY.md` |

## Реестр задач

| ID | Приоритет | Результат |
| --- | --- | --- |
| F42-01 | P1 | Version-aware storage mapping с понятными именами, стабильным владельцем и единым resolver |
| F42-02 | P1 | Новые `.torrent` и magnet используют новый layout без двойного корня и обхода reservation |
| F42-03 | P1 | Планирование, перенос/переименование каталога и конверсия старого layout через durable journal |
| F42-04 | P1 | Read-only анализ существующих данных и явное подключение подходящего расположения |
| F42-05 | P1 | Inventory после рестарта, ожидаемое наличие файлов и обнаружение недоступного/неожиданного тома |
| F42-06 | P1 | Согласованные состояния файлов, selected progress и verification progress в API/GUI |
| F42-07 | P2 | Измеряемая и ограниченная очередь проверки с приоритетом нужных стримингу диапазонов |
| F42-08 | P1 | Совместимость старых данных/journals, upgrade rehearsal, нативные filesystem tests и документация |

## Сохраняемые гарантии

1. SQLite — единственная БД, WAL и настроенный пул сохраняются. Файловые операции, hash-проходы и ожидания выполняются вне SQL-транзакций и общих engine/library/coordinator locks.
2. Настройки пользователя, runtime activity, safety blocks, storage workflow и наблюдаемые bytes остаются разными источниками состояния. Успех move/locate/verification не означает Resume и не снимает чужие запреты.
3. Только проверенные pieces текущего открытия разрешают соответствующее чтение и upload. Mapping, обычное имя, размер, исторический progress, fingerprint копии и offline verification сами по себе не дают runtime readiness.
4. Разные торренты не получают пересекающееся writable storage. Aliases одной идентичности используют одного владельца. Root containment, запрет опасных links, no-replace publication и защита служебных/recovery файлов сохраняются.
5. Любая команда и поздний результат проверяют record identity, storage identity, mapping revision и версию операции. Устаревший worker не меняет re-add и не удаляет данные по прежнему пути после перемещения.
6. Индексы, offsets, размеры и hashes из metainfo неизменны. Layout меняет физическое размещение, а не логический каталог или смысл ссылок TorrServer на file index.
7. Сохраняются CORS `*`, HTTP Range, общие transfer limits, QoS reference counting/debounce, paused intent, saved priorities, FIFO, temporary/permanent policy и sticky DiskFull.
8. Отмена HTTP-ожидания, остановка процесса и явная отмена операции различаются. Непрерываемый syscall не считается завершённым по deadline; ресурс остаётся у владеющей задачи до её завершения.
9. Чтение API и отрисовка GUI не запускают полный filesystem scan/hash и не дают разрешения на изменения файлов.

## Блок 1 — Контракты и воспроизводимый baseline

- Зафиксировать текущие root/fork/frontend checks и схему SQLite. Подготовить небольшие fixtures для старого и нового layout, aliases, выборочного скачивания, missing mount и restart на границах операций.
- Определить схему владельца storage location, aliases, layout version, mapping revision и временного reservation до вставки torrent record. Фактические названия таблиц/полей закрепить до перехода к блоку 2. Отделить layout version от revision mapping, operation version, record ID и snapshot sequence.
- Зафиксировать `tasks/phase42-storage-contract.md`: команды планирования/перемещения, анализа/подключения найденных данных, повторов и отмены; аргументы identity/version; разрешённые стадии; типизированные ошибки; additive поля read model. Это обязательный результат блока 1, а не ссылка на уже существующий документ.
- Использовать механизм recovery и его fencing из фазы 41. Обозначить несовместимость move/locate с pending deletion, legacy recovery и незавершённой migration. Не создавать параллельную систему разрешений, расходящуюся с coordinator/ownership.
- Тяжёлые принятые задания возвращают 202 и identity/version операции; stale/conflicting команды — 409, насыщение — явную retryable ошибку. Непредвиденные I/O/SQLite ошибки не маскируются под конфликт. Повтор запроса с той же identity/token не запускает второе перемещение.
- Численно определить лимиты очередей/активных задач и цену публикации. Контракты покрыть общими Go/TS fixtures; GUI не разбирает текст Go-ошибок.

**Приёмка:** схема и wire-контракт согласованы с существующими guarantees; baseline воспроизводим; заданы границы reservation, публикации, отмены и владения найденными данными.

## Блок 2 — F42-01/F42-02: Mapping и новые загрузки

### Единый источник путей

- Сохранить поддержку legacy layout и существующего hash layout; добавить именованный layout. SQL upgrade не переименовывает пользовательские файлы и не превращает старый `ready` в задачу обязательного переноса.
- Все runtime/open/verify/stream/download/archive/migration/recovery/cleanup пути получают физическое расположение через один version-aware resolver. Удалить вычисление актуального пути из hash вне явного adapter старого layout.
- В mapping хранить относительный корень и явное правило обработки metadata prefix. Для single-file не удалять имя файла; для multi-file с общим корнем не создавать `Series/Series/file`. Не переоценивать общий префикс при каждом открытии. Неподдерживаемая версия даёт понятный отказ, а не fallback в другой каталог.
- Aliases ссылаются на одного владельца. Проверка уникальности учитывает равенство и вложенность путей; сохраняются reservations между preflight и фактической записью. Незавершённый Add после сбоя не оставляет путь, который другой Add может молча присвоить. Освобождение orphan reservation не удаляет неизвестный payload.

### Выбор имени

- При Add использовать явно заданное пользователем имя каталога, иначе очищенное имя из подтверждённого metainfo. Для коллизии добавлять `(2)`, `(3)` и сохранять выбранный путь; не пересчитывать суффикс после рестарта или удаления соседнего торрента.
- Определить переносимую политику Unicode, регистра, разделителей, Windows reserved names, trailing dots/spaces и длины компонента. Зарезервировать служебные имена. Проверять и SQLite-владение, и фактически занятое место на целевой ФС; переносимость не означает доверие чужому существующему каталогу.
- До metadata у magnet существует логическое reservation, но не произвольно выбранный writable payload root. Получение metadata разрешено по существующей discovery policy; финальное имя/layout резервируются до первого payload write и открытия storage. `dn` — только очищенная подсказка. Private-safe режим не ослабляется.
- Изменение `name/title/category` само по себе не перемещает файлы. В Add GUI можно задать имя каталога; у существующей записи физический путь меняется через отдельную команду.

**Приёмка:** новые одно-/многофайловые v1/v2/hybrid торренты и magnets получают стабильные понятные пути; double-root отсутствует; aliases не создают второй каталог; старые layouts продолжают работать. Проверен каждый потребитель resolver, включая direct HTTP и cleanup.

## Блок 3 — F42-03: Перемещение и конверсия старого layout

- Добавить предварительный план source → target с layout, способом переноса, конфликтами и оценкой дополнительного места. План не перемещает файлы; execution заново проверяет identity/revision, ownership и volume evidence. Старый план после изменения файлов/записи отклоняется.
- Перенос старой записи в понятный каталог выполняется явной командой. При одновременном снятии metadata root учитывать изменение внутренних путей в manifest. Directory rename допустим только при неизменной внутренней раскладке; для остальных случаев использовать проверяемые шаги manifest.
- Записать durable intent и зарезервировать source/target до первого filesystem effect. Закрыть новое admission этого storage, дождаться активных transfers/verification/storage tasks и закрыть runtime descriptors. Долгое ожидание не держит глобальный lock и видно как отдельный этап. Pause может обновить intent; после операции учитывается его последняя версия.
- На одной ФС использовать нативный rooted no-replace rename с необходимым flush каталогов. EXDEV переключает на copy path; никакого безусловного overwriting или silent merge.
- При копировании сохранять исходник до устойчивой публикации и commit mapping. Проверять копию потоково, учитывать sparse/partial/zero-length files, наличие свободного места и отмену. Несовпадение с torrent hashes означает incomplete/corrupt pieces, а не разрешение удалить исходную копию; доказательство сохранности переноса отделено от torrent completeness.
- Журнал фиксирует план и identity, staging, начало публикации, подтверждение публикации, mapping commit и cleanup. SQLite и FS не являются одной транзакцией: restart проверяет обе стороны по evidence и продолжает допустимый шаг. При неоднозначности сохраняет данные и диагностирует конфликт.
- Mapping переключается вместе с зависимыми aliases и ожидаемым inventory одной согласованной DB-операцией. После commit cleanup проверяет identity и не удаляет заменённый source. Старые cleanup manifests не получают права на новый root. Метаданные record, FIFO, selection и temporary policy сохраняются.
- До стадии публикации разрешена явная отмена с завершением владеющего worker и обработкой только доказанно принадлежащих ему временных файлов. После записи намерения публикации отмена не обещает rollback: контракт возвращает 409, журнал доводится до согласованного состояния/recovery. Disconnect и shutdown сохраняют журнал.
- Retry продолжает ту же операцию, а не составляет новый путь по текущему title. Проектировать повтор после каждой границы FS/SQL. По завершении открыть новый runtime с текущими intent/blocks; move не переносит старое разрешение читать непроверенные данные.

**Приёмка:** rename и copy fallback переживают crash/flush/DB failures без потери единственной копии и перезаписи чужих данных; активный Range stream корректно завершает lease; unrelated torrent остаётся доступным; новая запись с тем же hash защищена от старого worker.

## Блок 4 — F42-04/F42-05: Существующие данные и startup inventory

### Ожидаемые файлы и том

- Хранить подтверждённое ожидание присутствия файла отдельно от completion: file index, catalog/mapping identity и источник/время наблюдения. Успешное создание/проверка обновляет ожидание в ограниченных batch writes; telemetry не превращается в транзакцию на каждый chunk.
- Отсутствие ещё не создававшегося либо skipped файла не означает MissingFiles. Для старой записи без надёжного evidence состояние unknown, а не выдуманное «был готов». Upgrade явно описывает границу диагностики до первого подтверждённого inventory.
- Перед открытием writable storage проверять ожидаемую файловую систему. Использовать устойчивое evidence регистрации управляемого тома, например сохранённый идентификатор и служебный marker с платформенной проверкой там, где она доступна. `st_dev`/inode не считать переносимым идентификатором между reboot/container environments. Задокументировать границы обнаружения подмены: копия marker сама по себе не доказывает физическую идентичность носителя.
- На уже зарегистрированном storage отсутствие marker/ожидаемого тома запрещает автоматические mkdir/download/cleanup на замещающей ФС. Проверка не создаёт новый marker, выдавая его за восстановление старого. Первичная регистрация свежей установки и подтверждение storage старой установки — разные пути; если evidence недостаточно, нужен явный reconcile через контракт восстановления.
- Инвентаризация ограниченно проверяет сохранённые пути, не сканирует произвольно весь диск. Отделять missing, access denied, volume unavailable, unknown и обычную недокачанность. Inventory не хеширует payload и не предоставляет readiness.
- Тяжёлую инвентаризацию выполнять как учтённые background jobs; bootstrap после согласованного чтения БД публикует честное pending-состояние. Pending record не получает writable admission; восстановленные записи и HTTP-диагностика не ждут полного обхода другого торрента. Сохраняются snapshot/version гарантии и отсутствие ложного пустого списка на startup.

### Найти, подключить, восстановить

- «Указать существующие данные» принимает относительный путь к выделенному каталогу внутри управляемого download root. Нужны metainfo и отсутствие конфликтующего владельца. Произвольный shared root/чужой каталог не принимается как writable location; для старого общего layout применяется существующий migration workflow.
- Анализ кандидата строго read-only: не создаёт файлы, не импортирует `.part`, не меняет размеры/имена и persistent completion. Использует metainfo verifier с v1 shared boundaries, v2/hybrid и частичными данными. Возвращает найденные/отсутствующие файлы, проверенные pieces и ограничения; одинаковые имена/размеры не доказывают совпадение.
- Результат связан с record ID, mapping revision и candidate evidence. Применение повторно проверяет их и ownership; изменившийся кандидат требует нового анализа. Подключение частичного кандидата допускается только явной командой и не выдаётся за полную готовность. При отсутствии подтверждённого соответствия автоматического подключения нет.
- Применение атомарно меняет mapping/aliases/ожидания и оставляет запись paused. Оно не удаляет старое расположение и не включает загрузку. Права на ранее управляемые оставленные данные сохраняются как retained ownership, подобно защищённым альтернативам фазы 41; удаление записи или старый cleanup не уничтожают их молча.
- «Проверить и докачать отсутствующее» — отдельное явное разрешение на repair, после проверки доступности тома и существующих DiskFull/verification gates. Не снимать запрет только потому, что каталог снова появился. Новое runtime открытие заново подтверждает readiness.

**Приёмка:** исчезнувшие до рестарта ожидаемые файлы и недоступный mount диагностируются без неконтролируемой повторной загрузки; незагруженные/Skip-файлы не дают ложной потери данных; locate сохраняет обе стороны и ничего не записывает во время анализа; paused/blocked intent сохраняется.

## Блок 5 — F42-06: Состояния файлов, API и GUI

Read model расширяется additive полями. Точные wire-имена и версии закрепляются в блоке 1; семантика обязательна:

| Независимое поле/группа | Смысл |
| --- | --- |
| Base priority / selection | Устойчивая настройка пользователя, отдельно от временного streaming overlay |
| Verified bytes | Подтверждённые данные текущего открытия |
| Last-known bytes + timestamp/source | Историческое наблюдение, которое не подменяет verified bytes и не теряется при начале нового прохода |
| Presence + freshness/reason | Present/missing/unknown и отдельная недоступность storage; отсутствие полного scan не равно present |
| Verification operation + progress/result | Queued/running/completed/failed/cancelled; completed означает завершённый проход, а не обязательную корректность всех pieces |
| Direct-read readiness + reason | Наблюдаемая возможность прочитать полный файл; фактическое открытие повторяет gate |
| Activity / effective priority | Загрузка/чтение и влияние stream range без изменения base priority |

- Сохранить смысл существующих `downloaded`, `progress`, `state`, `progress_source`, `selection_complete` и TorrServer полей. Не менять знаменатель старого progress молча; добавить selected size/verified bytes/progress с согласованной версией selection/catalog.
- Если файлы не выбраны, отображать «Нет выбранных файлов», не 100% и не Seeding. Для zero-length отдельно определить существование/готовность; не использовать деление на ноль и не считать metadata-only объект доказанно доступным.
- GUI показывает отдельные подписи «Получение метаданных», «Ожидание слота», «Нет передачи», «Проверка», «Перемещение», «Выбранные файлы готовы» на основании уже разделённых осей. Нулевая мгновенная скорость не автоматически означает «Нет пиров». Не вводить один persistable enum, который вытесняет одновременно действующие блокировки/активность.
- У файла показывать путь в metadata и фактический относительный путь, состояние presence/verification, источник прогресса и причину недоступности. `Skip` может сочетаться с готовым файлом, `is_media` не является readiness.
- Приоритеты менять как прежде; кнопки transfer/recovery строить по наблюдаемым доступным действиям, а backend повторно проверяет условия. Не блокировать streaming неполного файла по одному `direct_read_ready=false`: torrent-reader path сохраняется при разрешённых gates.
- Добавить интерфейсы плана переноса, применения/retry/cancel в допустимых стадиях, анализа/подключения найденного каталога, диагностики inventory/volume. План показывает source/target, метод, свободное место и конфликты до выполнения.
- REST details/files, compact/full SSE и GUI используют одну проекцию. Большой file catalog загружается для выбранного торрента, не дублируется в каждом compact event. Старые epochs/revisions/attachments и ответы после изменения selection не перезаписывают новые данные.
- Progress показывает текущий этап, bytes/files, очередь и время последнего продвижения. Heartbeat не имитирует продвижение. При неизвестном total процент/ETA не выдумываются; повторный проход явно отличается от продолжения прежнего.

**Приёмка:** сценарий выбранных 50% отображается как завершённая выборка без ложного полного Seeding; проверка после рестарта не выглядит потерей сохранённого прогресса; file reasons согласованы во всех интерфейсах; readiness остаётся серверной проверкой, а не выводом Vue из процента.

## Блок 6 — F42-07: Очередь проверки и измерения

- Измерить baseline на воспроизводимых fixtures: время до API readiness, до первого проверенного воспроизводимого диапазона, до завершения проверки; фактические bytes read, число stat/open, максимальную конкуренцию, memory/queue bounds и количество SQL writes.
- Связать фоновые verification jobs с управляемой ФС. Ограничить конкуренцию, особенно для одного HDD; identity устройства не выводить только из строкового совпадения пути. Уже действующие глобальные/per-torrent hasher limits учесть, а не создать вторую независимую очередь, умножающую их.
- Приоритизировать проверку диапазона, нужного активному HTTP reader, затем выбранные данные и фоновую библиотеку. Планировать на границах pieces/ограниченных batches, учитывать v1 boundary dependencies и v2 layout. Не удерживать library lock на ожидании очереди и не отменять уже выполняющийся syscall ради seek.
- Ограничить pending requests и объединять повторные задания по storage/attachment/range. Для startup использовать durable pending records и ограниченные порции enqueue вместо worker на каждый торрент. Насыщение не теряет обязательную проверку; интерактивный запрос получает явное ожидание/ошибку по контракту.
- Обеспечить продвижение фоновой очереди при повторных seeks и нескольких клиентах; численные limits и правило fairness закрепить в конфигурации/контракте. Delete/move/locate/shutdown отменяют или отзывают старые jobs через существующее generation fencing. Pause обновляет admission без включения сети; явно запрошенная локальная проверка paused-файла остаётся допустимой и не превращается в Resume.
- Наблюдаемость не добавляет hash-проходов и запись SQLite на каждый chunk. Сохранить ограничения памяти и обычный file I/O без введения mmap для payload.
- В отчёте сопоставить baseline/результат и отделить synthetic/local FS от HDD/NAS. Не задавать нестабильный CI-порог в MB/s и не объявлять быстрым запуском только раннее открытие HTTP-порта.
- Отдельно описать возможность будущего fast resume: необходимые checkpoints, flush ordering, invalidation до новых writes, crash/dirty state, limits обнаружения внешних изменений. Реализация доверенного persistent completion не входит в DoD этой фазы.

**Приёмка:** deterministic тест подтверждает, что запрос стриминга опережает фоновый backlog без чтения непроверенных pieces; фон не голодает, бюджеты соблюдаются, unrelated torrent/HTTP не ждут заблокированный I/O под общим lock. Измерения и границы их применимости представлены в отчёте.

## Блок 7 — F42-08: Совместимость, документация и итоговая приёмка

- Обновить SQLite на копиях данных с legacy/ready/blocked/moving, pending deletion/cancel и legacy alternatives фазы 41. Старые manifest paths интерпретировать по их версии; не переписывать незавершённый journal как новый layout.
- Проверить смешанную библиотеку: старые hash roots остаются рабочими, новые записи получают понятные имена, отдельный старый торрент переносится без влияния на остальные.
- Обновить README, wire-контракт фазы 42, `STATE.md`, `CONCURRENCY.md` и описание recovery фазы 41 в части совместимости. Описать naming, владельцев/aliases, retained data, volume enrollment/reconciliation, команды и политику проверки.
- Описать backup/upgrade/recovery rehearsal: согласованная копия SQLite/metainfo/payload, прерывание операций и восстановление из journals. Старый бинарник не должен работать с неизвестным layout; downgrade — через согласованный backup, без обещания обратной совместимости файловых операций.
- Создать итоговый отчёт с результатами каждого F42, отмеченным DoD, точными командами и platform evidence. Если недоступна обязательная native проверка, зафиксировать незакрытый пункт; cross-compile не считать её выполнением.

## Обязательные сценарии проверки

| Область | Сценарии и проверяемые свойства |
| --- | --- |
| Naming/layout | Single-/multi-file, rootless, Unicode, case collisions, длинные и Windows reserved names, исходные имена `.part`, одинаковые названия разных торрентов, занятый чужой каталог, смена title без move |
| Metadata/aliases | v1/v2/hybrid, magnet без `dn` и с опасным `dn`, поздняя metadata, одновременный Add alias, crash до/после reservation; только один writable owner и ни одной payload write до готового mapping |
| Mapping consumers | Torrent read/write/upload, direct HTTP, archive, explicit/offline verify, legacy `.part` recovery, cleanup и deletion используют тот же mapping/version |
| Move | Same-FS rename, изменение внутреннего prefix, EXDEV copy, target exists, нехватка места, permission/flush/SQLite failures; исходник/копия сохраняются по контракту |
| Crash/retry | Restart до/после staging, публикации, mapping commit и каждого cleanup шага; повтор запроса, cancel до публикации и отказ позднего cancel; повреждение/замена source/target между этапами |
| Concurrency | Move + Range/HEAD/archive, Pause во время копирования, Delete/re-add, old observation/verification callback, aliases и competing Add, shutdown; нет stale writes/unlink и обхода transfer limits |
| Locate | Полный, частичный, неверный и изменившийся кандидат; v1 shared boundary, v2/hybrid, zero-length; analysis не пишет; Apply не удаляет прежние данные и не запускает сеть |
| Restart/inventory | Ранее существовавший файл удалён до старта, ещё не созданный Skip-файл, legacy evidence unknown, отсутствующий/другой mount при существующем пути, отказ stat, восстановление тома; нет записи на подменяющую ФС |
| State/UI | Выбран один из двух файлов, выбор пуст, Skip уже готов, paused/local read, DiskFull и streaming, checking с last-known 100%, runtime absent, delayed snapshot; независимые состояния не теряются |
| Verification | Corruption/truncation/replacement, отмена/ошибка recheck, shared pieces, queue saturation, repeated seek/fairness, blocked I/O и доступность другого торрента; только current-open verified bytes читаются |
| Upgrade | Старые mappings/journals/aliases/cleanup manifests, protected alternatives, SQLite reopen, смешанные layouts и документированный downgrade из backup |

Для бизнес-логики — unit tests; repository — реальные временные SQLite базы с WAL/reopen/ошибками commit; storage — настоящие файлы/root adapters и fault injection. Гонки проверять barriers/fake clock, а не случайными задержками. Публичный swarm и рабочий NAS для функциональной приёмки не требуются.

После каждого блока — проверки затронутых гарантий. Итог: `make check`, включая root `go test ./...`, race/vet/build, frontend tests/build и отдельный fork `.`/`./storage`; при изменении других fork packages расширить набор. Для нового filesystem protocol обязательны native Linux/macOS/Windows checks. Реальные HDD/NAS measurements описываются отдельно от переносимых deterministic acceptance tests.

## Definition of Done

- [x] F42-01: SQLite хранит versioned layout и фактический относительный путь; все потребители используют единый resolver, aliases сохраняют одного владельца.
- [x] F42-02: новые `.torrent` и magnets получают понятные стабильные каталоги, корректно решают коллизии и не создают двойной root; metadata/discovery gates сохранены.
- [x] F42-03: preview/move/rename/conversion доступны через API/GUI; журнал выдерживает FS/SQL crash boundaries, rename/copy fallback и допустимые retry/cancel без потери единственной копии.
- [x] F42-04: read-only locate анализирует metainfo и partial data; применение повторно проверяет evidence, сохраняет прежние данные/ownership и оставляет paused intent.
- [x] F42-05: expected presence и volume evidence переживают restart; missing/unknown/unavailable отделены от incomplete; потеря mount не вызывает запись/удаление на замещающем storage.
- [x] F42-06: API/GUI различают selected/whole progress, last-known/current verified bytes, presence, verification и direct-read readiness; старые wire-поля/TorrServer сохраняют смысл.
- [x] F42-07: очередь проверки ограничена, нужные reader диапазоны получают приоритет без голодания фона; измерены I/O/latency/memory/SQL, verified-only policy сохранена.
- [x] F42-08: upgrade старых layouts и незавершённых journals проходит без обязательного переноса всей библиотеки; retained recovery data защищены; rollback procedure описана.
- [x] Paused, DiskFull, QoS/debounce, transfer limits, FIFO, temporary policy, fencing и snapshot ordering проходят regression tests.
- [x] `make check` и обязательные native filesystem suites Linux/macOS/Windows проходят; источники platform evidence и ограничения измерений зафиксированы (Windows CI подтверждён пользователем).
- [x] README, wire/state/concurrency contracts и `tasks/implementation/phase42-report.md` обновлены; все отклонения от плана объяснены. Fast resume не включён скрыто ради ускорения старта.
