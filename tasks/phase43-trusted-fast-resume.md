# Фаза 43: Trusted fast resume с устойчивыми checkpoints

## Статус и зависимости

- Статус: **фаза 43 завершена, блоки 1–6 и все пункты DoD выполнены**. Default fast/config/REST/SSE/GUI, расширенная subprocess crash matrix, SQL busy/lost-ack/flush faults, opt-out/re-enable, WAL transaction и actual Linux fsync measurements реализованы и проверены. Финальные root/fork/frontend checks и native Linux прошли локально; успешный CI, закрывающий ожидавшийся native Windows gate, подтверждён пользователем сообщением «CI прошел». Основание и границы evidence — в [отчёте](implementation/phase43-report.md).
- Зависимость: фаза 42 завершена; native Windows CI подтверждён пользователем. Основание — [контракт хранения](phase42-storage-contract.md) и [отчёт](implementation/phase42-report.md).
- Начинать реализацию только по отдельной команде пользователя. Блоки выполнять последовательно, после проверки приёмки предыдущего.
- После каждого значимого блока обновлять `tasks/implementation/phase43-report.md`: изменения, миграции, тесты, измерения, DoD и отклонения. Следующую фазу автоматически не начинать.

## Цель

При повторном открытии управляемого storage по умолчанию восстанавливать ранее проверенные pieces из пригодного resume checkpoint без повторного полного чтения payload, в том числе после аварийного выхода. Ускорение не должно превращать прежние disposable completion hints в безусловное разрешение чтения или upload.

Первая версия поддерживает частично скачанные v1/v2/hybrid торренты и доверяет только неотозванным pieces из завершённого устойчивого checkpoint после облегчённой проверки metadata/evidence. Crash/dirty shutdown сами по себе не аннулируют весь checkpoint. Отсутствующее/несовместимое resume state, незавершённая storage mutation или признаки изменения данных требуют проверки по правилам ниже. Политика похожа на qBittorrent, но не копирует конкретную реализацию libtorrent и не обещает идентичного поведения.

## Модель доверия и границы

1. По умолчанию режим `fast`: доверие пригодным resume checkpoints с облегчённой проверкой файлов. Режим `verify` явно включает повторную hash-проверку при каждом открытии. Настройка `HUB_FAST_RESUME_MODE=fast|verify` / `--fast-resume-mode`, изменение требует restart. Решение о default принято пользователем при планировании и теперь реализовано.
2. `fast` предполагает, что payload обычно изменяет только сервис. Это эксплуатационное допущение, а не доказанная исключительность и не opt-in администратора. Предупреждение о внешних writers обязательно в README/config/GUI. После внешнего редактирования, sync или восстановления backup пользователь должен запустить recheck; read-only backup допустим при документированной согласованности снимка.
3. Межпроцессный exclusive lock защищает управляемый download root во время работы сервиса; SQLite ownership одной записи недостаточно. Это не защита от произвольного внешнего процесса. Если надёжный lock недоступен, trusted restore запрещён.
4. Размер, timestamps, inode/file ID, volume marker и выборочное hashing — только основания для отказа при несовпадении, не доказательство отсутствия внешней правки. Same-size правка с восстановленным timestamp в `fast` может остаться незамеченной. Данные могут быть прочитаны/отданы до обнаружения скрытой порчи. Для гарантии проверки содержимого перед использованием нужен `verify`.
5. Не заявлять защиту от bit rot, злонамеренного локального доступа, клонирования тома, отката согласованного backup или storage hardware, ложно подтверждающего flush. Для таких условий остаётся `verify`. Неподдерживаемые FS/flush/locking semantics не получают best-effort доверие.
6. В фазу входят periodic checkpoints и reuse пригодного subset после аварии. Не входят filesystem watcher/change-journal интеграция, sampling вместо проверки, обязательный периодический scrub, immutable snapshot backend, новая БД и mmap. Эти механизмы не следует обещать как уже существующие триггеры.

### Триггеры перепроверки и fallback

| Сигнал | Поведение |
| --- | --- |
| Пригодный checkpoint, совпавшие evidence, нет отзыва | Restore subset без hashing, даже после crash |
| Checkpoint отсутствует, повреждён, неизвестной версии или превышает budgets | Обычная проверка; старые hints не повышаются до trust |
| Размер/mtime/file identity не совпали | Отозвать trust для затронутого файла и всех пересекающих его pieces, включая v1 boundaries; проверить перед чтением/upload |
| Файл отсутствует, access denied или ошибка чтения/короткое чтение | Отозвать затронутый trust, сохранить соответствующий storage block/диагностику; не создавать файл и не начинать repair автоматически |
| Сменились mapping/metainfo/volume, выполнен locate или известное восстановление backup | Отозвать checkpoint владельца; проверка после восстановления корректного storage, не на подменённом томе |
| Незавершённый move/repair/delete/recovery journal | Сначала существующий workflow/gates, затем проверка; checkpoint не разрешает обход журнала |
| Явный recheck либо режим `verify` | Отозвать соответствующее доверие и выполнить hash-проход |
| Только crash/нечистая остановка | Валидировать сохранённый subset и durable revocations; это не самостоятельный триггер полного hashing |

Во время нормальной записи изменение evidence ожидаемо, но после crash без завершённого нового checkpoint оно может вызвать проверку затронутых файлов. Полное совпадение evidence не гарантирует обнаружения скрытой порчи. Потеря доверия к диапазону не требует автоматически перечитывать независимые файлы; если границы достоверно определить нельзя, fallback расширяется до всего владельца.

## Сохраняемые гарантии

- SQLite WAL и настроенный пул; SQL/FS I/O и ожидание flush выполняются вне library/coordinator/global locks. Короткая SQL-транзакция не охватывает filesystem syscall.
- `verify` сохраняет current-open verified-only policy. Default `fast` сознательно изменяет эту гарантию: readiness может получить отдельно валидированный checkpoint текущего attachment без нового hashing; старый `piece_completion` таким checkpoint не является. Это явное изменение политики фазы 42, а не равноценное доказательство целостности.
- Rooted containment, no-replace, aliases, storage ownership, record/mapping/operation fencing, volume gates и retained data фазы 42 не ослабляются.
- Paused intent, selection, temporary policy, sticky DiskFull, FIFO, CORS `*`, HTTP Range, streaming QoS/refcount/debounce и transfer limits сохраняются. Успешный restore не означает Resume.
- API/GUI не выполняют stat/hash/flush на чтении. Checkpoint не выдаёт исторические bytes за выполненную в этой сессии hash-проверку.

## Реестр задач

| ID | Результат |
| --- | --- |
| F43-01 | Модель доверия, протокол checkpoint/restore, baseline и численные budgets |
| F43-02 | Versioned SQLite checkpoint, durable invalidation и exclusive instance ownership |
| F43-03 | Periodic quiesce/flush/seal и восстановление после clean/crash без повторного hash доверенных pieces |
| F43-04 | Интеграция всех mutations, recheck, aliases и lifecycle fencing |
| F43-05 | Настройка, диагностика и честная проекция API/GUI |
| F43-06 | Fault/crash tests, upgrade, измерения и native platform acceptance |

## Блок 1 — Контракт и baseline

- Проверить исходники `internal/completion/sqlite.go`, engine restore/close, fork VerifyOnOpen, storage readers/writers, piece hashing, v2 layers и прямые HTTP paths. Составить полный список точек изменения payload и потребителей readiness.
- Зафиксировать `tasks/phase43-fast-resume-contract.md`: режимы, schema/version, last committed checkpoint и независимые durable revocations, dirty session, sealing generation, идентичности, порядок FS/SQL, ошибки/fallback и additive wire-поля. Dirty session не эквивалентна invalid checkpoint. Это результат блока, а не предположение о готовом документе.
- Checkpoint связывает metainfo digest и geometry, record ID, storage ID, mapping revision/layout/prefix, checkpoint generation, enrollment evidence, file inventory/identities и точный набор verified pieces. Hybrid aliases используют одного владельца и согласованное представление piece sets; некорректная длина bitmap/индексы/неизвестная версия отклоняются.
- Уточнить durable SQLite настройки: WAL сам по себе не доказывает устойчивость commit. Safety-critical invalidation/seal требуют подтверждённой durability (включая `synchronous`), с проверкой всех используемых соединений. Дополнительную стоимость измерить.
- Задать budgets: один seal worker; очередь не более 64 владельцев с coalescing, исходный periodic interval 60 секунд для изменившихся владельцев; metadata/file batches не более 128 entries; scratch buffer не более 128 KiB, без копии payload. До реализации назначить численные пределы checkpoint bytes/pieces/files, время quiesce admission и общий memory budget restore, включая дополнительные к существующим fork bitmaps allocations. Превышение даёт безопасный fallback/пропуск нового seal, не усечение. Не читать всю библиотеку в память; насыщение seal queue не теряет revocations.
- Baseline: clean restart частичного и полного torrent, смешанная библиотека; API readiness, first readable Range, hash payload bytes, stat/open, SQL writes/transactions, shutdown time и peak memory. Указать среду и версию Go.

**Приёмка:** доверенная модель, отличие от прежней гарантии и каждый путь записи/чтения перечислены; proof ordering и budgets проверяемы тестами; baseline воспроизводим. Нельзя объявлять evidence доказательством отсутствия внешних правок.

## Блок 2 — Durable state и отзыв доверия

- Additive SQLite migration: отдельный versioned checkpoint namespace. Старый `piece_completion` остаётся disposable bounded cache; его асинхронная очередь с допустимой потерей hints не используется для safety-critical writes.
- Получить process lock до admission управляемого storage. Проверить второй процесс, нормальное освобождение и crash; не использовать существование lock-файла как доказательство живого владельца. Зафиксировать ограничения сетевых FS и нескольких путей к одному root.
- Startup проверяет last committed checkpoint, durable revocations и evidence без изменения payload. Restore не потребляет и не аннулирует checkpoint: read-only session и crash без затронувших его mutations позволяют использовать его снова.
- До перезаписи уже доверенного piece durable revocation исключает его из persisted и live trusted sets. Новые writes в ещё не доверенные pieces не требуют отзыва независимого subset. Если syscall меняет больше данных (truncate/replace), отзывается весь затронутый file/piece set до эффекта. Изменение topology/mapping отзывает checkpoint владельца.
- Участок validate → attach и revocation → write защищён существующим mutation ownership и attachment fencing. Ошибка/неопределённый commit revocation запрещает соответствующую запись до выяснения результата. Запись dirty-session marker сама по себе не заменяет durable revocation.
- Revocations хранятся на storage owner, не только на hash alias, и связаны с checkpoint generation. Старый seal/callback не может стереть более новый отзыв. `verify` и explicit recheck отзывают предыдущие checkpoints до прохода; обратное переключение не оживляет отвергнутые данные.

**Приёмка:** SQLite integration tests с WAL/reopen доказывают dirty-before-overwrite и повторное использование нетронутого checkpoint после crash; старые hints и неизвестные версии не дают trust; второй процесс не получает storage admission.

## Блок 3 — Seal и restore

- Seal выполняется периодически для изменившегося storage и при штатном закрытии: временно закрыть mutation admission владельца, дождаться всех writers/hash/completion/layer updates и открытий. Для online seal не делать Drop и не обрывать read-only streams; если безопасный quiesce не достигнут за бюджет ожидания, отложить seal. Снять согласованный verified/trusted piece set; checkpoint не включает неуспешные/неполные pieces и не повышает readiness сам по себе.
- Порядок: **quiesce → join writers → flush всех относящихся к checkpoint payload файлов и необходимых namespace изменений → durable atomic seal в SQLite → reopen mutation admission**. Payload fsync и SQL commit не взаимозаменяемы. Между snapshot/flush/seal нет новых writers. Новый seal атомарно заменяет предыдущую generation и учитывает её revocations; дальнейшая перезапись требует нового отзыва по блоку 2.
- Failed flush, ENOSPC/DiskFull, отмена, shutdown deadline, commit failure и зависший syscall не публикуют новый checkpoint. Предыдущий checkpoint остаётся кандидатом только с учётом durable revocations и повторной проверки evidence. Deadline не освобождает ресурс за незавершённую задачу. Shutdown сначала завершает seal tasks, затем закрывает SQLite; не использовать cancel уже остановленного общего контекста для обещания успешного seal.
- Не создавать файлов ради restore. Отсутствие, truncation/replacement, несовместимая geometry, volume mismatch, неизвестная inventory или pending journal дают явный fallback. Проверять evidence через rooted handles; имена и cached inventory не заменяют открытие. Zero-length readiness требует фактического безопасного открытия.
- Fork принимает validated restore token текущего attachment, не произвольный `Complete=true` из completion callback. Проверить v1 shared pieces, v2 Merkle/piece-layer requirements и hybrid consistency; отсутствие необходимых проверенных layers означает fallback, а не синтез доверия.
- Доверять только sealed subset: остальные pieces проходят действующую ограниченную очередь фазы 42. Явный recheck отзывает доверие до начала прохода. Чтение, upload, direct HTTP/archive и torrent-reader используют единый gate, включая отзыв позднего token.

**Приёмка:** clean и crash reopen читают пригодные trusted ranges без startup hash payload reads; revoked/incomplete ranges не получают readiness. Periodic checkpoint работает во время загрузки без разрыва streams и без голодания writes. Barrier tests проверяют каждый порядок flush/commit/admission, отсутствие deadlock и доступность unrelated torrent.

## Блок 4 — Все mutations и recovery

- До write/truncate/create/import/replace и исправления ошибочного piece должен существовать durable отзыв всех затрагиваемых trusted pieces либо checkpoint владельца. Охватить torrent download, v2 metadata/layer effects, repair, `.part` import, legacy migration/recovery, move/locate, delete и повторное использование root. Независимые pieces не отзывать только из-за активности сессии. Любой bypass — незакрытый DoD.
- Move/locate/reconcile и смена volume/mapping не переносят trust по совпадению fingerprint. В первой версии checkpoint отзывается, после изменения расположения требуется обычная проверка и новый seal. Interrupted journal исключает trusted restore.
- Delete/re-add, aliases, Pause/Resume, Drop/Close/open races, смена selection и explicit verify используют существующий coordinator/fencing. Не создавать вторую независимую mutation lock систему. Selection не меняет piece validity, но не должна обходить dirty lifecycle.
- Результат failed/revoked recheck не может быть скрыт старым checkpoint. Safety blocks и недоступный storage имеют приоритет над trust. Сбой только оптимизации не блокирует обычную проверку; недоказанный dirty barrier блокирует соответствующие mutations.

**Приёмка:** матрица всех mutations покрыта tests со stale callbacks, aliases и restart; ни один старый checkpoint не разрешает чтение/удаление новой записи или нового mapping.

## Блок 5 — Настройка и наблюдаемость

- Документированная настройка `fast/verify`, default `fast` и предупреждение о внешних writers. Upgrade без явной настройки получает `fast`, но старые disposable hints требуют исходного hashing и создания нового checkpoint. Не добавлять включение per-request или доверие через одно наличие completion rows. Изменение режима допускает restart вместо небезопасного hot switch.
- Additive REST/SSE/GUI диагностика: configured mode, restore outcome, reason, checkpoint time/generation, trusted restored bytes отдельно от freshly hashed bytes. Точные имена закрепить в контракте и общих Go/TS fixtures; revisions — строки int64.
- Не маркировать restore как выполненный hash-проход. Existing progress/selection/readiness сохраняют смысл с явно описанным источником подтверждения; counters не складывают пересекающиеся bytes дважды. GUI объясняет fallback и предоставляет существующий recheck, не снимающий Pause.
- Не повторять bitmap/file inventory в compact SSE. Наблюдаемость читает cached facts и сохраняет snapshot/attachment fencing. Проверить TorrServer compatibility и отсутствие новых stat/hash на API polling.

**Приёмка:** пользователь различает hashing, trusted restore и fallback; default upgrade меняет политику явно в документации, но не повышает доверие к старым hints; недоступность оптимизации не выглядит потерей скачанных данных.

## Блок 6 — Проверки, измерения и выпуск

| Область | Обязательные сценарии |
| --- | --- |
| Clean | Полный/частичный v1/v2/hybrid, boundary/Skip/zero-length, aliases, paused и смешанная библиотека; trusted subset без повторного hashing |
| Crash | Kill subprocess до/после revocation, write, snapshot, каждого flush и periodic/shutdown seal commit, до attach; нетронутый checkpoint переживает crash, отозванные pieces не восстанавливаются; reopen SQLite и настоящих файлов, не только mock error |
| Failures | Payload/namespace flush error, SQLite busy/commit error, неопределённый commit, ENOSPC, access denied, deadline, blocked syscall; trust не повышается |
| Evidence | Missing/truncated/replaced файл, метаданные/bitmap corruption, неизвестная версия, volume/marker mismatch, unsupported FS, active journal; безопасный fallback |
| Trust model | Явный verify обнаруживает same-size правку с восстановленным mtime через hashing; default fast документированно не обещает её обнаружение. Изменение mtime/size/identity отзывает затронутые pieces; ошибка/короткое чтение отзывает диапазон. Второй экземпляр не получает lock |
| Lifecycle | Write/recheck/move/locate/delete/re-add + restore/seal; stale token, alias races, repeated Drop/Close, Pause, shutdown, незавершённый layer update |
| Limits | Saturation, oversized checkpoint, очень много files/pieces, bounded batching/memory, отсутствие worker на каждый торрент и SQL transaction на каждый chunk |
| Compatibility | Upgrade с текущими disposable hints и pending phase41/42 journals; clean/dirty DB reopen, opt-out/re-enable; согласованный backup и downgrade procedure |

- Fault injection дополнять отдельными subprocess crash tests; process kill не доказывает поведение при потере питания. Для power-loss заявлять только порядок, основанный на документированных FS/SQLite durability primitives, с явными ограничениями среды.
- Сопоставить baseline и результат на одинаковых fixtures: payload bytes hashing, time to first Range/API, весь restore, stat/open, SQL amplification, seal/shutdown latency и peak memory. В clean supported fixture — ноль startup payload reads для hash доверенных pieces; разрешены metadata reads и фактический Range read. Не устанавливать нестабильные CI thresholds по MB/s.
- Финальный `make check`: root tests/race/vet/build, fork `.` и `./storage` (и другие изменённые пакеты), frontend tests/build. Новый durability/locking protocol требует native Linux/macOS/Windows suites; cross-compile не считается исполнением. Если платформа недоступна, оставить DoD открытым, не объявлять фазу завершённой.
- Обновить README, phase42 contract в части новой default fast policy, `internal/usecase/STATE.md`, `internal/engine/CONCURRENCY.md`, конфигурацию и итоговый отчёт с точными командами/evidence. Не переписывать исторические результаты фазы 42 как будто fast resume существовал тогда.

## Definition of Done

- [x] F43-01: контракт, доверенная модель, baseline и численные budgets зафиксированы до реализации runtime; policy primitives покрыты тестами.
- [x] F43-02: checkpoints отделены от hints; durable revocation barrier и exclusive instance ownership доказаны DB/subprocess тестами; load не аннулирует нетронутый checkpoint. Подключение payload hooks и runtime restore — F43-03/F43-04.
- [x] F43-03: installed periodic/shutdown quiesce/flush/seal и validated backend-open restore проверены на full/partial v1/v2/hybrid без hashing trusted subset; process crash не вызывает полный fallback, revoked/unsealed ranges не получают trust. Реальные reader/writes и HTTP Range, root/fork/race/vet/build и native Linux прошли; mode/config/diagnostics выполнены в F43-05, итоговый Windows gate подтверждён пользователем через CI.
- [x] F43-04: core write/hash/layer/namespace и application repair/migration/move/locate/delete/recheck paths защищены durable barriers; aliases/read leases/Close/re-add проверены, journals/volume/safety gates не обходятся. Финальный Windows gate закрыт пользовательским подтверждением CI.
- [x] F43-05: default fast, restart-scoped strict verify и cached attachment-fenced REST/SSE/GUI diagnosis реализованы. Disjoint restored/fresh counters, общий Go/TS string-generation fixture, upgrade без adoption hints, paused semantics и HTTP polling без storage I/O проверены; policy/backup/downgrade ограничения описаны. Full root/fork/frontend check — PASS.
- [x] F43-06: расширенные subprocess snapshot/payload/namespace/periodic/shutdown seal crash tests, SQL busy/lost-ack и explicit flush errno cases, upgrade/opt-out/re-enable и сравнительные WAL transaction/online-seal/actual Linux fsync measurements выполнены. Ограничения external edits, power loss и local FS явно описаны; последний Windows platform gate закрыт пользовательским подтверждением CI.
- [x] Root/fork/frontend checks и обязательные native Linux/macOS/Windows suites проходят; локальные evidence и пользовательское подтверждение успешного CI, закрывающее Windows gate, сохранены в отчёте.
- [x] README/config/state/concurrency contracts и `tasks/implementation/phase43-report.md` обновлены; измерения/команды/evidence, ранее отложенная Windows проверка и её последующее CI подтверждение отражены. Фаза закрыта.
