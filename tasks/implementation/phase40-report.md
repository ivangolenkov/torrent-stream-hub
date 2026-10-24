# Фаза 40 — Отчёт об имплементации

Статус: **реализация и локальная приёмка завершены 24 октября 2026 года**. Нативные Linux/Windows CI runners подготовлены, но в этой сессии не запускались. Основание: [план фазы 40](../phase40-review-fixes-and-architecture-hardening.md). Фаза 41 не начата.

## Блок 1 — Исходное состояние

- Рабочая база: завершённая фаза 39; до начала изменений в рабочем дереве находился только новый план фазы 40.
- В `pkg/anacrolix-torrent/go.mod` минимальная версия Go согласована с корнем и используемыми методами `os.Root`: 1.25.8.
- Регрессионные сценарии добавляются вместе с исправлениями в соответствующих блоках.

## Проверки

- Исходные `go test -count=1 ./...`, fork `go test -race -count=1 . ./storage`, Vitest (22 теста) и frontend build прошли на Go 1.25.8 / Node 22.15.0. Sandbox не разрешал запись Go cache; Go suites выполнены с разрешённым расширенным доступом.
- Baseline `BenchmarkVerifiedWriteBookkeeping` (Apple M3 Max, 20 iterations): 1000 pieces — 13748 ns/op; 10000 — 135019 ns/op; 100000 — 1382375 ns/op; 0 allocations. Это стоимость bookkeeping одного write, а не throughput загрузки.
- После блока 2 повторный полный `go test -count=1 ./...` прошёл.

## Блок 2 — Каталог и команды

- F40-01: добавлен format v1/v2/hybrid/unknown, миграция `0011_catalog_format.sql`, проверка реальных aligned offsets отдельно от payload size, piece count и overflow. Legacy format не угадывается; корректируется только по новому metainfo-каталогу.
- F40-03: metadata/permanence сохраняются одной транзакцией и одной revision; одинаковая metadata/permanence не меняет revision. No-op не подтверждает предыдущую неприменённую команду.
- A40-04: введены общие `runIntentCommand`/`applyCommittedIntent` для commit/application/bind failures; Add, metadata/permanence, Pause/Resume и priority используют общую границу применения. Полная reconciliation и incremental apply остаются различными.
- Добавлены regression tests composite rollback/no-op/prior failed intent, v2 multi-file Add/sync/restore и v1/v2/hybrid offset/zero-length/overflow. Churn fixture теперь действительно меняет metadata на каждом проходе.

## Блок 3 — Storage

- Публикация `.part` переведена на native atomic no-replace для Linux/macOS/Windows; Link+Remove fallback удалён. Неизвестные legacy hardlink pairs сохраняются для безопасного ручного восстановления.
- Добавлен bounded SQLite completion adapter с неблокирующими Get/Set, ограниченным кэшем и очередью hints. В composition root используется основная SQLite; самостоятельный engine без injected backend использует memory cache. Старый Bolt не открывается и не удаляется.
- Rooted-open проверки объединены в storage port; usecase больше не выдаёт raw torrent.File. HTTP fixtures проверяют данные через публичный Recheck. Добавлены тесты atomic publication/foreign target/legacy hardlinks, SQLite reopen/coalescing/backpressure/failure. Нативные Windows/Linux прогоны подготовлены в CI; локально выполнены macOS suites и cross-builds, см. итоговую матрицу.

## Блок 4 — Verification

- Общие signatures файлов и инкрементальные counters заменяют обход всех verified pieces на каждую запись и завершение piece. Freshness-проход делает stat по уникальным файлам. Read/upload/direct-open/handshake/persistence проверяют свежесть вне client lock, затем применяют результат только к тому же backend.
- Неизвестная completion остаётся unknown: начальная verification после миграции не отключается. Перед persistence внешняя замена отзывает verified progress.
- Добавлены тесты линейного числа stat и доступности client lock при заблокированном storage; полные root tests и fork `-race . ./storage` прошли.
- Повторный `BenchmarkVerifiedWriteBookkeeping`, 20 iterations: 1000/10000/100000 pieces — 70.85/64.55/62.50 ns/op, 0 allocations. Это локальная стоимость bookkeeping, не измерение NAS throughput.

## Блок 5 — Lifecycle и бюджеты

- Добавлены ранняя отмена engine перед join workers, контексты download preparation и capture, общий HTTP admission, отдельная HEAD lease без debounce.
- Бюджеты: 64 service tasks, 256 metadata watchers, 2 verification tasks, `MaxActiveStreams + 4` stream watchers, 2 event workers/128 coalesced pending keys, 256 QoS file states. Saturation команд возвращает ошибку; storage safety сохраняет библиотечную синхронную latch и повторяется обслуживающим worker.
- Полные root tests и race suites engine/usecase/HTTP прошли. Проверены saturation, idempotent release, HEAD без debounce, отмена подготовки download и record wait. Непрерываемый filesystem syscall по-прежнему требуется дождаться перед закрытием storage.

## Блок 6 — Публикация и read model

- Publisher больше не получает record handles и не ждёт observation commits. Два persistence workers объединяют pending работу по hash (128 ключей), сохраняют fencing/identity и пропускают занятые записи до следующего sampling. SQLite writer wait использует request/service context и короткий busy timeout на выделенном соединении.
- Вместо второго полного DB read/DeepEqual используются global/per-record durable versions. Telemetry bytes/timestamps не меняют эти версии. Immutable catalog digest исключает повторные catalog writes; file observations обновляют только изменённые значения.
- Добавлены явный `?view=compact` для REST/SSE, catalog version и загрузка деталей выбранного torrent с проверкой identity/revision. Полные ответы сохраняются. Retained buffers: full 16 MiB, compact 2 MiB; до 128 SSE subscribers с latest-only mailbox.
- SQL baseline (1000 файлов, 20 iterations): 25.52 ms/op, 2000 file writes/op, 1091392 B/op, 61336 allocs/op. После: 1.58 ms/op, 0 file writes/op, 198676 B/op, 3840 allocs/op.
- Root tests и race engine/usecase/HTTP прошли; frontend 22 tests/build прошли на этом шаге; итоговые 27 тестов включают compact-details regression. Проверены publication при занятом record и SQLite writer, версии при ABA/telemetry и компактный payload на 10000 файлов.

## Блок 7 — Discovery и сеть

- Публичные retrackers добавляются только после получения public metadata. Known-private и явно private-safe торренты не получают DHT/PEX/public retrackers; при обнаружении private metadata отменяется текущий DHT announce и отбрасываются прежние peer connections/reserve. Private-safe сохраняется в SourceURI и восстанавливается для magnet и `.torrent`.
- Неизвестный magnet без private-safe использует только явно переданные trackers и обычные DHT/PEX до metadata. Это документированная граница: private флаг из ещё неизвестного info нельзя предсказать. При отключённом DHT для metadata нужен исходный tracker/peer.
- Upload burst не менее 64 KiB при сохранении заданного sustained rate. Boost хранит deadline, продлевается и восстанавливает исходный connection limit; fake clock проверяет expiry и защиту нового поколения.
- Полные root и fork race suites прошли. Добавлены deterministic DHT doubles и тест сохранения private-safe после file Add/restore.

## Блок 8 — HTTP

- Control body 1 MiB/30 s, upload 32 MiB внутри 34 MiB multipart envelope, fields 64 KiB/16 parts; максимум два общих upload/fetch jobs. Spill идёт в управляемый каталог и удаляется на выходе. Oversize/timeout/storage/policy/overload возвращают 413/408/507/403/429.
- Выделенный metainfo fetcher: 60 s на операцию, пять redirects, 32 MiB body, проверка каждого адреса, DNS pinning на фактическом dial, TLS по исходному hostname, proxy environment игнорируется. LAN разрешается только явной host:port/CIDR allowlist.
- M3U URL собирается через URL/path/query encoder; CR/LF в label удаляются. Regression выполняет запрос по сгенерированной ссылке с `#?%`, пробелами и Unicode.
- Root и race HTTP/metafetch suites прошли. Реальный slow HTTP/1 тест выявил auto-drain после снятия deadline; error response теперь закрывает незавершённое соединение. HTTP BaseContext связан с engine shutdown.

## Блок 9 — GUI и контракты

- Подтверждённые priorities ограничены command receipt/record identity. Более новый server snapshot снимает override независимо от значения другого клиента; старые snapshots и details responses не откатывают команду.
- Удалены hard refresh/recycle controls/client methods и отсутствующие поля диагностики. Ручные TypeScript health interfaces проверяются против Go JSON tags, типов и optionality. Boost отображает фактическое состояние engine.
- Legacy hard-refresh/recycle/episode/benchmark env/flags принимаются, но явно диагностируются как deprecated/no-effect; из активных примеров и defaults они удалены. Удаление парсинга допускается только отдельным версионированным изменением конфигурации.
- Root tests, Vitest (27 tests, включая компонент diagnostics) и production build прошли.

## Блок 10 — Проверки и завершающие исправления

- Добавлен reusable `.github/workflows/checks.yml` с read-only permissions: Linux root/fork tests/race/vet/build и Vitest; macOS/Windows native storage race/vet. Release binary/image jobs зависят от него. Go 1.25.8 и Node 22.15.0 согласованы с Docker.
- `make test` включает оба Go-модуля и Vitest; `make check` дополнительно запускает race, vet и build. Fork package list явно ограничен `.` и `./storage`; это не заявление о тестировании всех пакетов upstream-форка.
- Первая полная приёмка обнаружила старые предупреждения fork vet: позиционные литералы внешних типов, копирование mutex в PEX fixture и reflect.SliceHeader при сериализации диагностического peer identity. Литералы именованы, fixture копирует только данные, trace codec хранит числовую identity без восстановления указателя в память другого процесса. Добавлен round-trip/invalid-length тест. Пакеты за пределами core/storage не менялись.
- Процессный SIGTERM regression запускает реальный `run`, открывает SSE и подтверждённо блокирует body reader через Expect: 100-continue. Выявлен auto-drain `net/http` после снятия deadline; незавершённое тело теперь закрывается под expired deadline. Shutdown прошёл без ожидания HTTP drain timeout.
- Общий HTTP budget проверяется одновременно на archive/file/TorrServer endpoints; HEAD проходит без slot, завершение archive освобождает его. 32 control slots удерживаются до завершения handler, ограничивая также накопление уже прочитанных bodies у заблокированных команд.
- Добавлены реальные SQLite query_only/max_page_count failures, cancellation ожидания занятого connection pool, malformed multipart cleanup, flush failure перед publication, потеря acknowledgement после atomic rename/reopen и гонка двух публикаций за один target.
- Последний аудит private-safe охватывает hybrid aliases: повторное добавление по другому hash не может временно изменить discovery policy без её durable SourceURI.

## Итоговые проверки

Среда: macOS arm64 / Apple M3 Max, Go **1.25.8**, Node **22.15.0**. Сначала выполнен `npm ci` по lockfile, затем проверки со свежей frontend сборкой.

| Проверка | Результат |
| --- | --- |
| `make check` | Успех, exit 0: root и fork tests/race, Vitest, TypeScript/Vite, оба vet и backend build. |
| Root `go test -count=1 ./...` | Успех, включая SQLite integration, compatibility и процессный SIGTERM regression. |
| Root `go test -race -count=1 ./...` | Успех; предупреждений race detector нет. |
| Fork `go test -count=1 . ./storage` и `-race` | Успех. Это два изменённых пакета, не весь upstream module. |
| `go vet ./...` / fork `go vet . ./storage` | Успех без отключения анализаторов. |
| `npm test` / `npm run build` | 27 тестов в шести файлах, TypeScript и production build прошли. |
| Backend macOS arm64 build | Успех; `bin/torrent-stream-hub`. |
| `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build ... ./cmd/hub` | Успех cross-build. |
| `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ... ./cmd/hub` | Успех cross-build. |
| Windows `go test -c ... ./storage` | Успех cross-compile; не выполнение Windows binary. |
| Native no-replace с однобуквенным target, `-race` | Успех на macOS; Windows ABI сверена с локальным Go runtime и кросс-компилируется. |
| YAML Checks/Release/Compose и `git diff --check` | Успех. |

Локальный macOS linker при race-сборках печатает `malformed LC_DYSYMTAB`; сборка и тесты завершаются с exit 0. Предупреждение не скрывалось. `npm ci` также сообщает о deprecated транзитивных пакетах; зависимости вне scope фазы не обновлялись.

## Покрытие находок

| ID | Реализация и свидетельство |
| --- | --- |
| F40-01 | Format-aware catalog и migration 0011; repository offsets/overflow и usecase v2 Add/sync/restore tests. |
| F40-02 | Atomic no-replace; flush/acknowledgement/конкурирующий target/reopen tests, сохранение legacy hardlinks. Удалённого шага Remove больше нет. |
| F40-03 | Один metadata/permanence commit; duplicate/no-op/prior-failed-intent regressions. |
| F40-04 | Общая private discovery policy; fake DHT/PEX, deferred retrackers, private-safe file/restore/hybrid-alias tests. |
| F40-05 | Численные body/job/spill budgets; chunked/slow/malformed/temp-failure и retained-body bounds tests. |
| F40-06 | Общий атомарный transfer admission; concurrent slot test и реальные archive/file/TorrServer запросы. |
| F40-07 | Отдельный DNS-pinned fetcher; initial/redirect/IPv6/rebinding/proxy/allowlist/magnet/cancel tests. |
| F40-08 | Cancel до join, HTTP BaseContext, body interruption, cancelable SQLite reads/writes; process SIGTERM и coordinator cancellation tests. |
| F40-09 | Receipt/record/version ограничивают priority overrides; два клиента, пропущенный snapshot и late details regressions. |
| F40-10 | HEAD lease без slot/debounce; HEAD/Delete и HTTP cross-endpoint tests. |
| F40-11 | Burst >= 64 KiB; 1 KiB/s reservation обычного 16 KiB block подтверждает задержку без отказа. |
| F40-12 | Deadline/extension/expiry/restore limit; fake-clock generation test и GUI diagnostics test. |
| F40-13 | URL encoding и CR/LF labels; GET по настоящему M3U URL с особыми именами. |
| F40-14 | Удалены мёртвые controls/types; Go→TS contract test, warnings legacy options, обновлённые примеры. |
| F40-15 | Go 1.25.8 root/fork/CI/Docker; самостоятельные fork suites и release cross-builds. |
| A40-01 | Инкрементальные file signatures/counters, уникальные stat вне client lock; complexity tests и benchmarks выше. |
| A40-02 | Независимый publisher и два bounded persistence workers; blocked record/SQLite writer A не блокирует B. |
| A40-03 | Read/catalog versions, delta file writes, compact subscription/details; SQL benchmark и 10000-file compact test. |
| A40-04 | Общие commit/apply/bind/failure helpers; regression suites intent/reconcile/fencing. |
| A40-05 | Storage metadata handle, единый rooted-open adapter, PieceStatus и verified source APIs. |
| A40-06 | SQLite completion composition root, bounded cache/queue, close/reopen/full/read-only/failure tests; Bolt не открывается production. |
| A40-07 | Численные task/event/transfer/control-body budgets, coalescing и retained safety effects; saturation/cancel/release tests. |
| A40-08 | Make targets, reusable PR checks и release dependency; локальные исполнения отделены от будущих native CI runs. |

## Статус DoD

Чек-лист соответствует исходному плану. Отметки относятся к реализованным контрактам и локальным проверкам; они не означают, что удалённые Windows/Linux runners уже выполнились, или что проведён аппаратный power-cut test.

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


## Принятые решения и ограничения

- Обязательные исправления не перенесены в другую фазу. Фаза 41 не начата; публикация релиза и изменение branch protection не требовались.
- Legacy hardlink pairs не имеют достаточного durable ownership evidence. Согласно предусмотренному планом консервативному варианту обе копии сохраняются; процедура ручного восстановления описана в README. Новый протокол не создаёт эту пару.
- Для неизвестного magnet retrackers отложены до public metadata. В deferred `replace` первоначальные bootstrap trackers сохраняются; известный public `.torrent` заменяет их до admission. Это сознательная граница совместимости, подробно описанная в README. Private-safe требуется до первого discovery, его нельзя обещать ретроактивно.
- Проверки выполнялись на локальной macOS filesystem. Windows/Linux runtime-поведение зависит от подготовленного CI; cross-build не заменяет native execution. Directory sync на Windows не гарантируется этим adapter. Реальное отключение питания и испытания на NAS/сетевых filesystems не проводились.
- SQLite hints допускают потери при overload/full/read-only, но не предоставляют readiness. Legacy Bolt сохраняется; downgrade требует восстановления целого offline backup, включая payload, metainfo и SQLite.
- Application cancellation завершает waits, но не делает любой OS syscall прерываемым. Storage/SQLite не закрываются под активной owned task. Полные snapshots ограничены 16 MiB, compact 2 MiB на публикацию; превышение сохраняет предыдущую публикацию и логируется. Эти sampled DTO не являются доказательством готовности данных.
- Дополнительная очистка старых fork vet warnings и отмена pool wait нужны для обязательной приёмки. Trace serialization больше не создаёт Go pointer из сохранённого адреса; BitTorrent wire format не изменён.
