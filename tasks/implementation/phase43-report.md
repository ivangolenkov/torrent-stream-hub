# Фаза 43 — отчёт об имплементации

Статус: **реализация и macOS/Linux приёмка блоков 1–6 выполнены; остаётся только native Windows gate**. Public constructor использует restart-scoped configuration с **default fast**. REST/SSE/GUI diagnosis, upgrade/strict/re-enable, расширенные subprocess crash/fault tests и WAL transaction/online-seal/actual Linux fsync measurements завершены. По последней команде пользователя Windows проверка исключена, результат ожидается отдельно. Вся фаза до него не объявляется завершённой; исторические разделы сохраняют состояние на момент их выполнения.

Основание: [план](../phase43-trusted-fast-resume.md), [новый контракт](../phase43-fast-resume-contract.md). Все файлы/download fixtures — временные; пользовательский NAS/downloads не использовались.

## Блок 1 — что сделано

- Изучены hints (`internal/completion/sqlite.go`), engine open/verify и rooted fork storage, completion/signature gates, v2 layers, explicit recheck, streaming/direct/archive и projection. Integration checklist закреплён в контракте, включая namespace mutations, которые выполняются уже внутри `OpenTorrent` до возврата backend.
- Создан `tasks/phase43-fast-resume-contract.md`: default fast/strict verify, checkpoint identity/geometry, owner-scoped generation/revocation epoch, atomic replacement/CAS, durable revoke-before-write, periodic quiesce/flush/seal, validate/attach fencing, типизированные причины, additive read model и численные budgets.
- Создан `internal/fastresume/policy.go`: bounded schema, mode parser, checkpoint validation, pure candidate filter. Filter только сужает saved bitmap; не импортирует hints, не делает I/O, не предоставляет restore token и не разрешает runtime readiness. Dirty session не является входом фильтра, поэтому crash сам по себе не аннулирует snapshot.
- `internal/fastresume/policy_test.go`: default/unknown config values, schema/identity/geometry/bitmap validation, unsafe/colliding/overlapping paths, budgets, immutable output, v1 shared-boundary invalidation, v2 gaps, zero-length, missing/truncated/replaced files, stale identity, revocations, MaxInt64 overflow и fuzz «никогда не добавлять trust».
- `internal/usecase/verification_measurement_test.go`: добавлен `BenchmarkFastResumeBaseline/complete|mixed`. Первая сессия действительно проверяет данные с SQLite completion, затем закрывает engine/SQLite и открывает заново. Mixed — два полных и два 50%-valid торрента; читается корректный tail range частичного торрента. В отличие от прежнего benchmark ожидание заканчивается по завершённым hash attempts, а не по полному скачиванию, поэтому неполный torrent не даёт вечного ожидания.
- Измерения дополнены engine shutdown latency и sampled Go heap. Семантика прежнего benchmark сохранена как full-library сценарий; новые варианты отдельно обозначают preverified/DB-reopen baseline.

## Baseline измерений

Среда: macOS arm64, Apple M3 Max, Go 1.25.8, local temporary filesystem. Четыре файла по 2 MiB, piece 64 KiB, сеть/swarm отключены. Сохранена synthetic задержка 1 ms на payload read, чтобы Range конкурировал с hashing backlog; это **не скорость NAS/HDD**. Setup первой сессии исключён из отдельных latency/counter metrics, но включён в стандартный `ns/op`; последний для сравнения не используется.

```sh
go test ./internal/usecase -run '^$' -bench '^BenchmarkFastResumeBaseline$' -benchtime=1x -count=3 -timeout=120s
```

| Fixture/прогон | API ms | Первый tail Range ms | Hash pass ms | Payload reads B | Payload opens/stats | SQL row writes | Allocated B | Sampled heap B | Engine close ms |
| --- | ---: | ---: | ---: | ---: | --- | ---: | ---: | ---: | ---: |
| complete/1 | 2.945 | 26.08 | 440.8 | 8 388 624 | 261/3156 | 160 | 22 269 264 | 9 864 616 | 0.133 |
| complete/2 | 3.448 | 28.68 | 480.2 | 8 388 624 | 261/3156 | 160 | 22 176 472 | 10 031 024 | 0.140 |
| complete/3 | 5.471 | 39.46 | 510.8 | 8 388 624 | 261/3156 | 160 | 22 202 944 | 9 771 600 | 0.155 |
| mixed/1 | 4.439 | 29.43 | 393.0 | 8 388 624 | 227/2748 | 160 | 21 829 888 | 9 398 912 | 0.224 |
| mixed/2 | 2.574 | 21.56 | 375.7 | 8 388 624 | 227/2748 | 160 | 21 831 200 | 9 228 608 | 0.151 |
| mixed/3 | 4.984 | 29.76 | 386.1 | 8 388 624 | 227/2748 | 160 | 21 810 176 | 9 383 480 | 0.183 |

Во всех прогонах: resource opens/stats 128/512, peak hash tasks на этой ФС 1, pending hash leases 1, одновременные payload callbacks 2 (reader + hasher). Payload bytes — 8 MiB полного hashing плюс 16 bytes фактического Range. `SQL-row-writes` — изменённые строки выбранных storage/intent/catalog/completion таблиц, **не transactions/fsync**. Sampled heap — абсолютный Go HeapAlloc процесса с шагом 5 ms, не RSS и не доказанный instantaneous peak; allocated — TotalAlloc между началом reopen и engine close. Engine close не включает закрытие SQLite. Сравнение до/после должно использовать тот же sampler и fixture.

Baseline показывает стоимость прежней политики после уже выполненной проверки и штатного shutdown, но не является результатом реализации fast restore. Уменьшение прочитанных bytes/latency пока не заявляется. Дополнительно прежний `BenchmarkVerificationStartup -count=3` прошёл до изменений benchmark; для дальнейшего сравнения выбран новый complete/mixed harness.

Логи (вспомогательные, содержательные результаты сохранены здесь):

- `/private/var/folders/zg/l0438jl160s4wr89pw16vh4c0000gn/T/opencode/phase43-baseline-check.log`
- `/private/var/folders/zg/l0438jl160s4wr89pw16vh4c0000gn/T/opencode/phase43-baseline-verification.log`
- `/private/var/folders/zg/l0438jl160s4wr89pw16vh4c0000gn/T/opencode/phase43-baseline-clean-reopen.log`

## Проверки блока 1

- [x] Начальный `make check`: root/fork tests и race/vet, frontend 40 tests и build, application build — exit 0. Это baseline до подключения нового пакета.
- [x] `go test ./internal/fastresume` и `go test -race ./internal/fastresume -count=1`.
- [x] Policy unit coverage 97.9%; после добавления guards NUL/depth и отрицательного piece count повторный race/vet прошёл. Windows amd64 / CGO=0 test binary собран (не native execution).
- [x] Fuzz `FuzzFilterNeverAddsTrust`, 5 s: начальный прогон 1 441 153 executions; после последних guards повторный 1 440 169 executions, PASS.
- [x] Complete/mixed benchmark, три прогона каждого сценария.
- [x] Complete/mixed benchmark дополнительно прошёл с `-race -count=1`; после добавления проверок ожидаемого verified subset первой/второй сессии повторные обычный и race `-count=1` также прошли. Дополнительные прогоны не включены в таблицу baseline выше.
- [x] Финальный `make check` блока 1: exit 0, root/fork tests/race/vet, 40 frontend tests, frontend/application build. Root usecase race — 187.898 s; fork race — 8.058/2.045 s. Последние guards pure policy дополнительно прошли targeted race/vet/fuzz и Windows cross-compile. Лог: `/private/var/folders/zg/l0438jl160s4wr89pw16vh4c0000gn/T/opencode/phase43-block1-check.log`.

## Блок 2 — durable state и instance ownership

- `internal/repository/migrations/0020_resume_checkpoints.sql`: отдельные `resume_owners`/`resume_checkpoints`, без импорта `piece_completion`. Head сохраняет record/mapping, монотонные epoch/generation и invalidated tombstone. CHECK limits/types и defensive triggers отзывают trust при mapping/alias change, deletion intent и record deletion/change. Старый worker не может воскресить checkpoint re-add.
- `internal/repository/resume.go`: Capture/Load/Commit/Revoke/Invalidate с `BEGIN IMMEDIATE`, coherent record/mapping/alias/journal checks и exact CAS token. Новая generation и сброс её отзывов коммитятся вместе; поздний seal отвергается после изменения epoch. Повтор уже отозванных bits не добавляет UPDATE; load не потребляет checkpoint. Невалидный blob не мешает whole-owner invalidation. Это DB protocol, **не** runtime restore token и **не** подключение write hooks.
- `internal/fastresume/codec.go`: bounded architecture-independent binary envelope; counts, prefixes и total bytes проверяются до decode allocations. Отдельная SHA-256 integrity связывает payload/revocations и весь head fence. SQL CASE не возвращает oversized blob в Go; две глобальные working slots берутся до SQL и отменяемо ограничивают одновременные materialization operations. Snapshot decode read path выполняется после commit read transaction. Golden fixture независимо упакована и закрепляет 442-byte schema-1 envelope.
- `internal/repository/sqlite.go`: explicit FULL synchronous на каждом new/replacement connection; safety operations дополнительно проверяют WAL/FULL на своём соединении. Тест DSN `synchronous(OFF)` выявил сортировку pragmas драйвером: простое добавление FULL в конец DSN не работает. Исправлено private `tsh-sqlite` driver hook **после** DSN setup, без глобального hook на прочих пользователей `sqlite`.
- `internal/payload/instance_lock*.go`, `internal/engine/engine.go`: rooted `.tsh-instance.lock`, nonblocking Unix flock / Windows LockFileEx. Engine получает его до client/storage, освобождает после joins/close или при init failure. Файл не удаляется и не заменяется, существование файла не считается lock. Links и смена identity при acquisition отклоняются; root aliases не обходят lock. Unsupported locking не допускает startup.
- `internal/repository/resume_test.go`: WAL reopen, repeated load/revoke, stale seal/revoke, concurrent CAS, current record/alias/mapping/deletion/journal fences, corruption/oversized envelope, pooled connection tampering/replacement и DSN override, bounded working-set cancellation, epoch overflow. Проверены rollback после частичного UPDATE и настоящий **COMMIT failure** через deferred FK violation; дальнейшее соединение остаётся рабочим.
- Subprocess tests убивают ребёнка до/после revocation, после payload write и после committed seal; отдельные OS-lock subprocess tests проверяют orderly close/kill. Payload write выполняется только после successful DB barrier, kill не аннулирует нетронутую generation. Это process-crash evidence, не эмуляция потери питания.
- `internal/engine/instance_lock_test.go`: второй engine отвергается во время shutdown с ещё не joined задачей, после завершения storage close root снова доступен; init failure не оставляет lock.
- `README.md`, `STATE.md`, `CONCURRENCY.md` и контракт обновлены; `.github/workflows/checks.yml` включает новые repository/engine/codec tests в native Windows/macOS jobs. Native payload suite уже содержит OS-lock tests. Это подготовка CI, не успешное Windows execution.

### Проверки и измерения блока 2

- [x] `go test ./...` прошёл после первоначальных fixture corrections.
- [x] Targeted final race: repository 12.370 s, policy/codec 1.025 s, engine 1.036 s, payload 2.047 s. Команда: `go test -race ./internal/repository ./internal/fastresume ./internal/engine ./internal/payload -run 'TestResume|TestCodec|TestInstanceLock|TestEngineInstance' -count=1 -timeout=90s`.
- [x] После исправления cancellation в драйвере: SQLite cancellation + resume race suite прошёл три раза за 42.397 s (`TestPendingStorageOperationsCancellation|TestSQLite.*Cancel|TestResume`); всего 6000 конкурентных отмен с обязательным успешным запросом после каждой. Дополнительный upgrade-тест без импорта hints прошёл с race трижды.
- [x] `FuzzDecodeCheckpoint`, `-fuzztime=5s`: 1 635 459 executions, PASS. Package policy+codec unit coverage — 95.0% (исторические 97.9% блока 1 относились только к policy до добавления codec).
- [x] Native Linux arm64: payload OS-lock, engine ownership lifetime/init-failure и repository resume/crash suites прошли по три раза. Собраны статические test binaries **Go 1.25.8 / CGO=0**, исполнены локальным Docker Linux kernel в существующем image `golang:1.26-alpine`, `--network none`, 2 CPU/1 GiB на контейнер, read-only mount только бинарника. Test files/SQLite находились в native container FS. Это не Linux race и не NAS/SMB/NFS acceptance.
- [x] После обновления SQLite до 1.42.2 native Linux repository/cancellation и engine ownership suites повторно прошли по три раза на пересобранных бинарниках. Репозиторный suite включает фазу-42→43 upgrade: hints, paused intent, record и mapping сохранены, checkpoint authority автоматически не создаётся.
- [x] Windows amd64 / CGO=0: payload, repository и engine test binaries собраны. Native execution ожидается в расширенном CI; gate всей фазы остаётся открытым.
- [x] `go vet ./...`, `git diff --check`; startup fixture повторён 20 раз после уточнения snapshot-conflict retry.
- [x] Финальный `make check` с SQLite 1.42.2: exit 0, root/fork tests/race/vet, 40 frontend tests, frontend/application build. Repository race — 43.912 s, usecase race — 189.576 s, fork race — 8.116/2.048 s. Последний added upgrade fixture дополнительно прошёл targeted race ×3 и native Linux ×3. Лог: `phase43-block2-check-1422.log`.

`BenchmarkResumeDurableBarriers -benchtime=20x -count=3`, macOS arm64 / Go 1.25.8 / local temporary FS: первоначально 550 219 / 480 731 / 373 721 ns/op; после обновления SQLite до 1.42.2 — **454 690 / 327 471 / 314 506 ns/op**, **2 FULL/WAL commits на op**, 14 936–14 944 B/op, 379 allocs/op. Один op — revoke + replacement checkpoint. Это цена небольшого repository snapshot и двух durability transactions, **без payload flush/hash**, не end-to-end startup/streaming/NAS performance. Межпрогонный разброс не доказывает ускорение драйвера. Новые resume tables не подключены к runtime producer, ускорение старта по этим результатам не заявляется.

Первый `make check` блока 2 поймал существующий startup test race: после `<-ready` новый runtime revision может вызвать штатный `409 snapshot changed`, а тест требовал немедленного `200`. До исправления targeted suite прошёл 20 повторов, но один полный concurrent run получил 409. `cmd/hub/startup_test.go` теперь ограниченно повторяет **только** этот conflict при корректном CORS и всё ещё требует конечный 200; 503/500 и неверный CORS не игнорируются. Production snapshot gates и поведение API не менялись.

Ещё две fixture corrections: `deletion_test.go` удаляет новый later-version trigger только в своей временной pre-upgrade fixture перед DROP COLUMN; прежние assertions сохранности manifest не ослаблены. Init-failure fixture занимает wildcard IPv4 port, а не localhost: libtorrent мог законно открыть другой локальный интерфейс при занятом только loopback.

### Исправление upstream cancellation race, найденного полным check

Второй полный check с SQLite 1.40.1 упал в **неотменённом следующем** запросе `TestPendingStorageOperationsCancellation`: `interrupted (9)`, итерация 226. Это не допустимый результат отменённого запроса и не повод ослаблять тест. Исходник `interruptOnDone` 1.40.1 допускает TOCTOU: cancellation goroutine уже увеличила atomic done, но ещё не вызвала `interrupt`; defer исходного запроса возвращается и соединение начинает следующий запрос, который получает запоздалый interrupt.

Проверены upstream tags: 1.42.1 ещё содержит старую реализацию, **1.42.2** синхронизирует проверку done/interrupt и deferred completion одним `donemu`. [Исходник исправления](https://gitlab.com/cznic/sqlite/-/blob/v1.42.2/sqlite.go). `go.mod`/`go.sum` обновлены **только** с SQLite 1.40.1 до 1.42.2; версии libc/прочих зависимостей и Go не менялись (драйвер требует Go 1.24, проект использует 1.25.8). Никаких `recover`, retries здорового SQL-запроса или отключения cancellation не добавлено. Это необходимое исправление safety dependency, обнаруженное проверкой блока 2, а не несвязанное обновление стека. Предыдущие успешные прогоны на 1.40.1 не объявляются доказательством отсутствия этой гонки.

Финальные логи блока 2 находятся в `/private/var/folders/zg/l0438jl160s4wr89pw16vh4c0000gn/T/opencode/`: `phase43-block2-race-final.log`, `phase43-block2-linux-payload.log`, `phase43-block2-linux-engine-1422.log`, `phase43-block2-linux-repository-1422.log`, `phase43-sqlite-1422-race.log`, `phase43-startup-retry-repeat.log`, `phase43-block2-check-1422.log`. Неуспешные полные checks: `phase43-block2-check.log` (startup snapshot conflict), `phase43-block2-check-final.log` (upstream cancellation race). Runtime tokens, write/flush hooks и trusted import намеренно оставлены следующим блокам.

## Блок 3 — quiesce/flush/restore primitives (частичный результат)

- Fork `checkpoint.go`: `Torrent.WithFileCheckpoint`, один global capture slot, per-torrent gate, joins active hashes/layer updates/network writes; eligibility исключает dirty/pending/непроверенные pieces. Callback выполняется вне `Client.lock`; unrelated Add и read-only reader продолжают работу. После callback проверяется закрытие torrent, чтобы Drop race не сообщал об успешном seal.
- Fork `torrent.go`, `piece.go`, `peer.go`: checkpoint admission учитывается scheduler/network writes/v2 layer updates; Drop/Close ждут storage tasks. Cached completion не заменяет обязательные v2 hashes/layers; forced-verification policy сохранена.
- Fork `storage/checkpoint.go` и file backend: lifecycle-exclusive capture, payload и namespace flush, повторная rooted evidence проверка перед publish. Ошибки flush/evidence/cancellation не вызывают publish. Чтения не требуют exclusive lifecycle lock. Бюджеты проверяются до выделения больших masks/catalogs.
- `storage.RestoreToken`: opaque, one-use, привязан к backend open/context/epoch; rooted reprobe и финальный owner callback предшествуют изменению freshness. Пока нет mutation hooks блока 4, trusted import ограничен read-only backend; writable restore отвергается. Restore не создаёт отсутствующие zero-length files.
- `internal/fastresume/storage.go`: canonical catalog digest с exact info bytes, ordered physical layout/geometry и sorted validated required v2 layers; `FromStorage`/`StorageClaims` связывают native evidence с record/mapping/volume и durable revoked subset. V2 layers ограничены 4 MiB, piece length — 64 MiB для bounded Merkle scratch.
- `internal/fastresume/storage_test.go`: immutable bitmaps, mapping/volume/metainfo/catalog/hash/geometry/path mismatch, отсутствующие/повреждённые layers, malformed revocations и budgets. Golden catalog digest независимо рассчитан Python `hashlib`/`struct.pack('<q', n)` и закрепляет domain/порядок/byte order/length prefixes. Итоговое покрытие всего fastresume пакета — 93.8% statements.
- `internal/repository/resume_storage_test.go`: реальные payload files + SQLite close/reopen для v1/v2/hybrid; capture/conversion/commit, отменённая публикация сохраняет предыдущую generation, durable revoke исключает piece, restore без payload reads, реальное чтение trusted bytes и отказ позднего token после durable invalidation. Backend fixture читает bytes и вручную отмечает результат проверки; реальный library hashing и zero-hash restore отдельно покрыты fork core tests.
- Fork tests: complete и partial v1/v2/hybrid, reader trusted range при заблокированном hasher остатка, missing v2 layers, explicit ignore-completion, reader/unrelated Add во время publish, Drop join; backend tests покрывают flush errors, cancellation, evidence changes, stale/replayed token, boundaries и missing zero file. Writer syscall barrier доказывает удержание lifecycle fence и отсутствие publish до join; отрицательная проверка через мгновенный `select default` заменена проверкой фактического lock ownership.
- `.github/workflows/checks.yml`: native macOS/Windows gates дополнены core checkpoint и conversion tests. Это подготовка CI, не подтверждение native Windows execution.

### Проверки primitives блока 3

- [x] `go test ./internal/fastresume ./internal/repository -count=1 -timeout=120s`.
- [x] `go test -race ./internal/fastresume ./internal/repository -run 'TestStorageConversion|TestResumeStorageRoundTrip' -count=3 -timeout=90s`.
- [x] В fork: финальные `go test -race . ./storage -run '^(TestFileCheckpoint|TestCheckpoint)' -count=20 -timeout=90s` после расширения partial fixtures до v1/v2/hybrid и финальных fencing corrections — PASS.
- [x] Linux arm64 / CGO=0: test binaries repository/fastresume/fork core/storage собраны Go 1.25.8 и исполнены в локальном Docker `golang:1.26-alpine`, `--network none`, 2 CPU/1 GiB, read-only mount бинарника. Repository/fastresume tests прошли ×3; после финальных fencing corrections fork core/storage пересобраны и прошли ×20. Файлы и БД — на native container FS. Это не Linux race и не distributed FS acceptance.
- [x] Windows amd64 / CGO=0: четыре test binaries собраны; native execution остаётся открытым gate.
- [x] Финальный `make check` primitives: exit 0, root/fork tests/race/vet, 40 frontend tests, frontend/application build. Repository race — 43.820 s, usecase race — 193.081 s. После последних fencing corrections отдельно повторены полные `make test-fork race-fork` (core race 8.437 s, storage race 2.625 s), fork vet, targeted root race ×3, Windows cross-compile и native Linux fork ×20; `git diff --check` — PASS.

Исправления fixtures: v2 metainfo кодируется через `bencode.Marshal(&info)`, а не value; integration physical path согласован с persisted layout без несуществующего prefix stripping. При расширении partial hybrid тест искусственно блокировал v1 hash до завершения установки v2 layers в Add, которая законно ждёт storage lock. Теперь scheduler запускается после завершения установки layers, и barrier блокирует только hashing untrusted remainder. Production поведение ради fixture не менялось. Первый полный check прошёл root tests, но был остановлен на старой заблокированной fork fixture; запущен новый полный check. macOS race linker выдаёт прежний `malformed LC_DYSYMTAB` warning, targeted suites завершаются успешно.

Дополнительный race-прогон обнаружил более узкую Drop гонку: `closed.Done()` уже закрыт, но `closedCtx` ещё не отменён. Проверка после publish теперь читает authoritative `getClosedErr()` под `Client.lock`, а не полагается на порядок cancellation notifications. Финальные ×20 race/Linux прогоны прошли. Restore повторяет owner callback **после** потенциально блокирующего rooted reprobe непосредственно перед импортом; fault test отзывает owner во время reprobe и проверяет отсутствие частичного trust. Attachment/mutation ownership адаптер всё равно обязан удерживать через callback/import; дополнительный callback не заменяет этот fence.

Логи/бинарники: `/private/var/folders/zg/l0438jl160s4wr89pw16vh4c0000gn/T/opencode/phase43-block3-*`; native Linux логи — `phase43-block3-linux-{fastresume,repository,core,storage}.log`, финальный full check — `phase43-block3-primitives-check-final.log`.

**Открыто в блоке 3 после primitives:** application producer/adapter, engine shutdown ordering и end-to-end crash/restore через production gates. Bounded worker реализован ниже, но к runtime ещё не подключён. До безопасного подключения mutations блока 4 production trusted import не включается. Успешные primitives tests не закрывают F43-03.

### Bounded periodic/shutdown worker

- `internal/fastresume/worker.go`: `SealWorker`, 60-second ticker, одна worker goroutine, FIFO queue/coalescing до 64 владельцев с учётом активного callback. Callback выполняется без queue mutex. Новая dirty notification во время seal требует следующей попытки; failure остаётся в очереди до следующего tick. В одном batch каждый владелец обрабатывается не более одного раза.
- `MarkDirty` возвращает false на saturation/закрытом admission/невалидном ID. Producer обязан сохранять dirty state и повторять enqueue при bounded inventory scan; это lossy optimization admission, **не** очередь safety-critical revocations. Потеря места в seal queue не разрешает пропуск durable revoke.
- Первый `Shutdown(ctx)` останавливает admission, задаёт независимый drain budget и делает один проход очереди. Следующий caller не заменяет context первого. Deadline/cancellation отменяет active callback, но не освобождает его ресурсы: `Done` и `Wait` наступают только после callback return. Drain errors агрегируются; неуспешный seal не выдаётся за успех.
- `internal/fastresume/worker_test.go`: FIFO, coalescing, redirty во время active seal, concurrent producers, saturation и retry, включение active owner в bound, periodic failure retry, bounded drain failure, остановленный runtime, expired deadline без начала I/O, blocked callback и повторные shutdown callers. Вместо ожидания 60 s тесты используют controlled tick channel и barriers.
- `internal/repository/resume_storage_test.go` теперь публикует первый checkpoint через настоящий worker shutdown drain до закрытия SQLite/payload. Close/reopen, durable revoke, failed publication и stale restore проверки сохранены для v1/v2/hybrid.
- Native macOS/Windows CI selector дополнен `TestSealWorker`. Настройка engine/config и production producer не менялись.

Проверки worker:

- [x] `go test -race ./internal/fastresume ./internal/repository -run 'TestSealWorker|TestResumeStorageRoundTrip' -count=30 -timeout=120s` — PASS; repository 43.577 s.
- [x] После добавления expired-deadline test: `go test -race ./internal/fastresume -run '^TestSealWorker' -count=100 -timeout=90s` — PASS.
- [x] Native Linux arm64, Docker local FS/2 CPU/1 GiB/`--network none`: worker и repository round-trip ×30 — PASS; после добавления expired-deadline test worker пересобран и прошёл ×100. Go 1.25.8/CGO=0 static binaries; это не Linux race и не production lifecycle acceptance.
- [x] Windows amd64/CGO=0: fastresume/repository binaries собраны; native execution не заявляется.
- [x] `go vet ./internal/fastresume ./internal/repository`, `git diff --check` — PASS.
- [x] Unit coverage: worker functions — 100.0% statements, весь fastresume package — 95.0%. Это не замена end-to-end lifecycle/crash acceptance.
- [x] Новый `make check` с worker — exit 0: root/fork tests/race/vet, 40 frontend tests, frontend/application build. Repository race — 42.322 s, usecase race — 188.767 s, fork core/storage race — 8.523/2.510 s. Последний expired-deadline test дополнительно прошёл targeted race и native Linux ×100.

Логи/бинарники: `phase43-worker-*` в `/private/var/folders/zg/l0438jl160s4wr89pw16vh4c0000gn/T/opencode/`; full check — `phase43-worker-check.log`.

Runtime integration намеренно не включена: существующий `Engine.BeginShutdown` отменяет runtime, а coordinator shutdown запрещает новые command reservations. Production adapter должен получить/удержать существующее ownership и закончить worker join **до** закрытия SQLite/storage/instance lock, не обходя coordinator независимым context. Кроме lifecycle ordering, безопасное использование persisted checkpoint требует mutation hooks блока 4; standalone worker не закрывает эти DoD.

### Engine storage-finalizer lifecycle

- `internal/engine/storage_finalizer.go`: интерфейс `StorageFinalizer` (`Shutdown` + обязательный `Wait`) и одна регистрационная slot через `RegisterStorageFinalizer`. Ownership передаётся только при true; duplicate/nil/registration после BeginShutdown отвергаются. Регистрация сериализована с cancellation существующим `storageTaskMu`, без отдельной mutation lock системы.
- `Engine.Close` после cancellation/quiesce managed state/streams запрашивает finalizer Shutdown с независимым 10-second budget вне engine locks. Этот запрос выполняется параллельно с joins engine tasks: coordinator cleanup может ждать reservation, которую держит active seal, поэтому нельзя сначала ждать все tasks и лишь потом уведомлять worker об остановке. После ordinary tasks + Shutdown обязательно вызывается Wait, даже при drain failure/deadline. Client/backend close и release instance lock происходят только после него.
- `internal/engine/storage_finalizer_test.go`: настоящий SealWorker при Engine.Close получает живой shutdown context после отмены runtime; rejected registration сохраняет caller ownership; synthetic deadline/error не освобождает instance lock/client до Wait. Проверены отсутствие engine locks внутри callback, ordering stop-before-coordinator-style-task-join, concurrent registration/shutdown и repeated Close без double finalization.
- `CONCURRENCY.md`, phase43 contract/plan и native macOS/Windows CI selector обновлены. Сам finalizer по умолчанию не установлен: production producer/adapter и coordinator drain reservations пока отсутствуют, runtime trusted restore не включён.

Проверки finalizer:

- [x] После окончательного stop-before-join ordering: `go test -race ./internal/engine -run 'TestStorageFinalizer|TestEngineInstance' -count=50 -timeout=120s` — PASS (в каждом repetition дополнительно 20 registration/shutdown races).
- [x] После окончательного ordering: Windows amd64/CGO=0 cross-compile и native Linux arm64 ×50 — PASS; в каждом repetition дополнительно 20 registration/shutdown races. Go 1.25.8 static binary в Docker local FS, 2 CPU/1 GiB/`--network none`; это не Linux race и не native Windows execution.
- [x] Финальный `make check` — exit 0: root/fork tests/race/vet, 40 frontend tests, frontend/application build. Engine race — 6.442 s, repository race — 43.634 s, usecase race — 190.242 s, fork core/storage race — 8.387/2.452 s. Лог `phase43-finalizer-check-final.log`; первоначальный full check также завершился успешно, финальный повторен после исправления stop-before-join ordering.
- [x] `go vet ./internal/engine`, `git diff --check` — PASS.

Логи: `phase43-finalizer-*` в `/private/var/folders/zg/l0438jl160s4wr89pw16vh4c0000gn/T/opencode/`. Lifecycle port — частичный результат F43-03, не доказательство production shutdown seal: независимый context не разрешает acquire через уже отменённый coordinator.

### Repository adapter, coordinator drain и real library crash/reopen

- `internal/repository/resume_adapter.go`: `ResumeSource` с canonical immutable metadata/identity, attachment authorization и capture capability. `SealResume` берёт CAS token до quiesce, повторяет authorization после flush и публикует из capture callback. `RestoreResume` связывает persisted identity/layers, вычитает durable revocations и повторяет owner/attachment + durable token authorization через native one-use token. SQL transaction не охватывает filesystem I/O. `NewResumeProducer` связывает bounded worker с lookup/release application ownership, удерживая release через failures и publication.
- `internal/usecase/coordinator.go`, `command_coordination.go`: shutdown-only `coordinatorDrain` на **том же** registry sorted keys. Capability выдаётся только после обычного admission close и join его references; обычные команды не оживают от свежего context. Cancelable drain reservations удерживают общие refs; finish запрещён до join. Drained channel закрывается ровно один раз, в том числе при конкурентном coordinator cleanup/producer shutdown.
- `internal/usecase/resume_producer.go`: application adapter повторяет owner lookup под существующими record/storage/root reservations; сохраняет их до конца seal, включая cancellation/blocked callback. Worker shutdown получает budget параллельно с coordinator transition. Новые drain callbacks ждут capability без refs, поэтому не удерживают ресурсы, которые ждёт beginDrain. Первый shutdown context authoritative; Wait присоединяет worker и transition перед retirement capability.
- `internal/repository/resume_library_test.go`: настоящие fork clients, payload, library hashing, rooted flush, SQLite и adapter. Полные v1/v2/hybrid проверяются, sealing идёт через worker, после client/backend/DB close открываются снова дважды с **нулём startup payload reads**, затем читается настоящий trusted Range. Subprocess kill после committed seal, внутри post-flush callback до следующего commit и после durable revoke; все три формата открываются через adapter без rehash, revoked remainder не получает readiness. Это process-crash evidence на read-only restore, не production writable/power-loss acceptance.
- `resume_adapter_test.go`: late attachment/epoch/cancellation/capture/catalog failures сохраняют старую generation; invalid capability/lookup не публикуют и не теряют release. `resume_storage_test.go` переключён на adapter/producer с сохранением прежних durable revoke/late-token checks.
- `coordinator_drain_test.go`, `resume_producer_test.go`: normal owner join до drain, cancel/key exclusion, expired budget не освобождает reservation, unique capability и retirement, actual Engine.Close + producer + SQLite publication после coordinator cancellation, blocked capture retains refs, lookup mapping/record/owner revalidation.
- Runtime safety gate намеренно не обойдён: producer пока не установлен в NewTorrentUseCase, native writable restore по-прежнему запрещён. Production source/dirty scanner и mutation barriers остаются открытыми. Adapter тесты с synthetic native evidence в usecase проверяют только coordination; реальные файлы/hashes/flush покрыты repository library tests отдельно.

Проверки:

- [x] `go test -race ./internal/repository ./internal/usecase -run 'TestResumeAdapter|TestResumeStorageRoundTrip|TestApplicationResumeProducer|TestCoordinator.*Drain' -count=10 -timeout=120s` — PASS (repository 103.062 s, usecase 10.582 s). Последние lookup-revalidation assertions дополнительно включены в полный check.
- [x] `make check` — exit 0: root/fork tests/race/vet, 40 frontend tests, frontend/application build. Repository race 51.943 s, usecase race 193.391 s, fork race 8.445/2.591 s.
- [x] После полного check: Windows amd64/CGO=0 repository/usecase test binaries собраны; native Linux arm64 adapter/library subprocess/coordinator suites ×3 — PASS. Docker local FS, 2 CPU/1 GiB, `--network none`, read-only binary mount. Native Windows execution остаётся открытым gate.
- [x] Native CI selector расширен application producer/coordinator drain tests; repository `TestResume` selector уже включает library crash/adapter suite.

Логи/бинарники: `phase43-adapter-*` в `/private/var/folders/zg/l0438jl160s4wr89pw16vh4c0000gn/T/opencode/`, full check — `phase43-adapter-check.log`.

**Зависимость приёмки:** F43-03 требует production writable restore и periodic seal во время downloads, но правило безопасности запрещает это до mutation hooks F43-04. Read-only clean/crash path и coordinated shutdown publication уже доказаны; закрывать F43-03 целиком или ослаблять writable gate без покрытия mutations нельзя. Для завершения требуется согласовать выполнение safety-critical hooks блока 4 вместе с production integration блока 3; конфигурация/API/GUI блока 5 при этом остаются вне текущей работы.

На этом этапе пользователь отдельно выбрал **«Нет, сохранить границу»** на предложение совместить production integration F43-03 с safety hooks F43-04. Тогда блок 4 не начинался, writable gate оставался закрыт, F43-03 не отмечался выполненным. После этого пользователь поручил продолжить до завершения следующего блока; агент явно определил его как блок 4, и пользователь продолжил эту работу командами «Продолжай». Ниже — результат этого отдельного разрешения, без включения config/API/GUI блока 5.

## Блок 4 — mutation hooks, aliases и lifecycle fencing

### Реализовано

- `internal/repository/resume_mutation.go`: durable `InvalidateResumeMutation` проверяет FULL/WAL и exact current record/storage/mapping, но допускает active recovery journal/deletion intent для **отзыва**, не для выдачи trust. Epoch повышается даже у invalidated head, исключая late pending seal. `RevokeResumeMutation` отзывает bitmap по coherent CAS snapshot; уже discarded generation сообщает backend о возможности очистить весь pending cache.
- `NewResumeMutationHooks` связывает callbacks с одним open/record/mapping. В verify mode owner отзывается до открытия/проверки без checkpoint admission: explicit verified recovery во время journal остаётся возможной. Fast/internal writable path принимает только пригодную geometry, иначе invalidation/fallback; метаданные и hints не становятся readiness.
- `internal/engine/client_config.go`, `storage_roots.go`, `internal/usecase/torrent_uc.go`, `resume_mutation.go`: main rooted backend получает per-open hooks от usecase; canonical owner один на aliases, split layout отвергается. Initial Add может открыть подготовленную mapping до первого intent: это допускается только без record owner и live checkpoint, затем provisional callback связывается с одним record. Drop/Close joining сохраняется, callback не может переназначиться на re-add.
- Fork `storage/resume-mutation.go`, `file-torrent-io.go`, `file-piece.go`, `file-client.go`: persisted pending bitmap коалесцирует revoke до overwrite/known-invalid completion/verification. New/untrusted chunks не выделяют mask и не выполняют safety SQL; discarded owner очищает cache. До publish сохраняется union старого и возможно committed bitmap, поэтому потерянный commit acknowledgement не разрешает последующую запись без отзыва.
- Пропажа/truncation/replacement файла требует отзыва **всех** его contributing pieces до `O_CREATE`, extension и implicit zero fill. `.part` import и zero-file creation требуют namespace invalidation до эффекта; известный конфликт preflight оставляет оба файла нетронутыми.
- Per-file payload leases в offset order охватывают реальные reads/hash reads/writes. `ReadAt` проверяет freshness после shared lease; overwrite держит exclusive lease и закрывает live gate до write. Это file syscall fencing, не вторая application mutation lock система. Independent files не блокируются одним чужим read lease.
- Close атомарно закрывает read admission, ждёт file leases включая metadata evidence + SQL revoke callback, очищает freshness/zero proofs и закрывает handles. Readers не используют exclusive seal gate, поэтому online publish не останавливает read-only streams. Закрытый backend не может читать данные нового владельца при root reuse.
- Fork `piece.go`: known v2 hash replacement отзывает persisted/live trust вне `Client.lock`, учитывается как layer work, после I/O проверяет backend pointer/old hash/closed state. Storage lock освобождается до повторного Client.lock; Drop/Client.Close присоединяют задачу. Ошибка revoke не применяет hash и не оставляет known-invalid live readiness.
- Rooted zero-length readiness больше не выводится из отсутствия pieces: `RefreshEmptyFile`/`CachedEmptyFile`, `File.storageIndex` и file observations используют safe-open proof. Polling остаётся I/O-free. Readonly/writable restore candidates не создают missing empty payload. Backend без rooted verified callbacks сохраняет историческое поведение, но fast trust ему не выдаётся.
- Evidence mismatch/short-read закрывает live file gate даже при SQL failure. При writable prepare affected file durable revoke может изменить epoch; `RestoreResume` один раз перечитывает coherent snapshot и повторяет prepare/import, сохраняя независимые пригодные files. Ошибки durability не игнорируются. Исправлен nil snapshot fallback при отсутствии checkpoint.
- `internal/usecase/recheck.go`, `temporary_files.go`, `storage_worker.go`, `storage_migration.go`, `legacy_recovery.go`, `storage_inventory.go`: barriers до recheck, repair, reconcile, move/locate execution, migration, legacy publication и manual/FIFO unlink. Selection и Pause/Resume без repair не отзывают независимый trust. Все прежние transfer/ownership/journal/volume/DiskFull gates сохранены.
- Migration `0021_resume_mutation_fences.sql`: active recovery journal insert/update и mapping workflow transition атомарно отзывают owner. Завершение/очистка journal не оживляет прежний checkpoint. Trigger epoch exhaustion не допускает mutation без барьера. Upgrade fixture удаляет только phase-43 artifacts во временной БД и заново проверяет отсутствие adoption старых hints.
- Native checkpoint flush использует независимый job context, а не уже cancelled runtime root-I/O context. Обычный writer при этом остаётся cancelled. Import memory budget учитывает writable pending/revoke masks: `trustedPieces*256 + files*2048 + bitmapBytes*8 <= 32 MiB`.
- README, state/concurrency contracts, phase43 contract и native CI selectors обновлены. Новых API/config/GUI полей нет; producer/dirty scanner по умолчанию всё ещё не установлен.

### Приёмочная матрица и тесты

| Область | Evidence |
| --- | --- |
| Normal overwrite / SQL failure | Real library+SQLite v1/v2/hybrid: injected revoke UPDATE failure оставляет настоящие payload bytes неизменными; success durable-revokes только touched pieces; untrusted повтор не меняет epoch; recheck не оживляет старый bitmap |
| Create / truncate / read error | Native tests: missing/truncated file не создаётся/не расширяется при failed full-file barrier; v1 boundary pieces отзываются вместе, независимый file остаётся ready; known read error закрывает live gate и при SQL failure |
| Crash ordering | Kill настоящего library/SQLite subprocess до overwrite после durable revoke и после overwrite; v1/v2/hybrid reopen не читают payload для startup hashing и никогда не доверяют overwritten piece. Изменённый file evidence может дополнительно отозвать остальные pieces этого файла; неизменившийся timestamp не оживляет touched piece |
| Restore evidence / zero | Changed mtime при writable restore durably отзывает affected file, bounded epoch reload завершается без rehash/trust; subsequent fresh verification восстанавливает только current-open readiness. Missing zero не создаётся readonly/writable import и не получает direct readiness |
| v2 layers | Barrier test: blocked revoke не держит Client.lock, known-invalid piece уже не ready; failure не применяет replacement hash, независимый piece остаётся ready |
| Recheck / reconcile / repair | Реальные managed torrents с sealed verified payload: old checkpoint отзывается; injected SQL barrier failure не запускает соответствующие эффекты и не создаёт missing payload |
| Move / locate / migration / legacy | Реальные workflows, plus SQL triggers; successful move/locate не переносят trust, failed barrier не публикует target/не изменяет source. Legacy final/part сохраняются при отказе barrier; explicit verified deletion-cancelled recovery не блокируется правилом checkpoint admission |
| Delete / re-add / aliases | Manual keep/payload deletion и re-add не наследуют record capability; FIFO использует тот же deletePayload path. Consistent alias использует одного canonical owner, split alias layout отвергается, stale mapping/record callbacks не проходят |
| Lifecycle / reads | Native syscall leases join до overwrite; independent file прогрессирует. Evidence SQL callback удерживает lease до возврата; Close не освобождает owner раньше. Closed attachment ReadAt/WriteTo/CachedComplete/CachedEmpty не дают доступ к reused root |
| Selection / paused / safety | Selection и Pause/Resume без repair сохраняют generation/revocations. Полные phase42 engine/usecase suites проверяют sticky DiskFull, transfers, ownership, interrupted journals, paused intent и QoS |

### Проверки блока 4

- [x] Targeted native/fork race suites многократно: `TestFileCheckpoint|TestResumeMutation|TestCheckpoint` ×10; отдельный полный fork core/storage race после последних Close/read-lease исправлений — PASS.
- [x] Root targeted race: `TestResumeMutation|TestResumeAdapter` и application mutation/fault/recovery matrix — PASS; реальные writable/crash сценарии повторены ×3.
- [x] Финальный `make check` — **exit 0**: root/fork tests/race/vet, 40 frontend tests, frontend/application build. Лог `phase43-block4-acceptance-final.log`; repository race 63.218 s, usecase race 217.268 s, fork core/storage race 8.442/2.576 s.
- [x] Финальные Windows amd64/CGO=0 binaries repository/usecase/engine/fork core/storage собраны. Это cross-compilation, не native execution.
- [x] Те же последние исходники: native Linux arm64 repository/usecase/engine suites ×3, fork core/storage ×20 — PASS. Go 1.25.8 static binaries, Docker `golang:1.26-alpine`, `--network none`, 2 CPU/1 GiB, binary read-only; payload/SQLite на native container FS. Это не Linux race, NAS или SMB/NFS acceptance.
- [x] `git diff --check` и финальные state/contract/implementation docs.

Первый full check блока 4 выявил реальный compatibility defect: strict factory пыталась использовать checkpoint admission при deletion-cancelled verified recovery с активным journal. Исправлено разделением untrust-only namespace barrier и checkpoint admission; journal gate **не** ослаблен. Затем full check прошёл; последующие Close/evidence-lease изменения повторно прошли полный `make check`. Один foreground повтор был прерван командой пользователя и не считается успешной проверкой; он перезапущен и завершился exit 0. Mac race linker выдаёт прежние `malformed LC_DYSYMTAB` warnings без test failures.

Логи/бинарники блока 4: `/private/var/folders/zg/l0438jl160s4wr89pw16vh4c0000gn/T/opencode/phase43-block4-*`. Финальные native logs — `phase43-block4-linux-{repository,usecase,engine,core,storage}-final.log`.

**Статус:** F43-04 выполнен. F43-03 не закрыт автоматически: writable library protocol и hooks теперь доступны, но production source/dirty scanner и default installation producer/import остаются отдельной незавершённой работой. Блоки 5–6 не начинались, native Windows execution остаётся финальным platform gate всей фазы.

## Продолжение блока 3 — dirty revisions и concrete production source

- Fork: `TorrentImpl.CheckpointState` и `Torrent.FileCheckpointState` дают I/O-free revision/dirty accessor. Backend хранит captured/sealed revisions; failed publish ничего не подтверждает, successful seal подтверждает только revision начала capture. Evidence withdrawal во время publish остаётся dirty; closed backend не попадает в очередь. Successful restore подтверждает импортированную revision без фиктивного нового скачивания.
- Evidence/revoke paths увеличивают epoch под `freshMu`. После rooted I/O и последнего owner callback `restoreCheckpoint` повторно проверяет token epoch **под тем же `freshMu`**, до atomic import. Поздняя read-side потеря evidence больше не может быть перекрыта импортом старого token.
- `context.Canceled`/`DeadlineExceeded` при rooted read/stat не считаются corruption и не отзывают durable/live trust; отменённый zero probe сохраняет уже полученный proof. Это нужно для независимого shutdown flush после отмены runtime I/O context, но не ослабляет обработку actual missing/short/error evidence.
- `internal/repository/resume_scan.go`: keyset scan distinct canonical storage IDs, максимум 64 на страницу; без загрузки общей read model/bitmap и без создания resume heads. Отдельный read-only `CheckResumeAdmission` проверяет coherent record/mapping/alias/journal/deletion fence без FS I/O.
- `internal/usecase/resume_source.go`: concrete source захватывает runtime pointer, attachment/intent fence и applied revision; callback повторяет durable admission, current intent, runtime/durable safety/DiskFull, inventory session и rooted volume marker/device checks. Callback не берёт command keys и не вызывает library под checkpoint callback. Paused и orderly runtime cancellation сами по себе не отклоняют seal.
- Bounded `scanResumeDirty` и односекундный scanner loop читают только cached revisions, не stat/hash/flush; насыщение queue не подтверждает revision и допускает повторный обход. Periodic worker interval остаётся 60 s.
- Regression tests: failed/successful seal acknowledgement, evidence withdrawal во время publish, late token epoch, cancelled read/stat/zero proof; repository pagination на 131 owner и read-only admission; real managed paused source, stale attachment, durable verification/DiskFull и реальный shutdown seal через application drain с независимым budget.

**Граница этой промежуточной части на момент её завершения:** source/scanner были реализованы, но constructor ещё не устанавливал их. Backend-open restore, runtime mode constructor, final shutdown scan и HTTP Range/clean/crash integration оставались открытыми. Их последующая реализация описана ниже; блок 5 не начинался.

Проверки этой части:
- [x] Fork core/storage `go test . ./storage` — PASS; новые checkpoint regressions отдельно — PASS.
- [x] Root source/pagination/admission tests — PASS; targeted repository/usecase race ×5 — PASS.
- [x] Fork storage checkpoint/mutation race ×5 — PASS.
- [x] Windows amd64/CGO=0 repository/usecase/core/storage cross-compile — PASS, не native execution.
- [x] Native Linux arm64 repository/usecase source/pagination/admission ×5 и fork core/storage checkpoint/mutation ×5 — PASS (local Docker FS, без сети, 2 CPU/1 GiB).
- [x] Полный `make check` — PASS: root/fork tests и race, 40 frontend tests, frontend/application build, root/fork vet. Repository race 65.737 s, usecase race 213.246 s, fork core/storage race 8.455/2.674 s. Лог `phase43-source-check.log`, остальные логи/бинарники `phase43-source-*` в прежнем approved temp directory.

## Завершение production-интеграции блока 3

### Что реализовано

- `NewTorrentUseCase` автоматически устанавливает единственный producer/finalizer и managed scanner task. Public default по-прежнему `verify`. Добавлен явный restart-scoped Go entrypoint `NewTorrentUseCaseWithResumeMode(..., fastresume.Fast/Verify)` для runtime-интеграции; неизвестный mode отвергается, ENV/config/API/GUI не менялись.
- `ResumeOwnerMapping` переведён на один coherent context-aware SQL read: owner count, mapping и record не собираются из разных snapshots, и final scan не теряет свой cancellation budget на последующих non-context pool acquisitions.
- Failed finalizer registration останавливает и присоединяет только новый worker, не закрывает existing coordinator и не крадёт finalizer slot. После successful registration worker принадлежит Engine даже при racing rejection scanner task; caller при constructor error по-прежнему обязан закрыть свой Engine/DB. Обычные fixtures используют installed producer; только tests отдельного drain protocol явно оставляют slot свободным.
- `applicationResumeProducer.Shutdown` закрывает normal scan admission, запускает bounded final dirty scan (не более 1 s из общего independent 10 s budget), затем worker drain; coordinator drain transition запускается параллельно. Final scan keyset-paginated, bounded-memory, останавливается при queue saturation. Ошибка/timeout этой lossy scheduling оптимизации не отменяет уже поставленный drain и не заменяет durable revocation.
- Fork `FileResumeHooks.Restore` вызывается один раз после native initialization, но до публикации backend в library; namespace import отзывает checkpoint и очищает candidate status, callback после него не запускается. Restore callback error закрывает backend, включая любой уже импортированный live state.
- Fast per-open hooks используют existing repository adapter и one-use native token. Source привязан к exact tracked library open context/generation: до managed registration именно `closedCtx`, tracked open join и final closed-state check запрещают stale attach. Уже опубликованный magnet runtime дополнительно требует неизменных pointer/attachment fence и отсутствия live safety blocks. Существующие namespace reservations/Drop joins и atomic record/mapping/journal fencing не заменены второй lock-системой.
- Per-open authorize повторяет current intent version, canonical owner/mapping/aliases/journals/deletion, durable safety/DiskFull, inventory session и rooted volume evidence. Нельзя превратить новый lookup по hash в capability старого attachment. Недоступная optimization/metadata или prepare/import failure требует успешного exact durable invalidation перед ordinary verification; failed barrier отвергает open, не меняет bytes и не выдаёт readiness.
- Restore candidate не создаёт missing zero files, в том числе при fallback; `.part` import не может восстановить прежний snapshot. Strict verify всегда отзывает предыдущую generation. `VerifyOnOpen` остаётся включённым.
- `Torrent.FileCheckpointMetainfo` получает только binding info/layers без копии tracker/webseed lists; layer allocation ограничена заранее. Cached open metainfo ограничен file/declared-string size. Application envelope дополнительно резервирует `allPieces*256 + files*2048 + bitmapBytes*8 + metadataBytes*4 <= 32 MiB`, metadata ≤4 MiB. Это консервативнее generic partial-native budget; oversize пропускает seal/даёт ordinary verification, не урезает snapshot. File-count preflight использует bounded iterator без полной новой slice.

### Реальная приёмочная матрица

- **Automatic shutdown + clean:** v1/v2/hybrid warm payload → installed final scan (без ручного MarkDirty/SealResume) → Engine/DB Close → два последовательных reopen/RestoreTorrents. Startup и readiness доверенного полного payload дают **0 hash payload bytes**, data readable, paused intent сохранён, restore не делает owner dirty и не потребляет checkpoint.
- **Partial:** все три formats; второй piece реально перезаписан invalid bytes через guarded native writer до seal. Shutdown сохраняет только первый piece. После reopen хешируется ровно untrusted remainder (16 KiB), а не trusted prefix; untrusted storage read запрещён, trusted torrent reader возвращает prefix. Ноль startup hashing относится к **trusted subset**, не ко всей partial библиотеке.
- **Crash:** настоящий application/library/SQLite subprocess делает online seal через production source с command reservations; parent kill до любого shutdown/DB cleanup. Полный/partial × v1/v2/hybrid reopen сохраняет пригодный subset без повторного его hashing. Это process crash, не имитация power loss; overwrite/revoke crash boundaries дополнительно покрыты матрицей блока 4.
- **Periodic:** настоящий installed one-second scanner + настоящий 60-second ticker, без manual seal/shutdown. Всё время открыт torrent reader и идут writes в untrusted second piece. Generation публикуется с bitmap только trusted prefix, reader продолжает читать, writes прогрессируют после seal. Targeted run — PASS, 60.104 s.
- **HTTP Range:** реальный API router после automatic shutdown/reopen возвращает `206`, `Content-Range: bytes 6-15/21`, тело `downloaded`; startup и readiness не хешируют trusted payload.
- **Fallback:** timestamp evidence mismatch в Fast и явный Verify выполняют ordinary verification всего файла; failed SQL revoke/invalidation при evidence mismatch не публикует runtime, не читает payload и не изменяет bytes. Oversize metainfo и huge declared bencode string отвергаются до unbounded decode; oversized catalog не материализуется целиком.
- **Ownership:** повторная попытка installer не заменяет producer и не закрывает coordinator. Fork tests проверяют one-shot open restore, close/withdrawal при callback error после import, suppression после namespace import. Прежние library Drop/Close/open, stale token, alias/re-add, flush/SQL failure и independent-owner suites остаются обязательными.

### Проверки этой интеграции

- [x] Targeted application runtime clean/partial/crash/verify/fault tests — PASS.
- [x] Реальный API Range test и fork core/storage suites — PASS.
- [x] Installed periodic test с actual 60-second tick — PASS.
- [x] Windows amd64/CGO=0 repository/usecase/API и fork core/storage cross-compile — PASS (не native execution).
- [x] Native Linux arm64 repository/usecase/API runtime/source/adapter suites ×3 на финальных исходниках — PASS; fork core/storage checkpoint/mutation ×5 — PASS. Container local FS, `--network none`, 2 CPU/1 GiB. Отдельно actual installed 60-second periodic test на Linux — PASS; это не Linux race и не SMB/NFS/NAS acceptance.
- [x] Полный `make check` — PASS: root/fork tests и race, 40 frontend tests, root/fork vet, frontend/application builds. Repository race 64.995 s, usecase race 294.479 s, fork core/storage race 8.789/3.070 s. Лог `phase43-runtime-check.log`.
- [x] После последних bounded file-count/metadata preflight и coherent owner lookup: полный финальный root `go test ./...` — PASS (usecase 149.970 s); final repository/usecase/API resume/source/producer race — PASS (34.825/36.483/2.347 s), periodic уже покрыт полным race check. Отдельный runtime/source/API targeted race ×3 также прошёл. Final root vet/build и `git diff --check` — PASS.

Логи/бинарники: `phase43-runtime-*` в approved temp directory; финальные root/race — `phase43-runtime-root-final.log` и `phase43-runtime-final-race.log`, native root — `phase43-runtime-linux-*-final.log`. **F43-03 выполнен**; блоки 5–6 не начаты. Native Windows execution остаётся gate финальной приёмки всей фазы, CI selectors расширены installed runtime/source и HTTP tests. Прежние macOS race linker `LC_DYSYMTAB` warnings сохраняются без test failures.

## Блок 5 — default policy и наблюдаемость; продолжение блока 6

### Реализация

- `internal/config/config.go`: `HUB_FAST_RESUME_MODE=fast|verify` / `--fast-resume-mode`, default fast, restart-scoped. Unknown и explicit empty CLI/ENV отвергаются validation до admission Engine. Пустой Go Config использует default fast. `internal/config/resume_test.go` проверяет default upgrade, оба режима, CLI precedence и ошибки.
- `NewTorrentUseCase` выбирает проверенный engine mode вместо прежнего hardcoded verify. Explicit Go constructor оставлен. `VerifyOnOpen` не отключён, strict invalidation/recheck barriers не менялись. Compose и README документируют новый default и внешних writers.
- Native `pieceEvidence` расширяет **существующие** entries source-tag полем (restored/fresh), без второй bitmap/map. При успешном fresh hash source меняется даже при неизменившейся signature; withdrawal/Close удаляют источник вместе с existing proof. Консервативный production allocation envelope не изменён.
- `FileResumeHooks.Observation` / `TorrentImpl.ResumeObservation` / `Torrent.FileResumeObservation` несут scalar immutable **open-attempt** mode/status/reason и использованную generation/time. Factory/callback заполняют facts до library publication; после публикации их никто не меняет. Новые periodic seals не переписывают историю restore текущего open.
- `FileObservations` использует существующие extents и cached native source для disjoint current restored/freshly-verified bytes; v1 shared pieces и v2 gaps не складываются дважды. Реальный hashing после restore переводит bytes из первого counter во второй. Failed hashes не становятся freshly verified. Cached native proof и existing safety/recheck gates ограничивают counters.
- `models.ResumeProgress` — optional additive REST/list/details/full/compact SSE. Generation — decimal string int64. Compact projection не дублирует catalog/bitmaps. Existing consistent-read runtime/record/attachment stamps сохранены; stale/detached runtime не передаёт старые restore facts. Detached diagnosis: configured mode, pending/storage_blocked и zero counters.
- `web/src/components/ResumeObservation.vue` подключён в обе table/card views: mode/outcome/reason, checkpoint time/generation и раздельные bytes; warning о same-size timestamp-restored edits, external sync/backup и strict verify. Существующий menu Recheck доступен, не делает Resume. File observation label теперь «Usable verified or restored data», не «Verified now».
- Общий Go/TS fixture `internal/models/testdata/resume.json` включает generation `9007199254740993`; Go roundtrip и Vue tests проверяют сохранение строки, fallback explanation и отсутствие поля у старых servers. Контракт, README, phase42 policy addendum, STATE/CONCURRENCY обновлены без изменения исторических результатов phase42.

### Новые проверки

- Full v1/v2/hybrid clean runtime tests проверяют `restored_bytes=payload`, `freshly_verified_bytes=0`, real `verification.checked_bytes=0`, generation/time, несколько polling reads и compact projection.
- Реальный HTTP router list/details/compact возвращает diagnosis; storage observer не получает **ни одного нового события** при polling (проверяется до actual Range). Actual Range после restart остаётся `206` с правильными bytes.
- Evidence fallback и strict mode показывают ноль restored и полный freshly hashed payload, соответствующие outcome/reason.
- Upgrade fixture удаляет resume namespace, оставляет настоящие persisted completion rows `complete=1`. Public constructor без opt-in всё равно хеширует все 32 KiB. После shutdown seal следующий default restart восстанавливает 32 KiB без hashing. Последующая explicit hash verification переводит все bytes из restored в fresh, не меняя paused intent и не удваивая total.
- Strict same-size corruption fixture перезаписывает первый piece, сохраняет прежний mtime и file identity. Verify читает весь payload и оставляет usable только второй piece. Это не обещание, что Fast обнаружит такую правку.
- Record/attachment projection unit tests отбрасывают stale diagnosis; frontend shared fixture/fallback/old-server tests добавлены. Ранее обновлённый Native CI selector включает новые `TestResumeRuntime*` cases автоматически.
- Root `go test ./...` с default fast — PASS; targeted config/models/runtime/API resume race — PASS. Frontend build — PASS; после смены label соответствующий expectation обновлён. Полный `make check` — **PASS**: root/fork tests/race/vet, **43 frontend tests**, frontend/application builds. Root usecase tests 149.002 s, repository/usecase race 65.042/294.437 s, fork core/storage race 8.553/2.704 s. Лог `phase43-default-check.log`. Late cached-proof guard/typed reasons/projection и measurement уточнения проверены targeted race и final root vet. Один повторный root run завершён harness timeout 240 s без usecase результата; остальные пакеты прошли. Отдельный verbose полный usecase run с Go timeout затем **PASS, 147.117 s**, `phase43-default-usecase-investigate.log`; timeout не воспроизведён, не считается зелёным результатом и не скрыт. Ещё один targeted preattach race ×3 был прерван harness timeout до результата; plain ×1 и отдельный race ×1 завершились PASS.
- Дополнительный actual application crash **после native import, до library publication**: full/partial × v1/v2/hybrid. Child использует production hook/adapter, останавливается после successful restore до return из open callback, parent kill не выполняет cold Engine/SQLite cleanup. Следующий reopen сохраняет trusted subset, не создаёт лишнего hashing. Plain и targeted race — PASS; native Linux arm64 ×3 — PASS. Логи `phase43-default-preattach.log`, `phase43-default-preattach-final-race.log`, `phase43-default-linux-preattach.log`. Windows usecase cross-compile обновлён, не execution.
- Native Linux arm64 repository/usecase/API resume/source/producer suites ×3 — PASS; fork core/storage checkpoint/mutation/observation ×5 — PASS; actual periodic 60-second test — PASS. Fork targeted race ×3 на macOS — PASS. Windows amd64 root/fork cross-compilation — PASS, **не native execution**. Linux ограничен local container FS, 2 CPU/1 GiB, network none.

### Сравнительные измерения

Команда: `go test ./internal/usecase -run '^$' -bench 'BenchmarkFastResume(Baseline|Result)' -benchtime=1x -count=3 -timeout=180s`.
Среда: **Go 1.25.8 darwin/arm64**, Apple M3 Max, local FS. Fixture: 4 × 2 MiB, pieces 64 KiB; complete либо два полных + два 50%-valid файла. В read observer искусственная задержка 1 ms, это **не NAS throughput**. Старый baseline явно закреплён за verify; обе ветки используют одинаковый managed warm/restore и настоящие Engine/SQLite close/reopen. Исторические baseline выше не переписаны. Admission завершения restore и окончания hash queue измерены отдельно.

Медианы трёх прогонов (`phase43-comparison-metrics.log`):

| Метрика | Verify complete | Fast complete | Verify mixed | Fast mixed |
| --- | ---: | ---: | ---: | ---: |
| Hash payload bytes | 8,388,608 | **0** | 8,388,608 | 2,097,152 (только untrusted remainder) |
| Actual requested Range bytes | 16 | 16 | 16 | 16 |
| First API ms | 3.642 | 3.669 | 4.205 | 3.694 |
| Restore admission ms | 11.42 | 18.93 | 12.07 | 19.42 |
| First readable Range ms | 34.47 | 19.64 | 30.19 | 21.36 |
| All usable/check queues settled ms | 672.9 | 19.66 | 553.4 | 63.48 |
| Shutdown ms | 55.46 | 0.253 | 53.80 | 29.73 |
| SQL row writes (включая resume tables) | 172 | 32 | 172 | 68 |
| Native payload opens | 273 | 8 | 239 | 47 |
| Native payload stat events | 3252 | 164 | 2844 | 600 |
| Sampled process Go heap bytes | 9,604,120 | 7,290,184 | 10,353,280 | 9,789,336 |
| Allocated Go bytes | 22,685,496 | 2,285,568 | 22,302,424 | 7,324,920 |

Fast admission может быть медленнее из-за metadata/evidence validation, а all-ready — быстрее благодаря отсутствию hash прохода. Zero hash bytes относится к trusted pieces, не к actual content read. Complete Fast использует direct disk source, поэтому его actual Range не виден native torrent read observer; отдельный `actual-range-B=16` исключает ложное заявление «файл вообще не читался». Hash metric вычитает Range bytes только для torrent source. Heap — sampled process heap, не RSS/гарантированный peak; SQL metric — **rows, не транзакции/fsync**. Метрики не CI thresholds, latency noisy, включая concurrent checks.

### Открытая приёмка на момент завершения блока 5 (история)

- Native Windows runner в текущей среде недоступен. Для закрытия F43-06 нужен выполненный Windows job `.github/workflows/checks.yml` на **этом дереве** (native `go test -race`, locking/durability/runtime/HTTP), а не старое подтверждение фазы 42 или cross-compile. Код не публиковался, CI автоматически не запускался.
- Не объявляется закрытой вся расширенная subprocess crash matrix до/после каждого payload/namespace flush. Уже есть process-kill revocation/write/seal, actual application full/partial v1/v2/hybrid reopen **и pre-attach kill**, плюс deterministic flush/admission/commit fault tests; это не замена всем перечисленным отдельным crash boundaries.
- SQL transaction/fsync amplification и отдельная online seal latency пока не измерены отдельно от SQL row writes и shutdown latency; оставить соответствующую часть F43-06 открытой.
- Power-loss/NAS/SMB/NFS не испытывались. Process kill не доказывает power-loss semantics; заявления ограничены ordering FS flush + SQLite FULL/WAL и known local filesystem primitives. Нет обещаний выявлять hidden external changes в fast.

## Завершение оставшихся доработок блока 6 (без Windows)

Команда пользователя: «Приступай к оставшимся доработкам, кроме windows проверки. О ее результатах я напишу потом». Windows execution в этой части **не выполнялся**; исключение не считается подтверждением platform gate.

### Crash/fault acceptance

- `internal/repository/resume_crash_boundaries_test.go`: реальные SQLite WAL, torrent library и rooted файлы; v1 shared boundary piece между файлами 20,000/12,768 B, вложенная directory и zero-length file. Реальный hash-проход до baseline seal. Сначала публикуется generation 1 с subset `0b01`; generation 2 может добавить второй piece, но не до полного завершения протокола.
- **18 shutdown-worker subprocess kill points:** до/после native evidence snapshot; до/после каждого из трёх payload flush (включая empty); до/после каждого namespace sync (`.`, `payload`, `payload/film`, `payload/film/nested`); до/после SQL seal publication. **2 periodic-worker kill points:** до/после SQL publication на настоящем production 60-second ticker. Два periodic cases параллельны, таймер не подменён.
- Parent ждёт точный barrier, делает process kill без cleanup, затем заново открывает SQLite, rooted storage и library. До commit остаются **generation 1 / mask 01**, после commit — **generation 2 / mask 11**. Проверяется не только metadata: trusted subset даёт нужную cached readiness без hashing, реальные ranges читаются, unsealed remainder не получает readiness, все physical bytes/lengths сохранены. После generation 2 reader читает через shared file boundary.
- Snapshot/sync before/after signals добавлены к existing optional `IOEvent`; без callback нет новых SQL/FS operations или authority. `after` означает успешный **platform helper return**, не универсальный OS fsync и не hardware proof. Fork unit test проверяет порядок и отсутствие after/publish при ошибке helper.
- Существующие actual full/partial v1/v2/hybrid clean/crash, before-/after-revoke/overwrite, repeated reopen и pre-attach kill tests остаются частью матрицы. Native flush-boundary fixture дополняет их проверкой multi-file/zero-length namespace, не подменяет форматные тесты.
- `resume_seal_fault_test.go`: **настоящая SQLite writer contention после native flush** на отдельном connection, bounded deadline — generation не меняется. Отдельный actual successful WAL commit с injected lost acknowledgement — backend сохраняет union potentially persisted masks; последующая запись piece, отсутствовавшего в прежней generation, durably отзывает его в новой. Не подмена error-only mock вместо committed SQLite.
- Fork `TestCheckpointFailuresNeverPublish` дополнен explicit file/directory `ENOSPC`, access denied и deadline failures: код ошибки сохраняется, publish отсутствует, admission освобождается. Это injected syscall errors, **не физическое заполнение диска**. Sticky DiskFull/volume/intent gates остаются покрытыми существующими application tests.
- `TestResumeRuntimeStrictDetectsHiddenSameSizeEdit` теперь проверяет и opt-out/re-enable: после строгого выявления порчи и нового seal переключение обратно в fast не оживляет отвергнутый piece; доверенный второй piece не хешируется повторно, paused intent сохранён.

### Transaction и online-seal measurements

Benchmark расширен без production SQL tracing: тестовый WAL parser считает actual committed **write transactions** по commit frames, не rows. `wal_autocheckpoint(0)` задан каждому measurement connection; empty writer reservation защищает sampling от async hint writer. Salt reset/partial/stale frames отвергаются, лишний payload не читается. Unit test различает 17 rows в одной transaction, отдельный autocommit и rollback: ровно 2 commit frames.

`online-seal-4-owners-ms` — explicit seal четырёх owners через production source/adapter и existing command reservations, без Drop. Это service time, не 60 s scheduling delay; reader/write continuity проверяется отдельным installed periodic integration test. Для измерения выполняется даже unchanged seal. Из-за уже queued работы возможен дополнительный shutdown seal.

Команда прежняя: `go test ./internal/usecase -run '^$' -bench 'BenchmarkFastResume(Baseline|Result)' -benchtime=1x -count=3 -timeout=180s`. Go 1.25.8, macOS arm64, M3 Max/local FS, одновременно могли идти checks; **нет latency/throughput thresholds**. Медианы последнего прогона (`phase43-final-measurement.log`):

| Метрика | Verify complete | Fast complete | Verify mixed | Fast mixed |
| --- | ---: | ---: | ---: | ---: |
| Startup/restore committed write tx | 110 | 9 | 112 | 41 |
| Explicit online seal committed write tx | 4 | 4 | 4 | 4 |
| Shutdown committed write tx | 0 | 0 | 0 | 0 |
| Всего committed write tx | 114 | 13 | 116 | 45 |
| WAL appended bytes | 667,440 | 234,840 | 675,680 | 366,680 |
| Online seal четырёх owners, ms | 59.59 | 45.29 | 50.40 | 41.67 |
| Successful checkpoint payload helpers | 4 | 4 | 4 | 4 |
| Successful checkpoint namespace helpers | 12 | 12 | 12 | 12 |
| SQL trigger row writes, включая forced online seals | 172 | 40 | 172 | 72 |
| Hash payload bytes | 8,388,608 | **0** | 8,388,608 | 2,097,152 |
| Actual requested Range bytes | 16 | 16 | 16 | 16 |

Empty/read-only transactions не попадают в write-tx metric. Прежние SQL row/latency результаты выше относятся к прежнему benchmark без принудительного online seal и сохранены как история, не переписаны. Новые metrics не выдают каждую transaction за fsync.

### Actual Linux fsync amplification

Отдельный **native Linux arm64** run: compiled Go 1.25.8 binary в Docker Linux VM, 2 CPU/1 GiB, local container FS. `strace -f -yy -ttt -e trace=fsync,fdatasync` следит только за своим test child. Tracer установлен через Alpine apk в ephemeral container; внешние BT providers отключены в fixture. `TSH_RESUME_TRACE=1` печатает wall-clock boundaries. Setup и cleanup вне begin/end исключены; Engine.Close включён, последующий внешний DB.Close/final WAL checkpoint — нет.

`scripts/summarize_resume_syncs.py` разбирает interleaved unfinished/resumed syscalls, failures отдельно, generation/rows не превращает в guessed sync counts. Incomplete/undecoded trace отвергается. Unit tests и инструкция воспроизведения: `scripts/README-resume-measurement.md`.

Медианы трёх traced прогонов (`phase43-final-sync.trace`, `phase43-final-sync-benchmark.log`, `phase43-final-sync-summary.json`):

| Actual successful fsync calls | Verify complete | Fast complete | Verify mixed | Fast mixed |
| --- | ---: | ---: | ---: | ---: |
| Startup SQLite WAL | 141 | 9 | 141 | 41 |
| Startup payload | 132 | **0** | 98 | **0** |
| Startup directories | 3 | 1 | 2 | 1 |
| Online seal SQLite WAL | 4 | 4 | 4 | 4 |
| Online seal payload | 4 | 4 | 4 | 4 |
| Online seal directories | 12 | 13 | 12 | 12 |
| Shutdown SQLite WAL | 4 | 0 | 4 | 0 |
| Shutdown payload | 4 | 0 | 4 | 0 |
| Shutdown directories | 12 | 0 | 12 | 0 |

В этих windows не было failed sync calls и fdatasync calls. OS trace включает прочие namespace/background operations, не только checkpoint helpers. Трассировка меняет scheduling: dirty scanner может успеть поставить extra work, поэтому traced counters/latencies не приравниваются macOS untraced result. Число calls — факт попытки/успешного OS return, **не доказательство реального persistence hardware**; Linux local FS не переносится на NAS/SMB/NFS. Power-loss специально не заявляется испытанным, только документированный FS flush → SQLite FULL/WAL ordering.

### Финальные проверки этой части

- [x] `make check` — PASS (`phase43-final-check.log`): root tests/race/vet/build, fork core/storage tests/race/vet, frontend **43 tests** и build. Repository tests/race 68.285/140.871 s, usecase tests/race 150.934/308.246 s; fork core/storage race 8.473/2.712 s. Известные macOS race linker `LC_DYSYMTAB` warnings — без failures.
- [x] Новая actual crash matrix с real periodic ticker под macOS race — PASS, 72.226 s. SQL busy/lost-ack fault race ×3, explicit flush errno cases race ×3 — PASS.
- [x] Native Linux repository/usecase/API resume/source/producer suites **×3** с real periodic crash/runtime cases — PASS, 2 CPU/1 GiB, network none. Fork core/storage checkpoint/mutation/observation **×5** — PASS. После последних assertions отдельно повторены Linux SQL/crash-shutdown/mode-switch/WAL **×3** и observer/errno cases **×5** — PASS.
- [x] WAL unit + mode-switch runtime tests race **×3**, Python parser unit tests и real trace parsing — PASS. После последних test/parser/measurement уточнений полный финальный root `go test ./... -timeout=360s` — PASS (repository/usecase **68.426/149.886 s**, `phase43-final-root-late.log`), без harness wall timeout. После последнего reuse уже вычисленного path в observer (без дополнительных path allocations) полный финальный fork race — PASS (core/storage **9.046/3.430 s**, `phase43-final-fork-late.log`); runtime/usecase/API targeted race — PASS (**20.299/1.838 s**, `phase43-final-runtime-race-late.log`). SQL contention/lost-ack race ×3, observer/errno race ×3 и повторные native Linux shutdown-boundaries/SQL faults ×3 — PASS. Final root/fork vet, application build, Python parser unit tests и `git diff --check` — PASS. Python parser включён отдельным Linux CI step.
- [ ] **Windows native gate:** по просьбе пользователя не выполнялся, ожидается отдельный результат. Native repository timeout в CI увеличен до 240 s из-за actual 60-second crash cases, parser unit step добавлен в Linux CI.

Новые safety протоколы/настройки сверх блоков 1–5 не вводились; production изменение этой части — только optional checkpoint boundary observations. Исходники и отчёт остаются незакоммиченными, ничего не опубликовано. До Windows evidence вся фаза остаётся открытой, следующая фаза не начиналась.

## Статус Definition of Done

- [x] F43-01: контракт/budgets/policy primitives/baseline подготовлены; финальная проверка блока прошла.
- [x] F43-02: durable SQLite checkpoint/revocations, fencing/rollback и instance OS lock реализованы; final make check и native Linux прошли. Windows cross-compile прошёл; native Windows gate всей фазы остаётся открытым.
- [x] F43-03: installed periodic/shutdown seal и validated backend-open restore; full/partial clean/crash v1/v2/hybrid, active reader/writes, HTTP Range и fallback/fault cases проверены; root/fork/race/vet/build и native Linux — PASS.
- [x] F43-04: core/application mutation hooks, aliases, lifecycle/read-lease fencing; root/fork checks, writable/crash/fault matrix и native Linux прошли. Native Windows gate всей фазы остаётся открытым.
- [x] F43-05: default fast/restart-scoped verify, cached attachment-fenced REST/SSE/GUI diagnosis, disjoint byte sources, string-generation fixture, documented trust/upgrade/backup/downgrade и polling без storage I/O; full check — PASS (повторный late-tree run отдельно, см. выше).
- [x] F43-06: expanded snapshot/per-file/per-directory/shutdown/periodic/pre-attach subprocess crash tests, SQL/flush faults, upgrade и opt-out/re-enable; WAL committed write transactions, online-seal и actual Linux fsync measurements выполнены. Ограничения power-loss/remote FS/external edits документированы. Windows — отдельный platform gate ниже.
- [ ] Итоговый root/fork/frontend check и native Linux/macOS/Windows filesystem suites всей фазы.
- [x] README/config/state/concurrency docs и отчёт актуализированы; финальное Windows подтверждение ещё не получено, вся фаза не закрыта.

## Ограничения и отклонения

Никакой shortcut через `VerifyOnOpen=false` не добавлен. SQLite foundations, process lock, adapters, coordinator drain, mutation hooks, automatic producer/scanner/finalizer, guarded backend-open restore, config/default fast/API/SSE/GUI, расширенная crash/fault matrix и transaction/fsync/seal-latency measurements реализованы и проверены на доступных платформах. Pure `Candidate`, repository CAS token или наличие hooks сами по себе не разрешают readiness: обязательны canonical identity, rooted evidence и one-use restore authorization. Final scan/queue scheduling best-effort и bounded; неопубликованный прогресс может потребовать hashing, но durable revocations не теряются. Консервативный production metadata/import envelope может дать fallback раньше generic native partial limit. Retained heads/checkpoints пока не имеют automatic GC. **Единственный оставшийся gate — native Windows execution**, отложенный пользователем до его отдельного результата. Power-loss/NAS/SMB/NFS — явно неиспытанные условия, не незаявленные гарантии: process kill не доказывает power-loss durability, Linux local FS не доказывает distributed locking. Default fast сознательно не обещает выявлять same-size внешние edits с восстановленным mtime; strict verify доступен через restart-scoped config. Вся фаза до Windows evidence не закрыта; следующая фаза не начиналась.
