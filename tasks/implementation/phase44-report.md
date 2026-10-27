# Фаза 44 — отчёт об имплементации

Статус: **основной runtime реализован; финальная приёмка/measurements не завершены**. Основание: [план](../phase44-selective-verification-and-download-concurrency.md). Пользовательский сервер не изменяется; все fixtures — временные.

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
