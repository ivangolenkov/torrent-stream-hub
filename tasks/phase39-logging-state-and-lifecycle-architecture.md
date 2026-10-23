# Фаза 39: Устойчивое Логирование, Владение Состоянием И Lifecycle Coordinator

## Статус И Зависимости

- Статус: **завершена, общий DoD проверен**. Итоговый отчёт: `tasks/implementation/phase39-report.md`. Следующая фаза не начиналась.
- Основание: `tasks/draft/codebase-audit-2026-10-21.md`, **A06 + A01 + A02 + A04**. Объём расширен пользователем до всех четырёх находок.
- Предусловие реализации: завершённый и проверенный DoD фаз 34–38. Перед изменениями проверить актуальную базу и повторить обязательные regression suites.
- Это одна фаза с последовательными внутренними блоками и общей приёмкой. Зелёный отдельный блок не означает закрытия всей фазы.
- Отчёт после реализации: `tasks/implementation/phase39-report.md`. Следующие фазы автоматически не начинать.

## Цель И Границы

Исключить зависимость рабочих операций от скорости вывода логов; определить единственного владельца каждого вида состояния; заменить распределённые присваивания state управляемыми переходами; убрать общеприкладную сериализацию через repository mutex, не потеряв гарантии сохранности данных и конкурентности.

В scope входят необходимые изменения Go/SQLite/Vue, миграция существующего состояния, API-совместимость, диагностика, документация и детерминированные тесты. Полноценный механизм переходов A02 обязателен, но actor-модель, отдельная goroutine на каждый торрент и переписывание проекта целиком не требуются.

Не входят F09/F11/F12, auth, dependency upgrades, полный A05, генерация всей API-схемы, новое UI логов, live-follow логов, внешняя система метрик, event sourcing и отдельный message broker. Настройка CI полезна, но не подменяет приёмку и не является самостоятельной задачей этой фазы. A03 не переоткрывать общим storage rewrite: использовать уже введённые rooted mapping/manifest/verified gates.

## Проверенная Исходная База

- `internal/logging/files.go`: console/file writes, rotation и retention выполняются синхронно под одним mutex. `Logger.logf` дополнительно держит logger lock при выводе. Размер активного файла и общий объём не ограничены; default log level — debug.
- `internal/logging/reader.go`: безопасные snapshots с открытыми дескрипторами и pinning против retention; API умеет дату/диапазон/tail. Эти свойства необходимо сохранить при размерной ротации.
- `models.Torrent` используется как persisted record, runtime snapshot и API view. `SyncWorker` каждые две секунды сохраняет runtime через общий `SaveTorrent`; state/error всё ещё обновляются этим путём.
- Priorities, storage mapping, deletion intent и disk latch уже частично отделены. Фаза 38 добавила provenance прогресса и защиту старых observations. Не внедрять эти исправления повторно вместо завершения A01.
- `setStateLocked` только присваивает state/error и логирует; переходы вызываются из scheduler, restore, selection, recheck, metadata и disk gate. Есть также прямые присваивания. `Streaming`/`MissingFiles` объявлены, но полноценный контракт их использования отсутствует.
- `TorrentRepo.Lifecycle` сериализует команды, leases, cleanup, snapshots и SSE. Проверка Paused payload может удерживать его на всём hash-проходе. В Engine есть отдельный глобальный `admissionMu`, под которым также встречаются disk/persistence/library операции.
- Старые locks обеспечивают реальные инварианты. Простое удаление `Lifecycle`, его переименование или перенос того же длинного global lock в другой пакет не закрывают A04.

## Неизменяемые Гарантии

1. SQLite остаётся единственной БД: WAL, per-connection PRAGMA и настроенный пул сохраняются. SQLite и filesystem/library не становятся одной транзакцией.
2. Сохраняются isolated payload roots/aliases, rooted open/write/unlink, migration journal, no-replace publication и защита чужих/служебных файлов.
3. Только verification текущего открытия разрешает чтение/upload/cache readiness. Persisted progress и новое desired state не являются доказательством готовности.
4. Sticky DiskFull снимается только явным Resume после проверок и persistence. Priority/stream/metadata/recheck/duplicate add не могут его снять.
5. Сохраняются persisted FIFO, selected completion без ложного Seeding, scheduler limits, stream reference counting/debounce, preload limits и SSE deadlines/latest-only.
6. Оба deletion modes и migration остаются durable workflows. Старый callback, snapshot или release не действует на повторно добавленный тот же hash.
7. Сохраняются endpoint names, TorrServer-совместимость, CORS `*` и наблюдаемые ошибки. Нельзя вернуть успех после отказа commit, admission или постановки обязательной задачи.

## Блок 1 — A06: Ограниченное Асинхронное Логирование

### Producer И Очереди

- Все штатные источники — собственный logger, standard log, slog/library bridge, HTTP access/panic logs — используют один контракт неблокирующего admission. Producer не пишет в filesystem/stdout/stderr, не ждёт освобождения очереди и не захватывает mutex, удерживаемый sink во время I/O.
- Ограничить не только число записей, но и суммарные retained bytes и размер одной записи. Копировать принятые данные: последующая модификация переданного `[]byte` не меняет сообщение. Не использовать goroutine-per-write, бесконечные retries или неограниченный spill в память/на диск.
- Маскировать секреты до помещения в очередь и до любого вывода. Ограничение длины применять после маскирования, на корректной границе UTF-8, с явным маркером truncation. Сохранить многострочные записи/stack traces в пределах лимита. Не обещать ограничения произвольного пользовательского `Stringer` или transient allocations форматирования только за счёт bounded queue.
- Политика насыщения: **drop-newest**, без синхронного fallback даже для Warn/Error; считаются dropped records/bytes и доступный уровень. Отдельно считать oversized/truncated сообщения, sink errors и потери при shutdown. Отсутствие потерь при перегрузке не гарантируется и явно документируется.
- Разделить sink I/O и короткую синхронизацию очередей/статистики. Блокирующий console sink не должен останавливать доставку в исправный file sink и наоборот; количество workers и общий бюджет их очередей конечны. Зарезервировать отдельные доли budget для sinks: один зависший sink не может занять весь общий бюджет и лишить исправный sink admission.

### Лимиты И Настройки

Базовый контракт для NAS; значения могут быть уменьшены через internal options в тестах:

| Параметр | Default / правило |
| --- | --- |
| Общий бюджет owned очередей | Не более 1024 ожидающих записей и 4 MiB retained payload: по 512 записей / 2 MiB на file и console; учёт копий обязателен |
| Максимальная запись | 64 KiB после маскирования, включая маркер truncation |
| `HUB_LOG_MAX_FILE_BYTES` / `--log-max-file-bytes` | 16 MiB |
| `HUB_LOG_MAX_TOTAL_BYTES` / `--log-max-total-bytes` | 128 MiB управляемых log-файлов |
| `HUB_LOG_RETENTION_DAYS` | Существующая семантика; default 3 предыдущих дня плюс текущий |
| Уровень по умолчанию | `info`; явный `HUB_LOG_LEVEL=debug` продолжает работать |
| Default drain deadline | 2 секунды; context-aware flush/close, управляемый deadline в тестах |

- Byte limits положительные, total не меньше file limit, file limit в production не меньше максимальной записи; не превращать ошибку ввода/overflow в unlimited. CLI/env precedence сохранить, defaults и изменение log level описать в README/compose/Makefile, где они заданы.
- Общий бюджет очередей не включает произвольные буферы вызывающего кода. Число одновременно обрабатываемых worker-записей также ограничено и учитывается при описании максимальной retained памяти.

### Rotation, Retention И Чтение

- Совместить суточную и размерную ротацию. Использовать безопасный однозначный namespace сегментов с датой и устойчивым порядковым номером; старые `hub-YYYY-MM-DD.log` остаются читаемыми. Рестарт не перезаписывает сегменты и не сбрасывает счётчик поверх существующих файлов.
- Timestamp принятого события сохраняется; определить и протестировать смену даты при backlog. Не сортировать сегменты через `mtime` или лексикографический порядок непаддированных чисел.
- Перед append соблюдать file limit с учётом всей записи; не создавать unlimited active file из-за одной большой записи. Не обрезать старые файлы и уже открытые snapshots.
- Retention по датам сохраняется; общий size budget может удалять самые старые допустимые сегменты и внутри срока хранения. Удалять только собственные обычные файлы, не symlink/каталоги/посторонние файлы; active и pinned snapshots не уничтожать.
- При нехватке места в budget из-за pinned файлов/ошибки удаления приостанавливать file append и учитывать потери, а не бесконечно превышать quota. После освобождения ресурсов возобновлять вывод с ограниченными retries. Уже oversized legacy-файлы не увеличивать; описать их безопасную очистку и случаи, когда немедленно уменьшить занятый объём невозможно.
- `GET /api/v1/logs` объединяет legacy и новые segments в правильном порядке, сохраняет date/range/tail, rooted opening, snapshot upper bounds, streaming memory и cancellation. Читатель не держит writer/queue locks во время сетевой передачи.
- Асинхронно принятая запись не обязана сразу появиться в logs API: endpoint читает завершённые записи, не делает неограниченный implicit flush. Для тестов и штатного shutdown предоставить flush с context.

### Ошибки, Диагностика И Shutdown

- Ошибки sinks не останавливают engine и не сообщаются рекурсивно через тот же отказавший logger. Retry/summary ограничены по частоте и также не создают блокирующий stderr fallback на producer path.
- Добавить thread-safe stats без ожидания sink I/O: queue depth/bytes, dropped/truncated, file/console errors, quota-paused и shutdown losses. Минимальная read-only диагностика доступна через `GET /api/v1/logs/status`, без внутренних путей/секретов, с `no-store` и CORS `*`; это не новый UI/metrics framework.
- После остановки штатных producers закрыть admission, завершить доступный drain и закрыть owned sinks. Concurrent/repeated flush/close и late write безопасны; после close не возникает send-on-closed или нового worker.
- Deadline flush/close ограничивает ожидание вызывающего кода, но не прерывает произвольный зависший `io.Writer`/OS syscall. При timeout вернуть диагностируемый результат, не закрывать ресурс из-под выполняющейся записи и не запускать заменяющие workers бесконечно. После разблокировки управляемого sink worker завершает cleanup. Не объявлять такой timeout доказательством мгновенного прекращения любого I/O.
- Проверить startup-error paths, восстановление adapters и shutdown order в `cmd/hub`; normal graceful shutdown сохраняет доступные финальные сообщения. Логи не являются durable audit journal: enqueue/flush без fsync не равны сохранности при power loss.

## Блок 2 — A01: Единственный Владелец Каждого Вида Состояния

### Разделение Моделей И Записи

| Вид данных | Источник истины и разрешённые writers |
| --- | --- |
| Desired intent | SQLite: run intent `paused`/`enabled`, base priorities/selection, пользовательские metadata, temporary policy и persisted FIFO; меняются явными командами |
| Metainfo/catalog | Валидированный hash-matching metainfo и собственные repository операции; новые файлы получают defaults только при первом появлении |
| Runtime observations | Текущее поколение Engine: фаза, временные blockers, ошибки, verified bytes, скорости, peers; persistence хранит только last-known observation |
| Stream/preload overlays | Менеджеры сессий текущего runtime generation; не записываются как пользовательские priorities/Resume |
| Safety/workflows | Deletion/migration/disk latch и требующие явного retry activation blockers: отдельные durable записи с собственными протоколами и полномочиями; observation save их не меняет |
| REST/SSE/TorrServer/UI | Проекции перечисленных источников, не самостоятельный источник командного намерения |

- Выделить разные типы для intent, runtime observation и API read model. Общий `models.Torrent` допустим как совместимый transport DTO, но не как универсальный аргумент для изменения всех таблиц.
- Заменить production-вызовы общего `SaveTorrent` на узкие create/intent/metadata/catalog/observation операции. Observation writer не принимает desired fields, не меняет priorities/policy/source/queue/workflow и **не создаёт отсутствующий torrent** через upsert. Не оставить обходной compatibility helper с прежними полномочиями в рабочем пути.
- Metadata discovery не перезаписывает уже выбранные High/Skip/Normal. Автоматический scheduler, selection completion, stream/preload и recheck не переводят desired intent в Pause/Resume.
- Добавить durable intent revision, идентичность записи, отличающую delete/re-add, и fence текущего runtime attachment. Applied revision — runtime observation, не замена desired revision. Stale observations/completions отклоняются по идентичности/поколению/версии, а не только по wall-clock timestamp.
- Порядок наблюдений внутри attachment задаётся монотонной sequence или эквивалентным доказанным механизмом. Старое поколение не пишет поверх нового даже при совпавшем hash/revision. Существующие timestamps остаются диагностикой и совместимыми полями; не использовать browser clock как authority.

### Команды, Частичные Отказы И Restore

- До реализации описать точку commit/вступления в силу и постусловие каждого Add/Pause/Resume/Priority/MakePermanent/UpdateMetadata/Delete. Resume означает разрешение на участие в scheduler, не гарантию немедленного Downloading.
- Открытие download gate возможно только после commit соответствующего intent и повторной проверки текущих blockers/версий. Допустимо предварительное fail-closed quiesce при Pause/ошибке; отказ БД не должен приводить к автоматическому обратному Allow.
- Успешный Pause сохраняется до ответа и переживает restart. Если commit не состоялся, вернуть ошибку и явно различать временно закрытый gate и durable Pause; не обещать сохранность незафиксированного намерения.
- Если commit состоялся, но runtime apply завершился ошибкой, intent не откатывается старым snapshot. Оставить безопасный gate и наблюдаемые applied revision/reconcile pending/error; обеспечить идемпотентный bounded retry либо явный повтор команды. Не возвращать ложный успех и не терять committed command при reopen.
- Add использует staged registration: runtime без успешного создания записи не допускается к payload transfer и не публикуется как успешно добавленный. Duplicate add не заменяет Resume, не сбрасывает selection и не меняет FIFO. Delete/re-add получает новую идентичность записи.
- Restore читает desired intent и workflows; не восстанавливает прежний operational `Downloading`/`Seeding` как готовность или право обойти scheduler. Last-known progress остаётся last-known до verification текущего открытия. Paused restore/read остаётся offline, включая отсутствие cached metainfo.
- Явно классифицировать transient diagnostics и safety errors, требующие ручного восстановления. Последние сохранять типизированными activation blockers, а не одним runtime `Error` и не изменением run intent. В частности, failed verification не должна после restart превращаться в автоматический Resume/repair; успешная новая локальная проверка снимает только свой verification blocker, не disk latch/deletion. История recheck по-прежнему runtime-only: durable safety block не означает durable operation history.
- SyncWorker только сохраняет допустимые observations и собирает read model; не создаёт новые intent records, не восстанавливает удалённые записи и не исправляет пользовательские настройки из runtime. REST single/list, SSE и TorrServer используют согласованный mapper.

### Миграция Существующей БД

- Ввести следующую SQLite migration после `0008` с upgrade/reopen/rollback tests. Не выполнять destructive пересоздание базы и не удалять legacy observations/manifest/aliases/latch.
- Backfill run intent: `Paused` → `paused`; `Queued`/`Downloading`/`Seeding`/legacy `Streaming` → `enabled`; `Error`/`MissingFiles`/неизвестное значение → консервативный `paused`, без автоматического запуска сети/repair. Исходную диагностику ошибки сохранить отдельно.
- `DiskFull` не считать пользовательской Pause: импортировать с запретом активации и сохранить/восстановить sticky latch; если reason отсутствует, создать явный legacy block. Освобождение места само по себе не разрешает скачивание. Pending deletion/migration всегда сильнее backfill intent и не запускают runtime.
- Сохранить base priorities, temporary/permanent, source/metainfo, FIFO и последние известные bytes/validity. Невосстановимое историческое намерение не угадывать по скоростям, overlays или размеру файла; выбранную conservative mapping описать как ограничение upgrade.

## Блок 3 — A02: Централизованные Переходы И Ортогональная Активность

- Ввести один механизм принятия state events и вычисления следующего состояния/разрешённых эффектов. Отдельно тестируемая transition policy принимает актуальный intent, runtime facts, blockers, operation и slot reservation; выполнение эффектов проверяет ту же generation/revision непосредственно перед apply.
- Все пути state/error/blocker изменений — Add/restore, Pause/Resume, metadata, scheduler grant/release, selection/payload completion, disk/write error, verification, Delete/Close — проходят через этот механизм. Удалить распределённые прямые присваивания и helper, позволяющий обойти правила произвольным `setState`.
- До изменения side effects составить таблицу событий/переходов: предусловия, изменяемые поля, допустимые эффекты, persistence owner, отказ/повтор и stale event. Неизвестное/запрещённое пользовательское событие возвращает typed error без частичных side effects; устаревший callback отвергается без изменения нового поколения.
- Streaming — отдельный признак активности/счётчик, не взаимоисключающий режим рядом с Downloading/Seeding/Paused. Проверенный direct stream из Paused не превращает intent в enabled; accounting-only/HEAD не создают payload overlay. Preload и recheck также имеют отдельные operation/activity признаки.
- Для существующих `Queued`, `Downloading`, `Seeding`, `Paused`, `DiskFull`, `Error`, `MissingFiles` определить полную проекцию и precedence при сочетании условий. Сочетания Pause + disk latch, selection complete + incomplete payload, recheck + slot reservation, deletion/migration + active sessions не сводить к потере одного из blockers.
- `MissingFiles` обозначает подтверждённую блокирующую недоступность ожидаемого payload/root, а не обычные ещё не скачанные pieces или отсутствующий metainfo. Определить detection/recovery event на существующих storage/read/verification путях без нового бесконечного filesystem scan. Hash mismatch не выдавать за физическое отсутствие файла; NoPeers не означает автоматически InvalidTorrent.
- Для каждого blocker определить событие снятия и его полномочия: явный Resume после disk check/persistence, успешная verification, разрешённый retry отсутствующих файлов, финализация deletion/migration. Нельзя снимать все причины одним переходом в Queued; repair, которому нужна сеть, начинается только по явному разрешённому действию пользователя.
- Каждый объявленный рабочий state/error имеет producer, условия выхода и тест; неиспользуемые значения удалить из внутренней модели либо оставить только явно обозначенными legacy compatibility aliases. Legacy `Streaming` принимается при upgrade, но не нужен как runtime source of truth.
- Reconcile вычисляет effective priorities/gates из актуальных осей состояния, не восстанавливает сохранённый до операции state. Только полностью проверенный payload даёт Seeding; completed selection лишь освобождает background slot.
- Политика не вызывает сама SQLite/filesystem/network I/O. Командный слой оркестрирует commit/apply; единый payload gate и writer-error generations остаются последней защитой фактической загрузки.

## Блок 4 — A04: Coordinator И Короткие Общие Критические Секции

### Владение И Конкурентность

- Перенести orchestration из `TorrentRepo.Lifecycle` в явный application coordinator, общий для usecase, SyncWorker, cleanup/migration и transfer admission. Repository отвечает за короткие SQLite transactions и conditional writes, не владеет mutex всего приложения.
- Ввести сериализацию команд по стабильной идентичности записи/runtime, включая DB-only torrents, а не только по существующему `ManagedTorrent`. Registry handles/refcounts защищают от двух разных locks для одного объекта, удаления lock с ожидающими командами и release старого поколения в новый счётчик leases.
- До появления infohash разбор `.torrent`/чтение входного Reader выполняется без global coordination lock; после определения hash резервируется каноническая идентичность/aliases и повторно проверяется duplicate/conflict. Блокирующий input Reader не удерживает команды независимых торрентов.
- Определить короткие cross-torrent участки: scheduler slots/FIFO, регистрация поколений, ownership roots/hybrid aliases, temporary eviction reservations и shared policy. Для операций нескольких объектов задать детерминированный порядок захвата; не держать registry/global lock при ожидании per-torrent lock.
- Не заменить один global mutex вторым. `admissionMu` также проверить и сузить: глобальная reservation/map-синхронизация допустима, но disk probe, hash/copy/unlink, сериализация JSON, sink I/O и ожидание tasks/Drop не должны выполняться под общеприкладной/global admission блокировкой.
- Библиотечные locks и единственный SQLite writer не исчезают. Не обещать параллельный commit двух SQLite write transactions; не держать транзакцию при длительном library/filesystem/network I/O. Новую схему lock order/ownership описать до удаления прежней защиты.
- Library callbacks только передают generation-scoped события в owned обработку: не захватывают синхронно coordinator/command locks из-под library locks. Persistence callbacks не создают обратный захват application lock, уже удерживаемого командой.

### Двухэтапные Операции И Общие Ресурсы

- Длительный I/O выполнять по протоколу reserve/mark → unlock → work → validate generation/revision/reservation → apply/finalize. После снятия lock результаты не становятся актуальными автоматически. Конфликтующие команды одного торрента отменяют операцию либо получают явный conflict по описанному контракту.
- Локальный hash Paused, recheck, migration copy, deletion unlink/Drop и массовый cleanup не удерживают global lifecycle lock. Pending intent/reservations продолжают блокировать опасный Resume/add/transfer. Не удалять журнал или резервирование ради «разблокировки» приложения.
- Scheduler сначала атомарно резервирует слот, затем выполняет необходимую подготовку без глобального lock, затем подтверждает grant при актуальных token/intent/blockers. Failed/stale/отменённая подготовка освобождает только свой слот. Параллельные проходы не превышают лимит, не обходят FIFO и не теряют отдельный stream-driven admission.
- Storage aliases/root reservations защищаются атомарными SQLite constraints/коротким namespace coordination; одновременные v1/v2/hybrid Add и cleanup не создают двух владельцев root. Новая per-torrent сериализация не означает независимость операций над общим physical namespace.
- Temporary eviction резервирует кандидата с повторной проверкой policy/transfer protection до destructive action. Одновременный transfer/MakePermanent/add не теряет защиту; старый lease release не уменьшает счётчик нового поколения. Безопасно переоценить кандидата при конфликте вместо удержания всех torrent locks на весь cleanup.
- Coordinator/task admission имеет bounded lifecycle: закрывает приём работы при shutdown, отменяет/присоединяет owned tasks без удержания нужных им locks, освобождает reservations ровно один раз. Keyed registry и retry queues не растут из-за delete/re-add или ожидающих отменённых команд бесконечно.

### Snapshot И Публикация

- В отсутствие старого global lock читать связанные SQLite данные согласованной короткой read transaction или эквивалентным snapshot-протоколом. Нельзя собрать intent/priorities/workflow из несовместимых версий отдельных запросов.
- Снимать immutable runtime observations; сохранять/объединять их conditional writes по текущим record/attachment/revision. Missing/deleted record не вставлять обратно. При смене версии ограниченно повторить чтение либо вернуть диагностируемый conflict, не зациклиться.
- Формировать JSON и отправлять SSE вне command/global locks. Ошибка чтения/merge не публикует пустой список или guessed policy. Медленный SSE-клиент не задерживает команды и persistence.
- REST/SSE/UI сохраняют защиту от старых command responses/snapshots. Если новые revisions используются в API, передавать большие версии без потери точности JavaScript; сравнивать только версии одной идентичности/эпохи. Старый список не воскрешает удалённую запись и не затирает re-add того же hash.

## Порядок Работ И Основные Файлы

1. **Baseline и проектирование:** инвентаризация writers/state events/locks, таблица ownership и переходов, commit/apply/recovery contracts. Закрепить опасные сценарии тестами до ослабления блокировок.
2. **A06:** `internal/logging/{logging,adapters,files,reader}.go`, новые queue/rotation/stats helpers; `internal/config`, `cmd/hub`, API logs handlers/tests, README/compose/Makefile. Закрыть блокирующий logging path до усложнения state transitions.
3. **A01:** отдельные domain types/repository methods и migration `0009_*`; `internal/models`, `internal/repository`, usecase add/commands/restore/metadata. Перевести SyncWorker на observation-only writes, сначала сохраняя прежнюю сериализацию.
4. **A02:** transition policy/events и effects integration в `internal/engine`: `engine.go`, `restore.go`, `priority.go`, `resource_manager.go`, `selection.go`, `metainfo.go`, `download_gate.go`, `recheck.go`, inactive verification и stream paths. Проверить фактические library gates/priorities, не только новое DTO.
5. **A04:** application coordinator, per-record command serialization, короткие shared reservations и optimistic fencing; usecase `torrent_uc.go`, `temporary.go`, `temporary_files.go`, `storage_migration.go`, `inactive_restore.go`, `sync_worker.go`; соответствующие engine registration/scheduler/task paths. Удалить `TorrentRepo.Lifecycle` после перевода всех потребителей.
6. **Read model и совместимость:** общие REST/SSE/TorrServer projections, необходимые Vue types/store/components. Старые timestamp/error contracts сохранить либо расширить без скрытой смены смысла; activity/pending/error не выглядят подтверждённой загрузкой/готовностью.
7. **Приёмка:** полный regression/race, upgrade/reopen и failure injection; актуализировать README и `internal/engine/CONCURRENCY.md`, добавить application state/coordinator contract; создать `tasks/implementation/phase39-report.md` с каждым пунктом DoD и пределами гарантий.

## Обязательные Приёмочные Сценарии

| Блок | Проверки |
| --- | --- |
| A06 producer | Заблокировать stdout и file writer независимо: команды и второй исправный sink продолжают работу; flood не превышает queue/record budgets и не создаёт worker на каждую запись. Проверить drop/truncation counters, копирование `[]byte`, sanitization всех adapters и отсутствие recursive fallback. |
| A06 files | Несколько rotations за день, backlog через полночь, append/reopen, numeric segment order, legacy + segments в full/tail; file/total limits, pinned reader, unlink failure/ENOSPC, quota pause/recovery; чужие файлы/symlink не затронуты. |
| A06 lifecycle | Concurrent Write/Flush/Close, late write, cancellation и deadline; обычный drain сохраняет принятые доступные сообщения. Timeout не закрывает sink под выполняющимся Write; после release управляемого writer workers завершаются. Status endpoint работает без sink lock и не раскрывает секреты/пути. |
| A01 write isolation | Подать snapshot со старыми Pause/priorities/policy/source/FIFO/workflow: observation writer не может их изменить. После Delete late save не создаёт запись; после re-add/нового attachment не меняет новые observations. Нулевые rows не выдаются за принятую команду. |
| A01 commit/apply | Отказ intent commit до Allow; commit успешен, apply отказал; повтор, bounded reconcile и реальный reopen. Успешные Pause/Resume/Priority/MakePermanent/metadata сохраняют намерение; background error/recheck/selection не превращаются в пользовательскую команду. Failed verification переживает reopen как safety block без фиктивной operation history; успешная verification не снимает чужой block. |
| A01 migration | Реальная pre-0009 база со всеми legacy states, unknown state, High/Skip, DiskFull без reason, pending keep/payload deletion, migration journal, temporary policy и FIFO; upgrade/reopen сохраняют данные и conservative activation. Ошибка миграции не оставляет полусхему. |
| A02 transitions | Table-driven tests всех событий/предусловий/недопустимых переходов; детерминированные последовательности Pause/Resume/metadata/recheck/disk error/selection/close. Проверять state axes, причины, фактические Allow/upload/readiness, priorities и slot accounting на каждом шаге. |
| A02 activity | Streaming одновременно с Downloading/Seeding и verified disk streaming из Paused; HEAD/accounting-only; Skip All/partial selected completion; реальная недоступность ожидаемого payload против обычного incomplete torrent/metadata pending/hash mismatch. Нет фиктивного Streaming-state и бессрочно недостижимого MissingFiles. |
| A04 independence | Barrier удерживает input Reader/hash/probe/copy/unlink или controlled Drop для A без открытой DB write transaction; команда и transfer admission для независимого B завершаются до освобождения A. Отдельно блокировать JSON/SSE/log sink: глобального command lock нет. Не подменять это обещанием обхода SQLite/library собственных locks. |
| A04 same object | Одновременные opens/Resume/Delete/recheck одного торрента; Delete/re-add перед late callback/lease release; cancellation ожидающего lock и registry cleanup. Нельзя получить два command owners, negative leases или применить старый эффект к новому runtime. |
| A04 shared resources | Параллельные scheduler passes/отказы/cancel при limits 0/1/N; проверять максимум reservations в процессе, FIFO и stream-only admission. Hybrid aliases/root collisions, temporary eviction против transfer/MakePermanent и migration/delete продолжают соблюдать ownership. |
| Snapshot/API | Старый snapshot после новой команды, metadata-получение при SyncWorker, observation после delete/re-add, DB read failure, частичный read/merge при смене revision; single/list/SSE/TorrServer согласованы. Vue не откатывает intent/activity из старого ответа. |
| Regression | Rooted storage и migration recovery, verified read/upload, sticky DiskFull, priorities restore, offline Paused read, recheck singleflight, refcount/debounce, preload cleanup, оба deletion modes, SSE latest/deadline и CORS `*`. |

- Использовать barriers/channels, управляемые часы, fake sinks/verifiers/probes и реальные временные SQLite с triggers/reopen. `Sleep`, успешный race или повтор теста не заменяют доказательства нужного interleaving.
- Доказывать независимость B до release A, а не только малым временем в обычном прогоне. Для queue/registry/worker bounds проверять счётчики и удерживаемые ресурсы; NAS latency/RSS benchmarks проводить отдельно при наличии окружения.
- Обязательные команды из корня: `go test ./...`, `go test -race ./internal/... ./cmd/...`, `go vet ./...`, `go build ./cmd/hub`, `git diff --check`; в `web/` — `npm test`, `npm run build`.
- Из `pkg/anacrolix-torrent`: `go test . ./storage` и `go test -race . ./storage`, без исключений обязательных suites. Форк менять только при доказанной необходимости; optional libpossum/FUSE ограничения указывать отдельно.
- Повторить targeted concurrency/failure suites под race, зафиксировать точные команды/результаты в отчёте. Реальные NAS/Smart TV, Windows/Linux runtime, физические ENOSPC/readonly/power-cut — только при доступном окружении; непроверенное явно отметить.

## Definition Of Done

- [x] A06: штатные logging producers не ждут sink I/O; record/queue/worker memory bounds и drop policy реализованы и проверены, маскирование сохранено.
- [x] A06: суточная/размерная rotation и общий disk budget действуют с учётом legacy и pinned snapshots; quota/error/retry не приводят к unlimited росту.
- [x] A06: logs full/tail/API совместимы, stats доступны независимо от sink; flush/close и timeout безопасны, пределы зависшего I/O и потери логов описаны.
- [x] A01: intent, catalog, observations, overlays и durable workflows имеют отдельные типы/владельцев/операции записи; production не использует универсальный snapshot upsert для intent.
- [x] A01: observation/sync не меняет пользовательские настройки, не воскрешает удалённое и не пишет в новое поколение; revisions/fencing не зависят только от wall clock.
- [x] A01: commit/apply/retry/restart contracts выполнены; миграция сохраняет данные, conservative intent, priorities/FIFO/policy и существующие blocks/journals.
- [x] A02: state changes и их эффекты проходят через единый проверяемый механизм событий/переходов; обходных прямых присваиваний в command/callback paths нет.
- [x] A02: streaming/operations ортогональны режиму работы; все рабочие states/errors имеют определённые условия входа/выхода, а legacy aliases явно ограничены совместимостью.
- [x] A04: repository больше не владеет global Lifecycle mutex; общий application coordinator корректно сериализует команды одного объекта и освобождает registry/reservations.
- [x] A04: длительный I/O, JSON/SSE и task joins не удерживают общеприкладные/global admission locks; независимый торрент обслуживается при заблокированной длительной операции другого.
- [x] A04: scheduler slots/FIFO, storage roots/aliases, temporary eviction/leases, migration/deletion и shutdown сохраняют cross-torrent инварианты без старой global сериализации.
- [x] Read models/API/UI согласованы и защищены от старых поколений/версий; CORS, endpoint compatibility и гарантии фаз 34–38 не ослаблены.
- [x] Детерминированные failure/concurrency/upgrade/reopen tests, обязательные race/vet/build/frontend/fork проверки проходят; пропуски и платформенные ограничения перечислены честно.
- [x] README/config examples, state/transition/coordinator contracts и `internal/engine/CONCURRENCY.md` актуальны; создан `tasks/implementation/phase39-report.md` с результатами каждого блока и DoD.
