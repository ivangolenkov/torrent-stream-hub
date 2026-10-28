# Фаза 44 — отчёт об имплементации

## Follow-up: независимые managed writes без file-wide convoy

По команде пользователя реализован локальный follow-up фазы 44. Развёрнутый NAS,
его конфигурация, payload и процессы не изменялись. Частота completion sync,
хеширование, WAL/FULL, DiskFull, Range/CORS/QoS и application ownership не менялись.

### Контракт и реализация

- История Git подтвердила: exclusive `payloadMu` вокруг каждого chunk добавлен
  в фазе 43 (`bbfdb36`), а inline peer writes и Flush существовали до неё. В
  managed-модели после отказа от per-chunk external evidence эта file-wide
  сериализация больше не нужна для независимых записей.
- `storage/managed-ranges.go`: существующий `lockPayloadRange` теперь выбирает
  byte-range admission для ManagedPayload. **Непересекающиеся chunks одной piece**,
  разные pieces одного файла и pieces на v1 shared-file boundaries могут писать
  параллельно. Read/read совместимы; пересекающиеся операции с writer исключаются.
  Нельзя заменять byte ranges постоянным mutex на piece: он оставил бы convoy
  между chunks одной piece. Hashing, snapshot и publication запрашивают полный
  extent piece и потому ждут её writes.
- Существующий engine `pendingWrites` сохранён. Он joins обычный сетевой download
  перед hashing, но сам по себе не защищает прямые backend ReadAt/WriteTo и
  промежуток publication от прямого write. Range admission заменяет широкое
  исключение только в backend, не добавляет второй scheduler или источник trust.
- Порядок: lifecycle read для mutation/publication → shared file join leases по
  torrent offset → byte-range admission → существующие metadata/cache locks.
  Ни range metadata mutex, ни cache mutex не держатся через syscall/SQL/sync.
  Предшествующие конфликтующие waiters имеют приоритет; независимые запросы могут
  обходить очередь. Нет goroutine/buffer/постоянного lock на каждую piece. Один
  32-byte node на 64-bit существует только пока вызов active/waiting; O(concurrent
  calls) admission scan/memory, после drain head/tail пусты. Это не глобальный cap
  concurrent calls. Ожидание range синхронное, как прежний native payloadMu; уже
  выданный syscall не отменяется принудительно. Cancelable descriptor pool сохранён.
- Файловый payloadMu у managed reads И writes теперь SHARED: его единственная
  роль — join всех I/O для Close/Reset. Они по-прежнему получают lifecycle exclusive,
  затем file-exclusive leases до закрытия fd. Checkpoint lifecycle-exclusive joins
  всех writers и удерживает admission закрытым до конца sync/publish; readers могут
  продолжаться. Полный Recheck не читает старый cached inode. Strict backend
  сохраняет прежние exclusive file leases для per-file stat/mtime accounting.
- Writes всё ещё revoke persisted/live piece trust и стирают hash snapshot **до
  mutation**. Post-sync publication под full-piece read lease повторно проверяет
  исходный snapshot; overlapping write/revoke не может вернуть stale completion.
  File creation/upgrade coalesces через существующий descriptor pool и guards.
- `managed-files.go`: контракт retained descriptor выражен interface positional
  I/O/Sync/Stat/Close (production implementation — os.File). Это позволило тестам
  остановить/fail именно borrowed handle на WriteAt/ReadAt/Sync, не обходя backend.
  Первый общий check обнаружил regression при failed open: typed-nil os.File в
  interface вызывал ложный Close error/отрицательный open gauge и закрывал дальнейший
  доступ. Исправлено: descriptor публикуется в interface только при successful open;
  добавлен missing-open→write/hash regression test. Первый failed check не принят.
- WriteLeaseWait counters теперь отражают range/file-join contention managed I/O,
  не exclusive file lock. WriteLeaseHold.Active может быть >1 для одного файла.
  Lifecycle admission/cache upgrade/OS-internal waits туда не входят. Go/TS wire
  поля и число timing atomics не менялись. README, CONCURRENCY, STATE и план обновлены.

### Приёмка этого блока

- [x] `TestManagedPayloadIndependentWrites`: остановленный borrowed WriteAt не
  блокирует write/hash/publication соседней piece, в том числе shared boundary.
  Stale snapshot изменённой piece отвергается; checkpoint включает только verified.
- [x] `IndependentChunksOfSamePiece`: второй chunk пишет до возврата первого;
  full-piece snapshot/hash admission ждёт оба диапазона. OverlappingWriteWaits
  доказывает исключение совпадающих writes, не blocking соседнего диапазона.
- [x] `HashExcludesOnlyItsPiece` (hash и verified ReadAt): чтение стабильно, соседний
  write/hash проходит; overlapping write ждёт чтения, затем отвергает stale hash.
- [x] `ParallelWritesCheckpointBarrier`: blocked write не позволяет checkpoint
  publish, cancel возвращается без publish; после записи seal исключает unverified
  piece и блокирует новую mutation до callback completion. Повторная запись
  отзывает только что сохранённый subset до syscall.
- [x] `ParallelWritesJoinResetAndClose`: два остановленных writes одновременно
  admitted; Reset/Close ждут оба и закрывают fd. Существующий reader/reset join
  test сохранён. `RangeFairness` проверяет writer priority и bypass соседних ranges.
- [x] `ParallelWriteErrorRevokesFile`: full/short write errors отзывают neighbor
  trust, retire descriptor ровно один раз, допускают guarded reacquisition.
  `ErrorRetiresAfterLastParallelBorrower`: ошибка одного writer не закрывает fd
  второго заблокированного writer; retirement происходит после последнего borrow.
  `ParallelWritesDuringSync`: neighbor writes не отвергают valid hash, same-piece
  writes и sync error не публикуют completion. Mandatory real Sync сохранён.
- [x] Core `TestManagedPayloadExplicitRecheck` усилен concurrent 1 KiB writes в один
  файл/одни pieces, затем **реальный** library hash v1/v2/hybrid восстанавливает
  completion; далее actual replacement-inode Recheck обнаруживает corruption.
- [x] Все managed storage race tests ×30 — PASS (4.252 s,
  `phase44-ranges-targeted-final.log`); полный engine/API race — PASS (6.550/17.900 s,
  `phase44-ranges-engine-api-race.log`). Core real-hash race ×30 — PASS
  (`phase44-ranges-real-hashes-race.log`, 2.479 s). Финальный полный fork race ×3,
  включая последний borrower-retirement test, — PASS (24.842/7.237 s,
  `phase44-ranges-fork-final-race.log`); повторный fork vet — PASS.
- [x] Native Linux arm64 полный storage ×3 и targeted core ×3 — PASS
  (`phase44-ranges-storage-linux.log`, `phase44-ranges-core-linux.log`). Windows
  amd64 core/storage cross-compile — PASS; native execution локально недоступен.
- [x] Финальный `make check` — PASS (`phase44-ranges-check-final.log`): root/fork
  tests/race, 44 frontend tests/build, vet и application build; usecase race
  309.767 s, fork race 9.804/4.759 s. Production source после старта не менялся.
  Последний borrower-retirement test добавлен после этого прогона и включён в
  последующий полный fork race ×3, Linux full storage ×3 и Windows cross-compile.
- [ ] Native Windows CI этого revision требуется повторить; прежние CI gates
  не объявляются достаточными для новых locks. Native macOS race выполнен локально,
  Linux выполнен в изолированном arm64 container; Windows execution недоступен.
- [ ] NAS performance/remaining storage stalls и общие F44-01/F44-07 — открыты.

### Ограниченное измерение накладных расходов

`BenchmarkManagedPayloadChunkWrite`, local macOS arm64, warm page-cache overwrite
16 KiB, без hashing/sync в timed loop, `-benchtime=1000x -count=3`:

| Mode | ns/op | volume guards/op | WriteStat/op | B/op / allocs |
| --- | --- | --- | --- | --- |
| strict | 42025 / 38888 / 41260 | 1 | 2 | 2512 / 46 |
| managed ranges | 1799 / 1769 / 1784 | 0 | 0 | 656 / 20 |

Новый range node добавил 32 B/1 allocation к прежним managed 624 B/19 allocs.
Это не сравнение NAS throughput и не доказательство ускорения относительно старого
managed mutex: другие число итераций/момент измерения. Доказательство снятого
convoy — barrier tests, где независимая операция завершается **до** release
остановленного writer. Системные WriteAt/fsync stalls и OS-internal descriptor
serialization этим изменением не устраняются. Sync batching в scope не входил.

Архитектурное уточнение к плану: оправдана замена широкой file lease на короткое
range-admission metadata, а не полное удаление exclusion — backend API должен
сохранять read/hash/publication safety вне engine pendingWrites. Все остальные
непринятые пункты DoD остаются явно открытыми.

## NAS: managed-cache работает, остаточные задержки внутри WriteAt

По просьбе пользователя выполнена только GET-диагностика обновлённого сервиса
`192.168.31.126:8080`. Последний запуск в логах — **2026-10-27 21:27:10 UTC**,
Resume 21:27:19 занял 34.714 ms. Код, конфигурация и payload сервера не менялись.

13 снимков health/torrents через 5 s: **21:29:16.946–21:30:16.983 UTC**, 60.037 s.

| Наблюдение | Результат |
| --- | --- |
| Useful reception | 183.234 MiB, среднее 3.052 MiB/s; интервалы 0.056–7.761 MiB/s |
| Peers / ожидающие writer leases | 33–37 / 22–30 |
| Managed cache | true; hits +12082, misses 4→4, evictions 1→1, open 3→3 |
| Actual guard/open/path/identity calls | за минуту новых нет; WriteStat/Account = 0 |
| WriteAt | +11726 calls, +57.771 s; lifetime max 1.696 s |
| Writer lease hold | +57.801 s, практически всё время внутри WriteAt |
| Writer wait | +1607.734 s суммарного ожидания разных goroutines; max вырос до 7.100 s |
| Completion sync | +89 calls, +24.654 s, среднее 277 ms; lifetime max 1.566 s; ошибок нет |
| Verification queue / active | 0–1 / 0–1 |
| Successful received pieces | 156→245, +89; bad pieces 1→1 |
| Freshly verified bytes | +178 MiB = 89 pieces ×2 MiB |
| SQLite completion | dropped=0, pending=0, ошибки не объявлены |

**Вывод:** прежний per-chunk guard/open/stat overhead действительно устранён на
этом запуске, но ровная скорость не достигнута. Задержки теперь наблюдаются в
самой записи payload. `file-torrent-io.go` держит exclusive **file-wide** lease
через WriteAt; `resume-mutation.go` использует один payloadMu на файл. В `peer.go`
запись синхронна с receive loop соединения. Медленная запись одного chunk поэтому
накапливает очередь peers, тормозит приём, затем даёт всплеск после разблокировки.
Суммарные wait/sync/write времена перекрываются и не складываются в wall time.

Sync остаётся значимой нагрузкой того же storage: один completion sync на каждую
успешную piece в этой выборке. Его file-wide publication lease ранее сокращён,
но это не устраняет конкуренцию fsync с WriteAt внутри ОС/устройства. Без системных
iostat/vmstat/dirty/writeback/mount metrics нельзя разделить диск, filesystem,
writeback throttling, NAS mount/container layers и доказать долю влияния sync.
Это наблюдаемая задержка OS write path, не доказательство аппаратной неисправности.

**Почему Verifying:** UI `TorrentTable.vue` показывает activity.verifying,
а health.go включает её при любой queued/active piece, включая обязательную
проверку новых данных. В минутной выборке +89 good received pieces точно совпадают
с +89 completion sync и +178 MiB fresh verification. Нет растущей очереди или
признаков полного Recheck; прежнее бессмысленное hashing отсутствующих файлов
не наблюдается. Startup отдельно пропустил 4513 incomplete pieces. Fast restore
принял checkpoint generation 51 от **18:11:09 UTC** (4.242 GiB); существующие
данные вне доверенного subset подлежат startup hashing. Поэтому fast не означает
полного отсутствия verify. Повторный shutdown в 21:27:07 предыдущего процесса
завершился deadline exceeded; это согласуется со старым checkpoint, но конкретная
причина отсутствия более свежего seal этим GET-набором не установлена.

В текущей сессии до 21:30:50 нет `error marking piece complete`,
`completion persistence failed` и массовых hash errors. Есть несколько некорректных
extended handshakes и smart-ban одного peer (21:28:05), но bad pieces за выборку
не растут; объявлять эти сообщения основной причиной storage queue оснований нет.
Отсутствие старой SQLite ошибки в этой сессии не доказывает устранения её причины.

Следующий блок после отдельного разрешения: воспроизведение медленного WriteAt
и file-wide convoy; проектирование bounded/coalesced writes и более узкого
исключения независимых piece ranges с сохранением lifecycle/revocation/shared
boundaries; оценка безопасного объединения completion sync без публикации до
durability barrier. Hashing и обязательный sync не отключать. Для attribution
низкого уровня нужны read-only host storage metrics. Изменение UI wording может
отделить проверку скачанных частей от полного Recheck, но не исправит I/O.

- [x] Подтверждены deployed ManagedPayload/cache reuse/bounded handles и отсутствие
  per-chunk guard/stat на реальном NAS.
- [x] Снята отдельная минутная выборка полезных байтов, writer/sync/verify counters.
- [ ] Контролируемое сравнение равных workloads/F44-01/F44-07 и общая приёмка фазы
  остаются открыты; 3.052 против прежних 1.693 MiB/s не является строгим A/B.

Evidence в OpenCode temp: `phase44-new-health.json`, `phase44-new-torrents.json`,
`phase44-new-samples.jsonl`, `phase44-new-logs.txt`, `phase44-new-logs-final.txt`.
Production source не изменён; тесты не запускались для этого диагностического блока.

## Windows CI: catalog path в progress-sync fixture

`TestProgressSyncCannotRestoreBytesAfterManagedRecheck` падал на первом
`persistForTest` с `ErrStateConflict`. Причина: `completedSafetyFile` сохранял
native storage path (на Windows `sync-replaced\nested\video.bin`) в поле
каталога `Path`, тогда как runtime использует display path `nested/video.bin`.
Legacy fallback в SaveCatalog принимает только точный slash-separated
root-prefixed вариант; Windows fixture ему не соответствовала.

В `internal/usecase/storage_safety_test.go` fixture теперь сохраняет
`FileInfo.DisplayPath`, как runtime, отдельно от native disk path. В
`internal/usecase/progress_sync_test.go` добавлена явная проверка сохранённого
`nested/video.bin` до persistence. Generation/revision fences, SaveCatalog и
проверка невозможности вернуть stale progress не ослаблены; runtime не менялся.

- [x] Targeted progress-sync/download/manual-delete tests, race ×5: PASS (34.371 s).
- [x] `go test ./...`: PASS (usecase 146.956 s).
- [x] Windows amd64 usecase cross-compile: PASS; `git diff --check`: PASS.
- [ ] Новый native Windows CI остаётся обязательным.
- [ ] Общая приёмка фазы 44 на NAS/comparative measurements остаётся открытой.

Отступлений от плана нет: исправлена платформозависимая тестовая фикстура.

## Windows CI: переносимая проверка закрытых managed descriptors

Пользователь сообщил о падении `TestManagedPayloadHotPathAndVerification` и
`TestManagedPayloadResetJoinsReaders`: после закрытия `os.File.Stat` на Windows
возвращает `GetFileType: The handle is invalid`, не `os.ErrClosed`.
В `storage/managed-files_test.go` обе post-close проверки заменены на непустой
`ReadAt`, который проверяет lifetime `os.File` и возвращает `os.ErrClosed`.
Проверка успешного Stat активного читателя до завершения Reset сохранена.
Runtime, закрытие handles и модель доверия не изменены; ошибка не замаскирована
проверкой «любая ошибка подходит».

- [x] Полный storage suite с race на macOS: PASS (2.504 s).
- [x] Windows amd64 storage cross-compile: PASS; `git diff --check`: PASS.
- [ ] Повторный native Windows CI: требуется, этот gate пока не закрыт.
- [ ] Общая приёмка фазы 44 на NAS и comparative measurements остаётся открытой.

Отступлений от плана нет: исправлены platform-specific test assertions.

## Уточнение модели доверия пользователем

После сравнения с qBittorrent/libtorrent пользователь сообщил: «Нам не нужны
более строгие обнаружения внешних изменений». Для следующего рефакторинга
зафиксировано владение активным payload сервисом, без гарантии немедленного
обнаружения внешних изменений; после них требуется остановка и Recheck.
Per-chunk filesystem refresh и повторные open можно пересмотреть в пользу
ограниченного переиспользования дескрипторов с lifecycle-инвалидацией.
Полученные pieces по-прежнему хешируются, existing untrusted data проверяются,
внутренние гонки, rooted containment и DiskFull остаются защищены. Новый контракт
volume/reopen/resume и descriptor lifecycle ещё требуется разработать; это не
разрешение игнорировать заведомо неверный том или возвращать устаревший trust.
В рамках этого уточнения изменены только план/отчёт, не runtime/сервер.

Последующая команда «Приступай к реализации» выполнена локально в managed-payload
follow-up ниже; ограничение «runtime не изменён» выше относится к моменту решения.

Статус: **основной runtime реализован; финальная приёмка/measurements не завершены**. Основание: [план](../phase44-selective-verification-and-download-concurrency.md). Пользовательский сервер не изменяется; все fixtures — временные.

## Актуализация platform gate

- [x] Native Windows CI gate **закрыт**: пользователь сообщил «CI прошел» и
  поручил отметить gate завершённым. Результат принят по подтверждению
  пользователя; агент независимо CI run не проверял.
- Открытые упоминания Windows ниже отражают состояние на момент соответствующих
  проверок и этим подтверждением заменены. Сравнительные measurements и NAS
  validation остаются незавершёнными; F44-01/F44-07 и фаза целиком не закрываются.

## Блок 1 — воспроизведение и контракт до runtime-изменений

`storage/verification_concurrency_test.go` детерминированно воспроизвёл `storage changed during verification` без внешнего writer: piece 1 пересекает a/b, после stat a и до stat b обычная запись piece 0 обновляет общую подпись a. Captured current[] оказывается старее mutable hashSnapshot. Baseline test PASS с ожидаемым конфликтом; после исправления он должен стать regression test, требующим успешной публикации.

Дополнительно обнаружено: library `pieceHashed(passed=true)` сообщает complete даже при ошибке `MarkComplete`. Cached backend gate затем не даёт читать bytes, но library bitmap может мешать повторной загрузке. Нужно возвращать incomplete при failed commit, не превращать storage fault в обвинение пира.

### Решения

- Negative startup evidence — **одноразовый immutable результат open**, только для initial completion/check admission. Не persistent trust, не общий cache для последующих writes/recheck. Отсутствие или короткий extent даёт известный incomplete, но не увеличивает hash counters. Snapshot собирается до публикации backend, после namespace/import/restore. После передачи initial state library callbacks освобождаются. Новые download writes всегда идут обычным hash-путём; recheck не использует устаревшее startup evidence.
- Evidence учитывает все contributing files и read-name policy `.part`/final, rooted errors не маскируются как missing. File length/size не подтверждает данные: все покрытые extents без checkpoint остаются unknown. Sparse-файлы не пропускаются по allocation.
- Не строить bitmap на каждый piece: ordered merged incomplete intervals ≤ file count, O(files) probes и O(pieces + files) начальный обход. Дополнительный budget ≤16 bytes на file до накладных расходов slices (два int индекса на interval, на 64-bit); исходный metainfo/catalog уже существует. Никаких payload buffers/workers.
- Для signature capture/publication использовать существующий `payloadMu`: lifecycle R → contributing payload read locks по порядку → file name lock/stat → freshMu (кратко). Begin и Mark держат payload lease только на свои metadata/publication шаги, **не на весь hash backlog**. Hash reads уже используют тот же lease. Внутренняя независимая запись между Begin/hash/Mark разрешена и обновляет общую signature; запись в тот же диапазон удаляет hashSnapshot, поэтому stale result отвергается. External mutations по-прежнему требуют сравнения identity/size/mtime. Rename/promotion требует проверки существующего lock order; избегать recursive payload RLock при ожидающем writer.
- SQL/FS не выполняются под Client.lock/global locks; сохраняются lifecycle ownership и revoke-before-write. Не добавлять новую глобальную систему locks. Ошибки MarkComplete не повышают completion; существующие queues остаются bounded, без автоматического busy retry.

### Baseline

macOS arm64, local temporary FS. `BenchmarkSelectiveVerificationStartup`: 10 файлов, 7233 pieces по **64 bytes**, не NAS geometry в bytes; existing payload 462912 bytes, missing payload 0. Networking disabled, in-memory completion, backend syscall counters; число events включает rooted path checks/open/fstat и не равно количеству stat syscalls. Startup-us включает fixture/client setup и close, не только attach. Measurements не доказывают улучшения NAS throughput.

Команда: `go test . -run '^$' -bench '^BenchmarkSelectiveVerificationStartup$' -benchtime=1x -count=3 -timeout=120s` в fork.

| Fixture | Прогоны ms | Hash attempts | Hash bytes | FS events |
| --- | --- | ---: | ---: | ---: |
| missing | 222.542 / 221.349 / 229.047 | 7233 | 0 | 14466 |
| existing | 640.528 / 564.805 / 561.976 | 7233 | 462912 | 94059 |

Baseline log: `sh_17a8554a7001hJ7LdyInDGy16b.out` в managed shell storage. Existing fast-resume/recovery fixtures фазы 43 будут перепроверены вместе с новой матрицей; partial/checkpoint/foreground throughput/SQL/memory measurements ещё не закрыты.

## DoD (текущий статус)

- [ ] F44-01: core cause и startup/strict complete/mixed baseline готовы; отдельные baseline invalid-checkpoint и сравнительный swarm throughput ещё нужны.
- [x] F44-02: negative startup planning, одноразовое evidence без positive completion, real 7233-piece geometry и linear probes проверены.
- [x] F44-03: обязательный hashing новых данных и совместимость restore проверены для v1/v2/hybrid; partial/sparse и checkpoints покрыты.
- [x] F44-04: публикация signature/write results, same-piece invalidation, revoke-during-commit и failed-commit library bitmap проверены.
- [x] F44-05: HTTP Range с локальным сидером, promotion полученного piece перед existing backlog, fairness/budgets и действующие safety suites проверены targeted runs; финальная общая приёмка отдельно ниже.
- [x] F44-06: cached skipped counters, общая Go/TS fixture, GUI и aggregate startup log; skips не выдаются за usable bytes или hashing.
- [ ] F44-07: полная матрица, checks, native platform gates и measurements.
- [x] README/storage/resume/state/concurrency contracts и отчёт обновлены; открытые gates и ограничения явно сохранены. Деплой не выполнялся.

## Блоки 2–3 — реализация и targeted приёмка

- `storage/initial-verification.go`, `storage/interface.go`, backend open и library `onSetInfo`: отрицательные ordered ranges собираются после restore, потребляются линейно один раз и освобождаются до готовности metadata. Нет bitmap/cached negative trust; completion=false/known подавляет только initial queue. Rooted metadata probes по файлам, guard errors остаются unknown. Existing bytes без checkpoint не становятся complete. Explicit recheck намеренно остаётся свежим полноценным проходом и не использует startup snapshot.
- `storage/file-piece.go`: Begin и commit signature защищены существующим contributing-file payload read lease. Lease освобождается **до** promotion/refresh, чтобы не получить recursive read-lock deadlock при ожидающем writer. После hint Set под freshMu повторно проверяется, что snapshot не отозван; MarkNotComplete удаляет его. Library не объявляет complete при ошибке MarkComplete и оставляет piece доступным для запросов без обвинения пира.
- Regression tests: deterministic соседний write при публикации; same-piece write и MarkNotComplete после Begin; revocation внутри hint Set; missing/truncated/shared boundaries/part-name evidence; guard rejection; после missing startup плохой piece отвергается реальным hashing, исправленный принимается; skipped bytes не попадают в checked/verified counters.
- `go test . ./storage -count=1 -timeout=120s` после блока 2 — PASS (6.692/1.627 s).
- После исправления публикации: `go test -race . ./storage -run 'TestSelectiveVerification|TestInitialIncomplete|TestVerification|TestVerified' -count=3 -timeout=90s` — PASS (1.187/1.444 s). macOS linker выдаёт warning LC_DYSYMTAB, тесты завершаются успешно.

После изменений тот же startup benchmark ×3:

| Fixture | Прогоны ms | Hash attempts | Hash bytes | FS events |
| --- | --- | ---: | ---: | ---: |
| missing | 3.129 / 2.218 / 2.524 | **0** | 0 | **20** |
| existing | 502.093 / 519.691 / 567.980 | 7233 | 462912 | 94089 |

Главный доказанный результат — 7233 → 0 бессмысленных hash attempts и 14466 → 20 rooted events для missing fixture, без уменьшения hashing существующего payload. Latency synthetic missing fixture уменьшилась; скорость реального NAS по этому benchmark не заявляется. Для existing добавилось 30 metadata events на 10 файлов; различия latency между прогонами не объявляются ускорением hashing.

Root complete/mixed baseline до runtime изменений также выполнен ×3 (`BenchmarkFastResumeBaseline`, full log `sh_17a85ef11001r3OegCPODqDRtS.out`): hash payload 8388608 B в каждом прогоне; Range complete 34.62/28.86/39.65 ms, mixed 25.17/37.97/37.48 ms; restore complete 18.76/13.24/21.34 ms, mixed 12.05/22.26/17.81 ms. Sampled heap 9.7–10.5 MB; total write transactions complete 104/106/105, mixed 126/115/114. Это strict/no-checkpoint reopen harness с synthetic read delay, не foreground swarm throughput. Полные raw counters доступны в логе.

Блоки 4–5 ещё проверяются; `make check` запущен, native Windows gate пока не закрыт.

## Блок 4 — приоритеты и наблюдаемость

- `verification-priority.go`, `torrent.go`: полученные целиком network pieces отмечаются в coalesced roaring subset **существующей hash queue** и получают приоритет выше фонового existing-data scan/selection, после reader-now/readahead. Метка удаляется при hash admission; никаких workers на chunk, busy retries или второго hash budget. Прежний fairness turn после восьми grants остаётся. `TestVerificationReceivedPiecePassesExistingBacklog` ставит tail piece уже существующей очереди и доказывает его следующий admission, а не ожидание полного прохода.
- При первом поступлении v2 piece layer library раньше превращала известный incomplete обратно в unknown и повторно ставила missing tail на проверку. Это найдено расширенной partial matrix; `setV2Hash` теперь сохраняет initial negative admission при установке **первого** layer без изменения bytes. Изменённый уже известный layer сохраняет прежний revoke path. При любом реальном hash admission initial flag очищается.
- `file-observations.go`, engine projection, models и GUI: additive `skipped_missing_bytes` для ещё не проверявшихся после startup skip pieces. Busy work исключает skip observation. Полные piece extents учитываются по пересечению с каждым файлом; на v1 boundaries счётчик может включать присутствующие bytes, непроверяемые из-за другого missing contributor. Это **не физический счётчик отсутствующих bytes**. Checked/restored/fresh/selection/downloaded semantics не менялись.
- Общая `internal/models/testdata/selective-verification.json` проверена Go/TS тестами. GUI явно пишет «skipped … not hashed», не повышает usable progress и не показывает idle new download как активную верификацию. Старые клиенты могут игнорировать additive поля.
- Rooted scanner работает с одним file evidence за раз и context check на каждом шаге. Ordered interval allocation lazy, capacity ≤65536 (≤1 MiB на 64-bit); overflow отбрасывает negative plan и даёт обычный hashing fallback, не trust. Новый received-piece bitmap ограничен cardinality/index domain существующей очереди; он не дублирует payload и не расширяет hash admission budgets.

## Блок 5 — проверки и текущие границы приёмки

- Новый core network test использует только loopback TCP seeder + httptest HTTP Range для v1/v2/hybrid. Перед разрешением hash barrier file observation verified=0 и piece complete=false; после — корректный `206` с 16 bytes, при этом весь 32 KiB torrent не скачан (готов один 16 KiB piece). Нет Responsive bypass. Это correctness/latency-order тест, не throughput benchmark.
- Partial/v1/v2/hybrid: существует 16 KiB из 32 KiB, startup читает/хеширует только покрытый piece; download tail затем обязан пройти hashing, skip исчезает. Sparse/full-size не даёт trust: хешируется весь **логический** extent, invalid zeros отвергаются. SEEK_DATA/HOLE может законно подать zeroReader без физических payload reads; тест проверяет BytesHashed, а не требует лишнего disk read.
- Дополнительные тесты: real 15 GB geometry без выделения payload, per-file metadata cost, отсутствие mkdir/write ради plan; unknown guard error не выдаётся за missing; corrupted downloaded bytes отвергаются; successful retry после failed MarkComplete восстанавливает complete.
- Root observation test (`TestAbsentStartupIsNotActiveOrVerifiedProgress`) и model contract — PASS ×3. Frontend — **44 tests PASS** (12 files), включая общий fixture и отсутствие ложного active verification/readiness.
- macOS targeted race ×5 после исправления v2 requeue — PASS. Расширенные старые checkpoint/write/crash suites также проходят; полная окончательная проверка отмечается ниже по завершении.
- Первый `make check` — PASS: root tests, root race, fork tests/race, frontend, vet и build. Он начат до последних расширений матрицы/received-priority; поэтому запущен повторный `phase44-final-check.log`, а latest fork дополнительно проверяется full race отдельно. В первом прогоне usecase unit 149.395 s, race 303.703 s; fork race 9.009/3.318 s. Нельзя выдавать первый прогон за окончательное evidence всех поздних изменений.
- Native Linux arm64 final selective/checkpoint/verification и storage write/checkpoint suites — PASS ×3, включая loopback HTTP. Бинарники собраны Go 1.25.8/CGO=0, исполнены в локальном Docker Linux kernel, image `golang:1.26-alpine`, `--network none`, 2 CPU/1 GiB; read-only mount только test binary, payload/SQLite в native container FS. Это не NAS acceptance и не Linux race. После последней received-priority правки suite будет пересобран/повторён.
- Windows amd64 core/storage binaries успешно cross-compiled. **Native Windows execution недоступен и не объявляется пройденным**; `.github/workflows/checks.yml` расширен на новые core/engine scenarios. CI не запускался агентом.

Повторный strict complete/mixed benchmark ×3 (`phase44-after-resume.log`):

| Fixture | Hash payload B | Range ms | Restore ms | Payload stat | Sampled heap B | Write transactions |
| --- | ---: | --- | --- | ---: | --- | --- |
| complete | 8388608 | 31.50 / 34.28 / 53.15 | 12.35 / 11.71 / 23.70 | 3268 | 9662608 / 10320352 / 10454384 | 122 / 125 / 98 |
| mixed | 8388608 | 28.90 / 30.39 / 29.45 | 12.19 / 12.07 / 12.35 | 2860 | 10201624 / 10441808 / 10446168 | 117 / 124 / 133 |

Hash volume и actual Range (16 B) сохранены; +16 metadata events в root harness объясняются startup probes. Вариативность SQL transactions/scheduling и фоновые checks не позволяют заявлять улучшение existing-data throughput или latency. Allocation 22.4–23.0 MB/op, sampled heap ~9.7–10.5 MB; sampling не доказывает instantaneous peak RSS. Seal/shutdown protocol не изменён.

### Оставшаяся работа / отклонения от полного плана

Фаза **не закрыта**: остаются native Windows CI и отдельные сравнительные baseline/results для valid partial/invalid checkpoint и download throughput под конкурирующим existing-data backlog. Корректность checkpoints покрыта действующей phase-43 matrix, но это не подмена отсутствующих сравнительных measurements. Явный Recheck остаётся свежим полным проходом; startup negative evidence специально не переиспользуется, поэтому ручной recheck отсутствующих данных всё ещё может делать per-piece попытки. Отдельной оптимизации этого пути в текущем результате нет. Никаких заявлений о решении всех проблем скорости NAS или о скрытой same-size/same-mtime порче в fast не делается.

Логи текущей приёмки — `/private/var/folders/zg/l0438jl160s4wr89pw16vh4c0000gn/T/opencode/phase44-*.log`. Отдельные неуспешные targeted прогоны при расширении тестов обнаружили v2 requeue и ошибочную тестовую предпосылку о physical reads sparse-файла; runtime v2 путь исправлен, а sparse assertion заменён на фактический logical BytesHashed без ослабления integrity check. Raw upstream BytesCompleted включает dirty network chunks и поэтому не используется как доказательство verified readiness.

### Latest-code дополнительные результаты

- После последней scheduler правки полный fork race `go test -race . ./storage -count=1 -timeout=120s` — PASS: 8.557 / 2.831 s (`phase44-fork-final-race.log`).
- Latest-code root `go test -count=1 ./...` — PASS, включая repository 69.220 s и usecase 149.155 s (`phase44-root-latest.log`).
- Native Linux core пересобран после received-priority изменения, selective/verification/checkpoint suites ×3 — PASS (`phase44-linux-core-latest.log`). Native storage final ×3 также PASS (`phase44-linux-storage-final.log`); после этого storage runtime не менялся. Windows core повторно cross-compiled, storage binary также собран; native Windows по-прежнему не запускался.
- `BenchmarkFastResumeResult` ×3 (`phase44-fast-result.log`) подтверждает сохранение phase-43 checkpoint reuse: complete — **0 hash payload bytes**, 13 write transactions/op, sampled heap 7.74/7.97/8.04 MB; mixed — **2097152 hash payload bytes** для недоверенного остатка, 45 write transactions/op, sampled heap 9.36/10.01/9.88 MB. First Range complete 33.66/26.19/19.87 ms, mixed 29.99/22.36/23.65 ms. Allocated complete 2.67–2.76 MB/op, mixed 7.47–7.53 MB/op. Actual Range 16 B учитывается отдельно: direct-path read не попадает в backend IORead counter. Это проверка сохранения существующего fast-resume поведения, **не** приписывание его выигрыша фазе 44 и не pre/post baseline valid checkpoint.

Повторный общий `make check` **завершился успешно** (`phase44-final-check.log`): root tests/race, fork tests/race, 44 frontend tests, vet и application build. Root usecase race — 304.718 s, repository race — 137.934 s; финальный fork race — 8.656/2.821 s. Latest-code root unit suite дополнительно выполнен после последней scheduler правки, как указано выше. macOS LC_DYSYMTAB linker warnings не привели к ошибкам тестов/сборки.

Не закрыты native Windows CI, invalid-checkpoint comparative measurements и controlled download-throughput baseline/result; к следующей фазе не переходить. Фаза остаётся частично принятой, несмотря на успешные локальные checks.

## Follow-up 27 октября: completion sync и просадки download

Команда пользователя после read-only диагностики: «Приступай к следующему шагу» — воспроизвести ожидание sync/lease, сузить критическую секцию без ослабления проверки и добавить измеримость. Это продолжение фазы 44, **не деплой и не разрешение менять NAS**.

### Наблюдения работающего сервиса

- В новом запуске `00:06:23` — `initial verification skipped absent data ... incomplete_pieces=7233`; прежних массовых ENOENT и signature-conflicts в полученных логах нет.
- GET sampling 24 раза за 69.08 s (`00:11:27–00:12:36 UTC`): 41–44 connected, 39–42 seeds, UI speed 0–12.55 MiB/s, useful counter average 1.91 MiB/s, wire payload average 2.26 MiB/s, verified bytes +150 MiB. Hash queue 0–1; не повторение initial backlog.
- BT Health сообщает `completion persistence failed`, dropped=4; за sampling dropped не растёт. Это не доказанная причина просадок и отдельное наблюдение, а не скрытая/исправленная данным патчем ошибка SQLite.
- Секундный sampler без сглаживания усиливает видимость кратких пауз; наличие сидов не доказывает доступную отдачу. Причинность всех провалов на NAS **не установлена** без профиля реального sync/locks.

### Детерминированное воспроизведение и исправление

Новый `storage/completion_sync_test.go::TestCompletionSyncDoesNotLeasePayload` останавливает настоящий путь completion Flush барьером и проверяет возможность file payload writer lease. До изменения runtime тест **FAIL**: `completion fsync holds the file payload lease and blocks all neighboring writes`. Это воспроизводит механизм регрессии без случайного sleep/скоростного CI threshold.

`storage/file-piece.go::commitCompletion` для VerifyOnOpen теперь:

1. Сохраняет lifecycle read lease, синхронно выполняет обязательный Flush **без file payload lease**.
2. После Flush получает прежние contributing-file payload read leases, снимает current signature и сравнивает с исходным hashSnapshot.
3. Публикует hint и повторно проверяет snapshot под freshMu перед trust; затем освобождает payload lease до promotion/refresh, как прежде.

Сам Flush не меняет bytes. Проверяемые данные получены до его начала; same-piece writer до/во время/после sync удаляет snapshot и запрещает старый commit. Neighbor writer обновляет только учтённую общую подпись и не аннулирует неизменённый piece. External replace/truncate меняет signature и отклоняется. После получения publication lease ни один внутренний writer не может пройти между stat и publication. Close/seal по-прежнему ждут lifecycle lease. Для legacy non-VerifyOnOpen сохранена прежняя payload-lease область sync: у них нет snapshot fencing. Обязательный fsync не удалён, не отложен в фон и не заменён размером/именем файла.

### Измеримость и затронутые файлы

- `storage/io-timings.go`, `file-torrent.go`, `resume-mutation.go`: девять int64 atomics на backend open (72 B без padding), фиксированная память, без очередей/новых workers. Completion Flush attempts/errors/in-flight/total ns/max ns; contended payload writer lease count/in-flight/total ns/max ns. В uncontended write path — TryLock без чтения часов; часы используются только при contention и при completion Flush.
- `storage/interface.go`, `file-client.go`, `storage-io.go`: callback/публичный accessor только cached atomics, nil/closed/unsupported не выдают данные старого attachment.
- Engine `health.go`, models и TS: additive `GET /api/v1/health/bt → torrents[].storage_io`. Polling не делает stat/hash/sync/SQL и не ждёт payload/lifecycle locks. Общий Go/TS health-contract test расширен на именованный `StorageIOTimings`, не ослаблен; inline TS object заменён на отдельный interface после обнаруженного contract-test failure.
- Completion sync time — **весь piece Flush**, включая file opens/name-lock waits и contributing files, не изолированное kernel fsync. Write waits — contended file leases, не write syscall, lifecycle/seal или SQLite. Concurrent totals могут превышать wall time. Count/total/max обновляются после завершения; active виден во время задержки. Это диагностика, не authority/readiness.
- README, CONCURRENCY/STATE и native CI selection обновлены. Ошибки остаются видимыми, не добавлены per-piece WARN/INFO.

### Контролируемые измерения ×3

`go test ./storage -run '^$' -bench '^BenchmarkCompletionSyncNeighborWrite$' -benchtime=1x -count=3` (`phase44-sync-bench.log`), macOS arm64/local temp FS, payload 8 B, один neighbor write 4 B. Benchmark-only wrapper **реконструирует старую область read lease на время Flush**; runtime содержит только новое поведение. В обоих вариантах искусственно добавлены 20 ms задержки и сохранён настоящий file sync. Это не запуск старого бинарника, не end-to-end swarm throughput и не оценка задержек NAS.

| Scope | Neighbor write, µs (3 runs) | Completion Flush, µs | Payload writer wait, µs |
| --- | --- | --- | --- |
| legacy sync lease (test-only) | 30626 / 27670 / 28037 | 30483 / 27593 / 27971 | 30453 / 27592 / 27968 |
| narrowed runtime scope | 32.67 / 33.50 / 31.29 | 26254 / 25949 / 25153 | 0 / 0 / 0 |

Sync остался медленным; соседняя запись перестала зависеть от его задержки. Корректность доказывают barrier tests, не нестабильный performance threshold. Общую скорость скачивания на сервере можно оценить только после отдельно разрешённого обновления и sampling новых counters.

### Проверки follow-up и DoD

- [x] Baseline barrier failure на старом runtime и PASS после сужения lease.
- [x] Тесты во время и после actual sync: same-piece write, external replace/truncate, revoke, sync error, independent neighbor и shared-file boundary. Invalid cases не публикуют completion; sync error остаётся error; sync сохраняет lifecycle fence.
- [x] Tests наблюдают in-flight sync и contention, completed count/total/max/errors, отсутствие изменения counters при polling и закрытый attachment. Engine projection и Go/TS wire contract проверены.
- [x] Targeted macOS race ×3: fork 2.079 s / storage 2.414 s (`phase44-sync-targeted.log`); расширенная latest storage matrix ×3 — PASS. Engine health race ×3 — PASS.
- [x] Latest полный fork `go test -race -count=1 . ./storage` — PASS: 9.050 / 3.033 s (`phase44-sync-fork-race.log`). Frontend — 44 tests PASS. `git diff --check` — PASS.
- [x] Native Linux arm64 storage selective/signature/sync/checkpoint/mutation suites ×3 — PASS (`phase44-sync-linux.log`); binary собран Go 1.25.8 CGO=0 и исполнен в Docker native Linux FS, не NAS.
- [x] Native Linux arm64 core selective/verification/checkpoint/cached-timings suites ×3 — PASS (`phase44-sync-linux-core.log`).
- [x] Windows amd64 core/storage cross-compile — PASS; **native Windows execution остаётся открытым gate**, не заменяется сборкой.
- [x] Полный follow-up `make check` — повторный прогон завершился успешно (`phase44-sync-check.log`): root/fork tests и race, 44 frontend tests, vet и application build. Root usecase race — 296.687 s, repository race — 136.604 s; fork race — 8.769 / 2.827 s. Первый прогон остановился на несовпадении Go/TS health contract; исправление и targeted contract test прошли. Предупреждения macOS linker LC_DYSYMTAB не привели к ошибкам тестов/сборки.
- [ ] Подтверждение улучшения публичного swarm на NAS и разбор отдельной persistence-ошибки не выполнены. Сервис, его конфигурация и payload не менялись.

Открытые F44-01/F44-07 (invalid-checkpoint comparative measurements, controlled swarm baseline/result, Windows native) не закрываются этим локальным microbenchmark. Фаза 44 остаётся частично принятой.

### Исправление Windows CI: нулевая измеренная длительность

Пользователь сообщил native Windows failure `TestWriteLeaseWaitTimings`:
`WriteLeaseWaitCount=1`, `Active=0`, `NS=0`, `MaxNS=0`. Contention зарегистрирован,
но короткий интервал может помещаться в один tick платформенных часов; тест
ошибочно требовал `NS > 0`. Та же предпосылка была в completion-sync assertion.
Оба теста теперь допускают нулевую длительность, сохраняя проверки barrier,
числа операций, active gauge, неотрицательных значений и согласованности max/total.
Добавлен детерминированный тест максимума с нулевыми и положительными samples.
Runtime measurement/locking не изменены: время не округляется искусственно вверх.
Комментарий к IOTimings фиксирует допустимость нулевых samples.

Проверки после исправления: targeted storage race ×30 — PASS (4.592 s);
полный `go test -race ./storage -count=1 -timeout=120s` — PASS (2.515 s);
Windows amd64 storage cross-compile — PASS. Повторный native Windows CI ещё нужен:
сообщённый неуспешный прогон не считается пройденным platform gate.

### Повторная диагностика скачков на сервере

После сообщения пользователя «Скачки скорости продолжаются» выполнены только
GET BT Health (24 snapshots через 2 s) и GET logs. Runtime/config/payload не
изменялись. Сервис уже возвращает `storage_io`; точная версия бинарника через
эти ответы не установлена. В логах новый запуск в 01:01:19 UTC.

Окно 2026-10-27 01:04:22–01:05:08 UTC, 46.06 s:

- Useful reception average 1.787 MiB/s; двухсекундные интервалы 0.20–4.73 MiB/s,
  displayed speed 0–8.28 MiB/s; peers 40–42. Это реальные колебания приёма,
  а не только эффект UI sampler.
- Во всех snapshots одновременно ждут file payload writer lease 30–35 операций.
  Завершилось 5270 contended acquisitions, суммарное ожидание 1512.47 s
  (параллельные ожидания складываются, это не wall time).
- В слабых интервалах mean завершившихся waits достигал 1.72–1.75 s.
  Имеется сильное evidence локальной очереди writer locks, но counters не
  идентифицируют держателя и не доказывают первичную причину его задержки.
- Completion sync errors=0; `completion persistence failed`/dropped=1 не менялись.
  Persistence failure остаётся отдельной нерешённой проблемой.

По коду `file-torrent-io.go:WriteAt` удерживает **exclusive file-wide** payload
lease через revoke/guard, open, before stat, WriteAt, after stat, accounting и
Close каждого сетевого chunk. Перенос completion Flush не устранил сериализацию
самих writers. Медленный syscall внутри одного writer или другой read-lease
holder способен выстроить очередь соседних pieces. Нельзя по этим данным
объявить виновным только fsync, SQL или сеть. Следующий локальный блок:
детерминированно воспроизвести задержку writer path, разделить время держателя
по стадиям и проверить безопасное сужение области без нарушения shared signature,
revoke-before-write и checkpoint/lifecycle fencing. Простая замена Lock на RLock
небезопасна для существующего before/after signature accounting.

Snapshots/logs сохранены во временной директории OpenCode как
`phase44-speed-followup.jsonl` и `phase44-speed-followup-logs.txt`.
Во время sampling другие клиенты запрашивали files и сделали storage POST
(409); это не изолированный benchmark. Фаза остаётся частично принятой.

### Writer-path follow-up по команде «Приступай» / «Продолжай»

#### Реализация и границы

- `storage/write_concurrency_test.go`: baseline barrier test
  `TestWriteCloseDoesNotLeasePayload` на прежнем runtime **FAIL** с сообщением
  `descriptor Close holds file-wide payload lease and blocks neighboring writers`.
  Барьер останавливает descriptor Close после фактического WriteAt/stat/accounting;
  TryLock доказывает лишнюю область исключения без speed threshold/sleep.
  Это доказательство механизма, **не доказанная основная причина на NAS**.
- `storage/file-torrent-io.go`, `file-io.go`, `file-io-root.go`: добавлена private
  capability descriptor-only Close, реализованная rooted writer. Только для
  VerifyOnOpen без part-files **последний** contributing descriptor закрывается
  после освобождения payload lease. Все bytes и before/after evidence обновлены
  до release; после него этот вызов не пишет следующие extents. Lifecycle lease
  сохраняется до возврата Close, errors/short-write precedence не теряются.
- Intermediate contributing-file Close остаётся под whole-range lease, как и
  unknown/buffered/mmap, part-file и non-VerifyOnOpen paths. Не вводятся новые
  fd caches, фоновые writers, range-lock registry или concurrent signature scheme.
  Rooted containment, application volume guard, durable revoke-before-write,
  snapshot invalidation, fsync, Close/seal/drop fencing не ослаблены.
- `storage/io-timings.go`: восемь фиксированных stage records с Count/Errors/
  Active/NS/MaxNS: write prepare, persisted-file guard, rooted open, descriptor
  stat, payload write, signature accounting, descriptor close и fully-acquired
  lease hold. Статистики содержат zero-duration samples без искусственного clamp.
  Payload short write считается stage error, существующий API error precedence
  сохранён. Stat failure по-прежнему не устанавливает evidence/trust и не
  меняет прежнюю политику возврата WriteAt; теперь виден в diagnostics.
- `internal/engine/health.go`, models/TS health contract и tests: additive
  `storage_io.write_*` records; только atomic snapshots, без polling I/O.
  40 дополнительных atomic int64, всего 49 = 392 B без padding на open.
  Память фиксирована, нет per-chunk logs/samples/queues. Часы/atomics добавляют
  bounded hot-path overhead; overhead на слабом NAS отдельно не измерен.
- README, CONCURRENCY/STATE и phase plan уточняют смысл стадий. `write_open`
  включает rooted checks и application IOGuard/volume validation; `write_guard`
  относится к persisted-file resume guard. `write_stat` — два descriptor stats,
  не все stat внутри open. Hold — после полного acquisition и до release,
  overlaps с вложенными стадиями; Hold.Errors всегда 0.

**Ограничение:** это узкое безопасное сокращение и измеримость оставшегося
узкого места. Open/stat/payload write/accounting пока эксклюзивны на весь файл.
Разрешать их параллельно без нового корректного shared-signature контракта
нельзя. Какой именно stage тормозит работающий NAS, установят новые counters
после отдельно разрешённого обновления; отсутствие скачков не заявляется.

#### Проверки и измерения

- [x] Baseline barrier FAIL → PASS после изменения. Neighbor write, verification
  publication и последующий same-piece overwrite работают при остановленном Close;
  lifecycle writer lock недоступен. Shared-boundary first Close сохраняет обе
  leases, final Close освобождает обе только после всех writes/accounting.
- [x] Барьеры open/stat/payload/close показывают корректную active stage и
  сохраняют исключение на изменяющих payload/evidence стадиях; polling не меняет
  counters. Prepare и file-guard durable revoke failures видны отдельно и не
  открывают writer/не изменяют payload.
- [x] Errors: open, descriptor stat, payload error/short write, close; completed
  counts/errors/max/total и очистка gauges/leases. Legacy capability opt-out,
  part-files и legacy verification retain close exclusion. Нулевые ns допустимы.
- [x] macOS targeted race `TestWrite|TestCompletionSync|TestVerification|TestResumeMutation`
  ×10 — PASS (3.947 s). Engine/models contract/projection race ×3 — PASS
  (1.222 / 1.036 s). Full `make check` также завершён успешно (см. ниже).
- [x] Native Linux arm64: targeted storage suites ×3, полный storage suite ×1
  и core cached-timings/verification/selective/checkpoint suites ×3 — PASS в Docker,
  CGO=0 (`phase44-write-stages-linux.log`, `phase44-write-stages-core-linux.log`).
- [x] Windows amd64 storage/core cross-compile — PASS; native Windows execution
  **не выполнялся**, platform gate остаётся открытым.
- [x] Полный `make check` — PASS, лог `phase44-write-stages-check.log`: root tests/race,
  fork tests/race, 44 frontend tests/build, vet и application build. Root usecase
  race — 304.718 s; fork race — 8.560 / 2.789 s. Предупреждения macOS linker
  LC_DYSYMTAB не привели к ошибкам. Native Windows и NAS validation остаются открыты.
- [ ] NAS runtime validation: не выполнялась, deployment/config/payload не менялись.

Controlled benchmark `BenchmarkWriteCloseNeighbor`, macOS arm64/local temp FS,
8 B payload, 4 B neighbor, искусственная задержка descriptor Close 20 ms,
`-benchtime=1x -count=3`. Test-only wrapper без capability реконструирует прежнюю
lease scope (не старый бинарник). Neighbor latency:

| Scope | Neighbor latency, µs (3 runs) |
| --- | --- |
| legacy Close scope | 20906 / 22904 / 20305 |
| narrowed final Close | 63.08 / 164.5 / 122.9 |

Общий call всё ещё ждёт Close, lifecycle barrier не сокращён. Это не swarm
throughput benchmark, не оценка настоящего NAS Close и не закрытие F44-01/F44-07.

### Read-only анализ после пользовательского рестарта 17:29 UTC

По запросу «Проанализируй логи после рестарта» выполнены GET logs/health/torrents,
без runtime/config/payload изменений. Логи подтверждают новый запуск
2026-10-27 17:29:04 UTC, restore 116 ms, negative startup skips=5258 pieces,
Resume в 17:29:14. Новые per-stage records присутствуют; точный build ID не получен.
На предыдущем shutdown в 17:28:59 есть `failed to close torrent storage: context
deadline exceeded` — отдельная нерешённая проблема shutdown, не доказанная причина
текущих скачков. После запуска в полученных логах только четыре malformed peer
extended-handshake WARN, без signature conflict/ENOENT flood/storage errors.
Ошибки отдельных peer handshakes не объясняют очередь локальных writers.

31 BT Health snapshot через 2 s, 17:31:38.240–17:32:38.332 UTC (60.09 s):

- Useful reception average 1.883 MiB/s; двухсекундные rates 0.05–10.41 MiB/s,
  displayed 0–14.14 MiB/s, connected peers 31–35. Провалы реального приёма остались;
  UI sampler дополнительно подчёркивает их: в одном интервале useful rate
  7.89 MiB/s при displayed 0.27 MiB/s.
- Payload writer wait active=16–20 во всех snapshots; 7201 completed contention
  events и 1045.64 accumulated wait seconds. Это сумма параллельных ожиданий,
  не wall time. Mean завершившихся waits в слабых интервалах до 1.89 s.
- 7239 completed write-range holds, 61.843 accumulated seconds. WriteOpen:
  7239 calls / 52.109 s, около 84% measured completed hold total; mean 7.20 ms,
  максимум с attachment 1.726 s. На большинстве sampling points активна именно
  стадия open. Значения completed counters могут включать операции, начавшиеся
  до окна, а stages перекрываются с hold; это не disk-utilization estimate.
- WritePayload 8.064 s (max с attachment 1.366 s), descriptor Stat 1.639 s
  (max 1.356 s), Close 0.041 s (mean 5.67 µs, max 182 µs). Prepare/guard/accounting
  суммарно ~0.017 s. Новых stage errors=0. Close в этом окне не bottleneck.
- Completion sync: +53 calls / 24.067 accumulated seconds, errors=0.
  Дисковая конкуренция sync с open/write возможна, но этими counters не доказана.
- Completion persistence error отсутствует, pending/dropped=0 в начале/конце.
  Это отсутствие ошибки в данном запуске/окне, не доказательство её исправления.
- Отдельные GET torrents после sampling: fast partial restore поколения 38,
  restored_bytes=3036676096, verification status=completed, queue/active=0,
  freshly_verified_bytes ~1.79 GB. Отсутствие backlog подтверждено после окна,
  verification queue внутри каждого BT Health snapshot не измерялась.

Вывод: основная измеренная задержка держателя file-wide write lease сейчас
**в rooted open path**, не в descriptor Close или durable revoke. По коду
rootFileIo.open включает relative path checks, application IOGuard, затем
openRootFileObserved с повторными rooted checks и open/fstat. Application guard
`guardPayloadIO → checkVolume` открывает DownloadDir root, читает volume marker
через `payload.CheckVolume`, проверяет device ancestry через FindVolumeRoot.
Эта работа выполняется при каждом open, включая сетевые chunks. Это установленная
I/O amplification по коду и кандидат внутри медленной стадии, но counters пока
не разделяют guard/pathwalk/syscall; объявлять виновным именно marker или fsync
преждевременно. Следующий локальный блок — детерминированное воспроизведение
задержки guard/open и разделение подстадий, затем сокращение лишних повторных
проверок без отключения volume/containment/revoke/signature safety gates.

Сравнение с прежними 30–35 waiters не является controlled before/after: peers,
payload и стартовое состояние различаются. Уменьшение скачков, отмеченное
пользователем, не превращается в доказанный end-to-end throughput gain.
Evidence: временные файлы OpenCode `phase44-restart-health.json`,
`phase44-restart-logs.txt`, `phase44-restart-logs-latest.txt`,
`phase44-restart-samples.jsonl`, `phase44-restart-torrents.json`.

### Root-open follow-up: подстадии и устранение лишних операций

Команда пользователя: «Приступай к поиску точной причины и оптимизации».
Работа локальная, без deploy/restart/config/payload изменений на сервере.

**Установлено по коду и локальным probe-budget tests:** прежний writer open
выполнял `relative` (полный rooted pathwalk), затем application guard, затем
`openRootFileObserved` с повторным полным pathwalk и leaf identity snapshot.
При каждом O_CREATE, включая существующий файл каждого network chunk, также
вызывался MkdirAll. Это повторная metadata-работа под exclusive file payload
lease; данные предыдущего sampling показывают, что именно родительская стадия
open доминирует, но **не доказывают**, какая из её подстадий давала секундные
задержки на NAS. Это ограничение остаётся явным.

#### Изменения

- `storage/file-io-root.go`: начальное преобразование пути теперь только
  lexical/root-relative. Application IOGuard выполняется каждый раз, без TTL и
  cached positive approval. **После** callback остаются полная filesystem
  validation всех компонентов, pre-open leaf stat и post-open descriptor identity/
  regular-file/hardlink checks. Callback-installed symlink отвергается; прежний
  двойной pathwalk заменён одним, а не переносом проверки до callback.
- MkdirAll только для отсутствующего entry; существующий файл не требует
  повторного создания каталогов. Для existing non-exclusive open снимается
  O_CREATE: исчезновение между stat и open приводит к ошибке, а не созданию
  пустого replacement перед обнаружением identity mismatch. O_EXCL enrollment
  semantics сохранены. Context повторно проверяется после задержанного guard.
- `storage/io-timings.go`, `file-client.go`: attachment-local write-only
  `write_open_guard/path/mkdir/descriptor/identity` records. Guard включает
  application volume validation; path — полный walk + pre-open leaf identity;
  descriptor — Root.OpenFile; identity — fstat и проверки открытого файла.
  Reads/syncs не увеличивают writer-open sub-stages. Public/unattached helper
  с nil timing pointer не читает часы. Expected ENOENT при create не stage error.
- Engine/models/TS и projection/contract tests расширены на пять nested records.
  Polling по-прежнему только atomic snapshot. Пять дополнительных fixed records,
  всего 74 atomic int64 = 592 B без padding на attachment, без queues/workers/cache.
- Добавлен `storage/root_open_test.go`; README, CONCURRENCY/STATE и phase plan
  обновлены. Durable revoke, payload/lifecycle leases, hash/snapshot публикация,
  volume markers/device ancestry, ownership, fsync и recovery gates не отключены.

#### Детерминированные тесты и benchmark

- [x] Барьеры каждой open-подстадии: guard/path/mkdir/descriptor/identity видны
  одновременно с parent WriteOpen и lease hold; до завершения writer сохраняет
  исключение; polling не изменяет counters; после завершения gauges очищены.
- [x] Три opening существующего fixture a/b/data: 12 Lstat attempts, ноль
  MkdirAll, три guard calls, open/fstat по три. Missing-file fixture создаёт
  parents только при первом open; повторный open не вызывает mkdir.
- [x] Guard failure, cancellation внутри guard, symlink installation callback,
  outside-root и read-only access, replace/removal между snapshot и actual open.
  Disappeared entry не создаётся заново; O_EXCL не перезаписывает existing file.
- [x] Test-only reconstruction прежнего existing-file пути: a/b/data требует
  7 Lstat attempts + 1 MkdirAll против 4 + 0 теперь, guard остаётся 1 в обоих.
  Это observed logical attempts, не полный kernel syscall count os.Root/guard.
- [x] Полный macOS storage race — PASS (2.566 s); targeted root-open race ×30 —
  PASS (1.545 s, `phase44-root-open-race.log`). Engine/models race ×3 — PASS
  (1.234 / 1.029 s). `git diff --check` — PASS.
- [x] Native Linux arm64 полный storage suite — PASS (`phase44-root-open-linux.log`).
  WARN permission denied — injected storage-guard scenario, suite завершён PASS.
- [x] Native Linux arm64 полный internal/payload suite — PASS
  (`phase44-root-open-payload-linux.log`), включая общий rooted boundary и volume checks.
- [x] Windows amd64 storage/internal-payload cross-compile — PASS.
- [x] Повторный native Windows CI для root-open path/create изменений — PASS
  по сообщению пользователя «Windows CI прошел». Агент независимо CI run не
  проверял; platform gate для этого follow-up закрыт.
- [x] Новый `make check` — PASS (`phase44-root-open-check.log`): root/fork
  tests и race, 44 frontend tests/build, vet и application build. Root usecase
  race — 296.231 s; fork race — 8.697 / 2.924 s. Предупреждения macOS linker
  LC_DYSYMTAB не привели к ошибкам. Native Windows CI впоследствии подтверждён
  пользователем; NAS attribution остаётся открытой.
- [ ] NAS attribution/validation для новых sub-stages: не выполнялась; сервис
  не обновлялся агентом. Фаза и comparative acceptance остаются открыты.

Benchmark `BenchmarkRootWriteOpenExisting`, macOS arm64/local temp FS,
existing 4 B file a/b/data, `-benchtime=200x -count=3`, includes descriptor Close.
Guard — пустой callback, **не настоящий NAS volume guard**. Legacy test-only
helper реконструирует прежний existing-case path без новых counters; optimized
вариант включает новые timing atomics. Это не старый бинарник и не swarm test.

| Path | ns/op ×3 | B/op | allocs/op | Lstat attempts / MkdirAll calls |
| --- | --- | --- | --- | --- |
| legacy reconstruction | 73734 / 71840 / 71970 | 3097 | 64 | 7 / 1 |
| optimized | 47752 / 46316 / 48664 | 1897 | 38 | 4 / 0 |

Локальный hot-path open ускорен примерно на треть, metadata probes/allocations
снижены. Переносить это число на NAS нельзя: настоящий volume guard всё ещё
читает marker и проверяет ancestry. Для точной атрибуции секундных NAS pauses
нужны новые write_open_* deltas; без separately authorized deployment они
недоступны. Кэшировать положительный результат проверки тома ради скорости
не стали, поскольку это ослабило бы обнаружение смены/исчезновения volume.

### Read-only анализ последнего рестарта 18:04 UTC

По запросу пользователя получены GET logs/BT Health/torrents и 31 парный snapshot
health + verification через ~2 s; сервер/config/payload не изменялись. Новый запуск
2026-10-27 18:04:08 UTC, restore ~150 ms, skipped missing=4833 pieces. Resume
запрошен в 18:04:20, HTTP action занял 8.16 s. В 18:04:21:
`observation persistence postponed ... context deadline exceeded`. В полученном
последнем shutdown есть closing engine, но прежнего close-storage deadline WARN
нет; это не доказательство crash/power-loss durability. После запуска также один
malformed extended-handshake WARN, без ENOENT/signature-conflict flood.

В первом snapshot в 18:05:28 hash queue=23; в минутном окне
18:05:39.345–18:06:39.416 UTC queue=0–1, active=0–1. Очередь не остаётся большим
startup backlog, но hashing/sync иногда активны одновременно с download.
Объект verification учитывает и новые полученные pieces; его active не доказывает
исключительно проверку старого payload.

За 60.07 s useful reception average=1.693 MiB/s, интервалы 0.078–11.519 MiB/s,
connected peers=35–39. 6505 write calls; completed writer waits=6498 / 1467.99
accumulated seconds. Wait-active обычно 18–28, один snapshot=0. Новых ошибок
write stages и completion sync нет. Длительности completed операций могут
пересекать границы окна, snapshots approximate, concurrent time складывается.

| Stage | Delta accumulated seconds | Max с attachment, ms |
| --- | --- | --- |
| Write lease hold | 58.199 | 3479.397 |
| WriteOpen (parent) | 45.339 | 2958.443 |
| Application volume guard | 26.944 | 2332.636 |
| Rooted path checks | 9.462 | 1513.680 |
| Descriptor open | 4.902 | 1600.794 |
| Descriptor identity | 4.015 | 1305.049 |
| Mkdir | 0 (0 calls в окне) | 0.049 |
| Payload WriteAt | 9.630 | 1453.680 |
| Descriptor Stat around WriteAt | 3.099 | 1444.293 |
| Descriptor Close | 0.035 | 0.106 |

Volume guard — крупнейшая sub-stage: ~59% measured WriteOpen time (~46% hold
total). Это уже измеренная application guard, а не предположение из старого
aggregate open. По текущему wiring она вызывает guardPayloadIO/checkVolume:
fresh DownloadDir root, volume marker и device ancestry. Какая операция внутри
guard медленная (marker open/read, ancestry, root open или ожидание исполнения
goroutine), данные пока не разделяют. Raw open/identity/stat/payload тоже имеют
секундные maxima: нельзя объявить единственной причиной marker или утверждать
kernel fsync causality. Возможны общая storage latency/конкуренция sync/SQL либо
descheduling; нужны host I/O/CPU evidence и разбор volume-check path.

Completion sync +53 / 21.820 accumulated seconds, errors=0. Completion persistence
ошибка **вернулась**: dropped=3 в первом GET, затем 4; во всём парном sampling
окне 4→4, pending=0. В коде completion.Status ошибка sticky (`lastErr` не
очищается после успеха), поэтому это evidence хотя бы одного failed batch,
а не доказательство непрерывной ошибки или SQL-причины каждого speed drop.
Observation deadline при Resume — дополнительный сигнал persistence latency,
но первичный SQLite error через API намеренно заменён generic message и не
установлен. Durable resume revocation не следует смешивать с disposable hints.

Вывод: устранённые duplicate pathwalk/MkdirAll и descriptor Close больше не
объясняют все провалы. Следующее обоснованное направление — mandatory volume
guard и общая I/O/persistence latency при всё ещё file-wide serialized writes.
Не отключать guard/fsync и не кэшировать положительный volume approval как
«исправление». Ускорение/регрессию относительно предыдущего окна по одному
average не доказываем: разные payload/restore/peer conditions, не controlled test.
Новых runtime изменений в рамках этого анализа не выполнялось.

Evidence в OpenCode temp: `phase44-latest-open-health.json`,
`phase44-latest-open-logs.txt`, `phase44-latest-open-logs-end.txt`,
`phase44-latest-open-torrents.json`, `phase44-latest-open-samples.jsonl`.

### ManagedPayload: реализация новой модели владения активными файлами

#### Контракт и файлы

- `storage/managed-files.go`: attachment-local LRU на **16 retained payload
  descriptors**, включая reserved opening/closing slots. References исключают
  eviction borrowed handles; readonly→write upgrade ждёт возврата ссылок. При
  заполнении все занятые slots ждут cancelable notification, без overflow fd,
  фоновых workers или I/O под cache mutex. Guards/rooted validation — на actual
  acquisition/upgrade, не на каждом chunk. Eviction сначала закрывает старый fd;
  Close errors остаются видимыми, failed I/O retires descriptor. Cap per attachment,
  **не global process fd limit**; temporary metadata/direct-stream handles отдельны.
- `NewFileClientOpts.ManagedPayload` opt-in: writable RootFS+VerifyOnOpen без
  part-files; неподдерживаемая комбинация явно отклоняется. Application backend
  `internal/engine/client_config.go` включает этот режим по умолчанию. Offline
  recovery и default fork tests сохраняют прежний strict mode.
- `file-torrent-io.go`, `file-piece.go`, `file-torrent.go`: writes/ReadAt/hash
  WriteTo/completion sync разделяют retained inode. У writes нет per-chunk
  before/after stat; refreshFile в managed mode не опрашивает namespace. Существующие
  fresh/hashSnapshot/signature maps сохраняются как единственный live trust gate;
  managed publication проверяет retained identity и internal invalidation, не mtime.
  **Полученные pieces по-прежнему хешируются**, cache hit не создаёт readiness.
- Все overlapping writes по-прежнему revoke persisted subset **до mutation**,
  стирают hash snapshot/fresh trust и держат payload/lifecycle leases. Независимые
  writes не аннулируют соседний hash только изменением общей mtime. Managed ReadAt
  требует cached completion даже без resume hooks; WriteTo — explicit hashing path
  для untrusted bytes. Short hash reads и non-cancel errors отзывают file trust.
- Mandatory completion sync не отменён; Windows FlushFileBuffers получает writable
  cached handle, включая upgrade после hashing existing file через readonly fd.
  Checkpoint по-прежнему joins mutation/publication, читает rooted current evidence,
  проверяет retained inode, синхронизирует и сверяет evidence повторно. Expected
  internal writes больше не отбрасывают subset по acquisition mtime. Strict
  PrepareRestore/RestoreCheckpoint metadata/volume validation не ослаблена.
- `storage/interface.go`, `torrent.go`: full VerifyDataContext захватывает
  ResetManagedPayload callback под Client.rLock, вызывает **вне Client.lock**.
  Reset берёт lifecycle exclusive и все payload writer leases, joins readers,
  закрывает live gate, пытается durable revoke и закрывает кеш; failed barrier
  не разрешает hashing и не оставляет live readiness. Drop/detach/shutdown закрывают
  дескрипторы до namespace work. Closed parent client отказывает cache access.
- Pause сохраняет attachment/cache ради streams/seeding; это не обещание release.
  Внешнюю замену делать при остановленном сервисе или detached attachment, после
  внешних изменений — full Recheck. Piece-level VerifyData не сбрасывает весь кеш.
  Активные external edits/replacement/unlink/mount change не поддерживаются и
  могут остаться незамеченными/попасть в reads. Это **явное изменение модели**,
  согласованное пользователем, не прежняя гарантия мгновенного обнаружения.
- BT Health Go/TS DTO и projection/contract tests: `managed_payload`,
  `file_cache_hits/misses/evictions/open`. Все diagnostics cached. WriteOpen/Close
  теперь могут означать borrow/release, не syscalls; WriteStat/Account не выполняют
  managed external-evidence refresh. Root sub-stages показывают реальные writable
  acquisitions, включая Windows sync upgrades. Добавлены 4 atomic counters:
  timing state всего 78 int64 = 624 B без padding + bounded cache metadata.
  В кеше нет payload buffers; hashing использует 32 KiB buffer на bounded worker.
- README, CONCURRENCY/STATE, phase plan и native CI selection обновлены. Storage
  namespace workflows, WAL/FULL, ownership, sticky DiskFull, Range, selection,
  CORS и QoS/refcount/debounce не изменялись. mmap не включён. Сервис не обновлялся.

#### Проверки

- [x] `managed-files_test.go`: 100 writes → один actual descriptor acquisition,
  ни одного before/after WriteStat; общий fd для hash/flush; successful hash grants
  completion, overlapping write отвергает stale result; соседние/shared-boundary
  pieces допускаются. Cached-only readiness polling не делает stat.
- [x] Checkpoint после внутренних writes сохраняет subset; valid checkpoint
  restore/read/write-upgrade, failed durable revoke на cache hit не меняет bytes;
  failed reset closes read gate и сохраняет pending obligation для retry.
- [x] FD bound/cancelable capacity wait, same-file miss coalescing, LRU eviction
  и обязательный guard при reacquisition; Reset joins payload readers; Close
  закрывает retained fd и open gauge; closed parent rejects cached writer.
- [x] Persistent completion после readonly hashing cache (Windows write-access
  upgrade scenario); core `TestManagedPayloadExplicitRecheck` v1/v2/hybrid реально
  хеширует данные, затем обнаруживает external corruption и reacquires handles.
- [x] Engine/progress/direct-source tests адаптированы к новой модели: не требуем
  implicit stat на polling, сохраняем direct-open size/type/security gates и
  проверяем Recheck. `TestProgressSyncCannotRestoreBytesAfterManagedRecheck`
  по-прежнему доказывает, что stale DB snapshot не возвращает revoked progress.
- [x] Targeted core/storage race ×10 — PASS (2.242 / 2.050 s,
  `phase44-managed-targeted-race.log`); engine/models/usecase projection/Recheck
  race ×3 — PASS (1.363 / 1.029 / 2.740 s). Позднее полный fork race — PASS
  (8.354 / 2.650 s, `phase44-managed-fork-race.log`).
- [x] Native Linux arm64 полный storage suite и targeted core selective/hash/
  checkpoint/managed/Recheck suites ×3 — PASS (`phase44-managed-*-linux.log`).
  Permission denied/failed commit/context canceled logs — injected scenarios,
  suites завершены PASS. Windows core/storage cross-compile — PASS.
- [ ] Новый native Windows CI для descriptor lifetime/reset/Flush upgrade — нужен;
  предыдущие Windows PASS остаются историческими, это новый runtime revision.
- [x] Второй `make check` — PASS (`phase44-managed-check-final.log`): root/fork
  tests/race, frontend, vet/build; usecase race 299.108 s, fork 8.771 / 3.017 s.
  Первый прогон остановился на старом тесте, требовавшем external-change detection
  от polling; тест обновлён на explicit Recheck, проверка stale DB fencing сохранена.
- [x] Повторный `make check` после EOF retirement — PASS
  (`phase44-managed-check-latest.log`): usecase race 301.416 s, fork 8.823 / 3.070 s,
  frontend/vet/build. Capacity-wait test дополнительно усилен наблюдаемым waiter
  barrier (не просто cancellation до admission).
- [x] Финальная safety-доработка: known IOGuard failure на acquisition/checkpoint
  latches cache error, clears live trust/hash snapshots и блокирует future hits до
  detach/reopen. Нет обещания мгновенного external detection, но уже известная
  unsafe volume не игнорируется. Новый barrier test покрывает обе точки. Targeted
  core/storage/checkpoint race ×10 — PASS (`phase44-managed-final-guard-race.log`).
  Linux full storage / targeted core ×3 и Windows cross-compile пересобраны после
  этой доработки, PASS. Runtime после неё больше не менялся.
- [x] Дополнительный usecase/engine safety race с package budget 600 s — PASS
  (198.715 / 1.211 s, `phase44-managed-final-safety-race-long.log`). Исходный 180 s
  budget истёк на сумме длинных storage/recovery suites; assertion/race failure
  до timeout не было. Это не исправление deadlock и не замена failed assertion.
- [x] Guard-fence `make check` обнаружил API regression: после GET symlink conflict
  повторный HEAD получал generic 500 от сохранённой volume error. Известные
  payload.ErrVolumeUnavailable/Unregistered теперь явно маппятся в HTTP 409,
  unexpected I/O errors остаются 500. HEAD/download/ZIP safety race ×10 — PASS
  (53.318 s, `phase44-managed-api-safety-race.log`), включая repeated requests.
  Новые API tests добавлены в native CI selection. Storage runtime после guard
  fence не менялся; это уточнение error mapping, не отключение guard.
- [x] Core Recheck test усилен actual replacement inode при retained original fd,
  а не только in-place corruption: v1/v2/hybrid race ×10 — PASS (1.437 s,
  `phase44-managed-replacement-race.log`).
- [x] Финальный `make check` после API mapping — PASS
  (`phase44-managed-acceptance-check.log`): root/fork tests и race, 44 frontend
  tests/build, vet и application build. Usecase race 293.867 s; fork race
  8.727 / 2.927 s. Production source после старта не менялся. Предупреждения
  macOS linker LC_DYSYMTAB не привели к ошибкам. Новый native Windows CI и
  проверка поведения/скорости на NAS остаются открыты.
- [ ] NAS deployment/throughput validation не выполнялись. Исходная SQLite
  persistence error не объявляется исправленной этим изменением.

#### Контролируемый hot-path benchmark

`BenchmarkManagedPayloadChunkWrite`, macOS arm64/temp local FS, cached warm-up,
перезапись одного 16 KiB chunk, `-benchtime=200x -count=3`. Сравниваются **оба
реальных** режима текущего backend, без reconstruction старого кода. Volume guard
— пустой callback, а не NAS marker/device checks. Нет seeder/stream/hash/sync в
timed loop; высокая MB/s — скорость overwrite page cache, не download throughput.

| Mode | ns/op ×3 | Guards/chunk | Descriptor stats/chunk | B/op / allocs |
| --- | --- | --- | --- | --- |
| strict | 42490 / 38074 / 38659 | 1 | 2 | 2512 / 46 |
| managed | 2528 / 2754 / 2279 | 0 | 0 | 624 / 19 |

Hot-path setup cost существенно меньше; обязательные hashing/sync намеренно не
сравнивались как отключённые. На NAS эффект и остаточные write/fsync/SQL pauses
следует измерять отдельно после авторизованного обновления. F44-01/F44-07 остаются
частично непринятыми до всех comparative/native/runtime gates.
