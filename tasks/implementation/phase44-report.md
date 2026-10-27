# Фаза 44 — отчёт об имплементации

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
- [x] Windows amd64 storage/internal-payload cross-compile — PASS. Это новые rooted path/create
  изменения: **нужен повторный native Windows CI**, прежнее подтверждение
  пользователя принято для предыдущего writer-path revision и не отменяется.
- [ ] Новый `make check` выполняется, лог `phase44-root-open-check.log`.
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
