# Фаза 42 — отчёт об имплементации

Статус: **реализация выполнена; приёмка ожидает native Windows**. Полный `make check` на macOS и native Linux suites прошли. Пользователь разрешил реализацию 25 октября 2026 года. Основание: [план](../phase42-readable-storage-and-file-recovery.md), [контракт](../phase42-storage-contract.md). Фаза не объявлена полностью закрытой: обязательный Windows filesystem DoD ещё не проверен.

## Baseline и блок 1

- Прочитаны правила репозитория, план фазы и существующие storage/recovery/restore границы.
- Начальный worktree содержал только два документа предыдущего исследования/планирования; они сохранены.
- `make check` в песочнице не прошёл из-за `bind: operation not permitted` в тестах локальных сокетов. Это ограничение среды, не regression кода.
- Повторный baseline `make check` вне песочницы прошёл: root tests/race/vet/build, fork `.`/`./storage` tests/race/vet и frontend tests/build. Лог: `/tmp/tsh-phase42-baseline-native.log`. Это macOS baseline до изменений runtime; результаты Linux/Windows для фазы 42 ещё не получены.
- Зафиксирован контракт identity/layout/операций/read model и численных budgets. Схема расширяет существующий mapping, а journal использует механизм фазы 41.
- Общая JSON fixture проверена Go и TypeScript: int64 revisions передаются строками, historical bytes не дают readiness, пустая выборка не получает ложный процент.

## Блок 2 — понятные каталоги и единый resolver

- Миграция `0015_named_storage.sql` добавляет storage identity, mapping revision, prefix и name hint. Файлы при SQL upgrade не перемещаются. Alias-группа меняет layout атомарно; конфликт старой ревизии, пересечение путей и rollback проверены реальной SQLite с повторным открытием.
- Новые `.torrent` используют `payload/<очищенное имя>`; коллизии БД и фактических каталогов получают `(2)`, `(3)`. Сопоставление учитывает Unicode NFC и регистр, имя ограничено 180 UTF-8 байтами. Multi-file prefix сохраняется в mapping; logical metadata paths/index не меняются.
- В Add GUI/JSON/multipart добавлен `folder_name`. Название торрента и title больше не определяют его физический путь после reservation.
- Magnet до metadata сохраняет пустой root. Fork возвращает специальный pending результат без открытия payload; один worker с очередью 64 сигнала и порционным SQL scan подготавливает layout и повторяет открытие. Проверенные metadata сохраняются, private flag закрывает discovery уже в период ожидания. Сбой подготовки публикуется как storage error без повторной записи на каждом polling tick.
- Engine/open/direct HTTP/disk checks, offline recovery, ownership и deletion используют сохранённый layout. Неизвестная версия не переводится в shared layout. Hash-based вычисление осталось только в явном adapter старой раскладки/миграции и standalone engine без application mapping.
- Обновлены HTTP fixtures, ранее записывавшие файлы напрямую в старые hash roots. Отдельные legacy fixtures сохранены. Проверены Range/preload/M3U со специальными именами, очистка, одно-/многофайловые торренты, v1/v2/hybrid aliases, paused restore и неизменность пути при смене title.
- `go test ./internal/repository ./internal/usecase ./internal/engine` прошёл. Отдельные HTTP и fork private-metadata тесты прошли. Полный `make check` прошёл (`/tmp/tsh-phase42-block2-check.log`): root/fork/frontend, race, vet, build. Дополнительная race-проверка последних guard-изменений фиксируется отдельно в `/tmp/tsh-phase42-layout-guards.log`.
- Измеритель прежней migration учитывает два дополнительных постоянных UPDATE от identity/revision triggers; число записей по-прежнему зависит от manifest batches, не chunks/files. Native macOS проверки выполнены; native Linux/Windows для нового filesystem protocol относятся к итоговой приёмке.

## Блок 3 — перенос и конверсия

- Добавлены POST storage-команды и GUI preview/apply/retry/cancel. Один учтённый worker читает durable очередь с лимитом 64, переживает disconnect и shutdown. Планирование не изменяет payload, применение повторно проверяет evidence и закрывает новые transfers; Pause и unrelated Add доступны при долгом копировании.
- Миграция `0016_storage_claims.sql`: атомарные claims source/target/staging, mapping commit вместе с aliases/runtime fencing, retained ownership после отмены и сохранения исходника. REST/SSE читают компактные summary-колонки, а не весь файловый journal.
- Native no-replace directory rename; при EXDEV и конверсии prefix — потоковое копирование со sparse/zero-length поддержкой, проверкой SHA-256 сохранности и отдельными filesystem identities. Это не torrent completion и не fast resume.
- Перед cleanup исходный файл перемещается в принадлежащий операции quarantine. Повтор после unlink/flush/SQL failures проверяет обе стороны. Заново созданный по прежнему пути файл сохраняется вместе с retained claim. Неизвестные файлы и каталоги не удаляются рекурсивно.
- Проверены реальные reopen SQLite/движка до/после публикации, commit mapping, staging, копирования и cleanup; EXDEV, flush/SQL faults, изменившийся source, active transfers, cancel и stale versions. Повреждённые journal paths отклоняются до I/O с видимой ошибкой.
- Полный `make check` прошёл: `/tmp/tsh-phase42-block3-check.log` (macOS, root/fork/frontend, race/vet/build). Отдельный race move/queue/API прошёл; новый Vue test проверяет серверные actions и поздний ответ после смены record. Дополнительные `TestStorageJournal|TestStorageMoveDrainsTransfersAndCancelKeepsSource|TestDeletionCancel` прошли после разграничения отмены удаления/переноса.
- Native Linux/Windows и volume evidence проверяются в следующих блоках и итоговой приёмке; этот результат их не подменяет.

## Блок 4 — locate, inventory и тома

- Добавлены read-only анализ существующего каталога и явное применение. Проверяются v1 shared pieces, v2/hybrid, частичные/неверные/изменившиеся данные и zero-length. Анализ не импортирует `.part` и не создаёт пустые файлы. Apply повторно проверяет candidate evidence, атомарно меняет mapping/aliases и очищает прежнее file inventory, сохраняет старый каталог как retained ownership и оставляет paused intent. Reopen после commit проверен отдельно.
- Миграции `0017_location_analysis.sql` и `0018_storage_inventory.sql` добавляют компактный результат анализа, регистрацию томов, текущую сессию inventory и ожидания присутствия файлов. Результат fenced по record, storage revision, catalog path/size и сессии процесса. Порция — 128 файлов; очередь выбирает не более 64 записей, один worker. Непросмотренные записи остаются pending без writable admission, metadata-only discovery разрешено отдельно.
- Подтверждённое присутствие сохраняется отдельно от bytes/completion. Файл, который раньше не наблюдался, может отсутствовать без MissingFiles. Потеря ожидаемого файла закрывает admission до явного repair. Metadata/access/volume ошибки различаются; stats/inventory не предоставляют readiness. Проверены 257 файлов за три порции и отсутствие создания файлов при inspection.
- Volume registration использует случайный marker и SQLite enrollment journal. Текущая граница mount проверяется device/volume identity; она не сохраняется как идентификатор между reboot. Отсутствующий marker зарегистрированного тома не пересоздаётся автоматически. Старая установка требует явного reconcile, свежая пустая регистрируется при старте. Reconcile сохраняет паузу. Копия marker на другом физическом носителе неотличима от перенесённого тома — это явно ограниченное evidence, не доказательство hardware identity.
- Runtime file opens/writes, source/target/staging и destructive boundaries прежнего migration/recovery проверяют том. Staging нового move находится под родителем target на целевой ФС; старые schema-1 staging paths продолжают читаться. Зарегистрированные markers защищены от payload и не переносятся как обычные данные.
- GUI содержит анализ/подключение найденного каталога и reconcile. Полный affected suite usecase/repository/engine/HTTP прошёл; focused inventory/locate race прошёл. Дополнительные shared-boundary, zero-length, session fencing и locate reopen тесты прошли. При полном check исправлено копирование mutex в range по fileExtra; последующий полный check блока 5 прошёл.

## Блок 5 — состояния файлов и прогресс

- Миграция `0019_file_projection.sql` сохраняет полный metainfo storage path и компактную оценку free space плана. Display `path`, file index, offsets, старые bytes/progress и TorrServer wire сохраняются. Неизвестный старый metadata path не угадывается по названию: заполняется из проверенного catalog/inventory.
- Fork предоставляет cached `FileObservations` без stat/hash: текущие verified bytes, завершённые попытки проверки (включая несовпавшие pieces), очередь/активность и эффективный piece priority. V1 boundary bytes пересекаются с файлами, v2 alignment padding не входит в totals. Readiness остаётся повторно проверяемым gate реального открытия.
- Общая REST/SSE проекция объединяет WAL inventory с текущим attachment. Исторические bytes/source/time остаются видны при checking и detached runtime; не превращаются в verified bytes. Недоступный том, pending inventory, операции хранения и safety blocks снимают наблюдаемую direct readiness. Zero-length требует фактического inventory open, один metadata-only объект недостаточен.
- `selection` содержит выбранный размер, текущие verified bytes, процент, files, catalog/revision и complete. Сценарий одного готового файла из двух: selected 100%, whole 50%. Пустая выборка не имеет процента и не проецируется в Seeding; совместимый `selection_complete` по-прежнему снимает необходимость фонового download slot. Skip у готового файла не запрещает локальное чтение; DiskFull не отнимает подтверждённые read-only bytes.
- GUI показывает metadata/physical paths, presence/freshness/reasons, проверку, historical/current progress и effective priority. Кнопки file/archive download используют наблюдаемую серверную readiness, streaming неполного media остаётся отдельным действием. Compact SSE не повторяет каталог; details fenced по record/catalog/intent/mapping/attachment и инвалидируются при новых наблюдениях.
- REST list/details/pieces переведены на cached engine reads. Background reconciliation и реальные открытия по-прежнему обновляют disk evidence. Время продвижения verification меняется по счётчику выполненных bytes, а не heartbeat.
- Focused projection/shared-boundary/empty/Skip/volume/DiskFull тесты, affected Go suites, все 40 frontend tests и сборка прошли. Полный `make check` после блока 5 завершился успешно (macOS): root/fork tests/race/vet, frontend и application build; использован `/tmp/tsh-phase42-block3-check.log` с новым запуском. Это не native Linux/Windows evidence.

## Блок 6 — baseline измерений

`go test ./internal/usecase -run '^$' -bench '^BenchmarkVerificationStartup$' -benchtime=1x -count=3 -timeout=90s`: четыре торрента по 2 MiB, piece 64 KiB, SQLite completion, настоящий reopen, HTTP materialization compact-списка и reader последних 16 bytes. В callback каждого payload read добавлена фиксированная 1 ms задержка, чтобы воспроизвести конкурирующий backlog; это synthetic local macOS FS, не скорость NAS/HDD. Setup первой установки исключён из отдельных latency/counter показателей, но входит в стандартный `ns/op` Go benchmark, поэтому последний не используется для сравнения.

Baseline до изменения очереди (после добавления наблюдательных counters):

| Прогон | API, ms | Первый проверенный диапазон, ms | Вся проверка, ms | Payload reads, bytes | Payload opens / stat attempts | SQL row writes | Allocated bytes |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 2.830 | 469.1 | 476.1 | 8 388 624 | 261 / 3180 | 160 | 21 888 872 |
| 2 | 3.273 | 519.2 | 519.6 | 8 388 624 | 261 / 3184 | 160 | 21 785 104 |
| 3 | 2.520 | 481.9 | 482.0 | 8 388 624 | 261 / 3184 | 160 | 21 819 216 |

Максимум одновременно выполняющихся read callbacks — 2. I/O counters охватывают rooted payload storage (включая его containment lstat/fstat); операции каталога, volume marker и metainfo отдельно в эти counters не входят. SQL counters считают изменённые строки основных storage/intent/catalog/completion таблиц, не transactions/fsync. Allocated bytes — TotalAlloc за измеряемый запуск, не peak RSS. Первый первоначальный запуск benchmark завершился timeout из-за ошибочного барьера по effective download priority queued piece; барьер исправлен на active reader bitmap до всех трёх приведённых прогонов.

### Реализация и результат очереди

Основной и offline clients разделяют `HashBudget(2, 1, 64, 8)`: два hash tasks глобально, один на live ФС, 64 pending leases, обязательный FIFO turn после восьми приоритетных выдач. Основной client оставляет два прежних slots/один на торрент; ожидающий lease занимает этот slot. Piece bitmap объединяет повторные запросы. На каждой границе piece выбирается active reader, readahead, selected data, фон; oldest torrent и циклический background cursor обеспечивают продвижение при повторных seeks. Открытие resource directories и ожидание lease происходят вне library lock. Все части v1 boundary получают общий lease по contributing devices; root strings не считаются device identity.

Настоящий `OpenTorrent` вынесен из общего lock. Pending generation, проверка закрытия, join открытий и storage closures не позволяют позднему результату присоединить storage к удалённому torrent. `Drop` теперь ожидает syscall вне library lock; `Client.Close` дополнительно соединяет операции уже удалённых из map torrents до закрытия default backend. Deterministic тест именно этого сценария выявил и помог устранить deadlock прежнего `Drop`.

После изменений, та же команда/fixture (лог `/tmp/tsh-phase42-verification-after.log`):

| Прогон | API, ms | Первый проверенный диапазон, ms | Вся проверка, ms | Payload reads, bytes | Payload opens / stats | Resource opens / stats | SQL row writes | Allocated bytes |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 3.020 | 24.66 | 491.1 | 8 388 624 | 261 / 3156 | 128 / 512 | 160 | 22 192 920 |
| 2 | 2.481 | 24.52 | 447.1 | 8 388 624 | 261 / 3156 | 128 / 512 | 160 | 22 202 728 |
| 3 | 3.765 | 28.32 | 446.2 | 8 388 624 | 261 / 3156 | 128 / 512 | 160 | 21 992 968 |

Максимум hash tasks = 1 на этой ФС, pending leases = 1, read callbacks = 2 (reader + hasher). Range опередил backlog, весь проход остался примерно на уровне baseline. Lease ограничивает hash read/compute и освобождается до reacquire Client.lock; completion flush/SQL может пересекаться с проверкой другого piece. Промежуточный вариант удерживал lease до completion и занимал 988–1025 ms; он заменён при устранении lock cycle v2 layers, поэтому не является итоговым результатом. Добавилось 128 opens / 512 stats для live resource identity, а payload bytes и SQL rows не выросли. Это не доказательство ускорения HDD. Ни API latency, ни synthetic MB/s не имеют CI threshold.

Память: новый queue хранит ≤64 leases, число coroutine waiters ограничено существующими client slots; в production основной client ≤2 плюс один offline worker. На каждый ожидающий piece не добавляется goroutine/SQL row: очередь остаётся bitmap, goroutine создаётся только для выданного hasher slot. Размер `Piece` сохранён в прежнем лимите ≤296 bytes на 64-bit; обнаруженное regression +8 bytes исправлено компактным полем и размещением atomic flag в padding, без ослабления size test. Hash streaming buffer остаётся 128 KiB, mmap не добавлен. TotalAlloc вырос примерно на 0.2–0.4 MiB за fixture; это не peak RSS и не оценка большой библиотеки.

Прошли deterministic reader-before-backlog/verified-only, repeated seek/background cursor, shared budget across clients, cancellation/Drop, queue saturation, unrelated torrent during blocked Open/layer update и live-FS identity tests. Application race обнаружил обратный порядок locks при v2 layers: waiting flag стал atomic, lease освобождается до Client.lock, а layer update ожидает storage lock вне Client.lock и повторно проверяет closed/unchanged state. Linux дополнительно выявил starvation layer writer: pending batch теперь временно закрывает новые hash slots только своего торрента. Repeated v1/v2/hybrid locate test прошёл трижды; Linux scheduler tests — 20 раз. Полный fork и affected application race прошли. Предложение fast resume с flush/dirty/external-change ограничениями записано в контракте, реализация отключена.

## Блок 7 — upgrade и итоговая приёмка

- Миграции 0015–0019 проверены на настоящей SQLite фазы 41 с ready/legacy/blocked/moving, alias-группой, deletion/cancel_requested и сохранёнными alternatives. Повторное открытие сохраняет IDs/версии, manifest/journal bytes и deletion intent. Старая migration-14 rollback fixture продолжает проходить.
- Смешанная библиотека проверена отдельным тестом: один hash-root преобразуется в named layout; соседний hash-root и новый named torrent сохраняют mapping/bytes/paused intent и успешно открываются.
- Upgrade-only библиотека с одним interrupted journal может выполнить enrollment. Reconcile берёт общий mutation ticket, сохраняет deletion/recovery journal и не даёт attachment. Тесты deletion/legacy подтверждают сохранность данных и запретов; нормальный reconcile и intent fencing также проверены.
- Новые native path fixtures используют host separators. Обновлены README, phase42 wire contract, STATE/CONCURRENCY и совместимость phase41. Описаны naming, aliases/claims, retained data, enrollment/marker limitations, backup, upgrade/recovery rehearsal и downgrade через согласованный backup.
- Workflow `.github/workflows/checks.yml` расширен native names/volume, scheduler/open lifecycle и phase42 upgrade/recovery suites для macOS/Windows. Само изменение workflow не считается CI evidence.

### Platform evidence

| Среда | Проверка | Результат |
| --- | --- | --- |
| macOS arm64, Go 1.25.8 | Полный `make check`, включая root/fork race/vet, 40 frontend tests и build | PASS, exit 0, повтор после Linux writer fix; fork race 8.053 / 2.076 s |
| Linux arm64, Go 1.26.2, локальный Docker, CGO=0 | payload/repository/usecase upgrade/storage suite; scheduler/open/budget tests ×20; весь fork storage suite | PASS: 0.009 / 1.182 / 47.394 s; core ×20 0.175 s; storage 0.411 s |
| Linux: разные `/tmp` и `/dev/shm` | `TestNativeSeparateMountRenameAndVerificationResources` | PASS, не skip: boundary получает две FS identities, настоящий rename возвращает EXDEV и сохраняет источник |
| Windows amd64, Go 1.25.8, CGO=0 | `go test -c` fork storage и root usecase | Сборка PASS; **native запуск отсутствует** |

Linux запускал исходники/module cache read-only, без сети; test payload находился в Linux container FS, не в production downloads. Это native Linux filesystem execution, но без Linux race (CGO=0), не тест реального HDD/NAS. Нативный Windows runner здесь недоступен; Windows filesystem DoD остаётся открытым до запуска расширенного workflow на этой версии. Cross-compile не заменяет проверку Windows rename/flush/handles.

Пользователь сообщил: «Я сам подтвержу прохождение CI». Поэтому подтверждение native Windows остаётся ожидаемым внешним результатом; это сообщение само по себе не означает успешного прогона. Агент не публиковал ветку и не запускал GitHub Actions. Расширенные native suites остаются в обычном Checks workflow.

Команды для повторения (логи `/tmp` — вспомогательные, результаты сохранены здесь):

```sh
make check
go test -race ./internal/usecase ./internal/repository -run 'TestStorageEnrollment|TestStorageMixed|TestNamedStorageUpgrade|TestInventoryUpgrade' -count=1 -timeout=60s
go test ./internal/usecase -run '^$' -bench '^BenchmarkVerificationStartup$' -benchtime=1x -count=3 -timeout=90s
# Native Linux, from the repository root (used Go 1.26.2, CGO_ENABLED=0):
go test -p 2 -count=1 -timeout=180s ./internal/payload ./internal/repository ./internal/usecase -run 'TestStorage|TestNamed|TestMagnet|TestEnrolled|TestInventory|TestFileProjection|TestRecoveryUpgrade|TestLegacyRecovery|TestDeletionCancel|TestOwnership'
# From pkg/anacrolix-torrent:
go test -p 2 -count=20 -timeout=60s . -run 'TestVerification|TestBlockedStorageOpen|TestHashBudget|TestTypeSizes|TestPendingStorage'
go test -p 2 -v -count=1 -timeout=60s ./storage
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -c -o /tmp/tsh-storage-windows.test.exe ./storage
# From the repository root:
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -c -o /tmp/tsh-phase42-usecase-windows.test.exe ./internal/usecase
```

Final logs: `/tmp/tsh-phase42-block3-check.log` (имя переиспользовано), `/tmp/tsh-phase42-native-linux-final.log`, `/tmp/tsh-phase42-verification-after.log`. В Docker использовался уже установленный image `golang:1.26-alpine` (фактическая версия 1.26.2), `--network none`, четыре CPU/3 GiB, `GOPROXY=off`, `GOTOOLCHAIN=local`, `GOFLAGS=-mod=readonly`.

## Definition of Done

Блоки 1–7 реализованы; macOS `make check` и native Linux проверки завершены. В блоке 4 исправлено копирование mutex, в блоке 6 — lock cycles и starvation, найденные тестами; limits/assertions не ослаблялись. Проверки вне песочницы необходимы для Go cache и локальных sockets. Единственный открытый gate итоговой приёмки — native Windows.

- [x] F42-01: versioned mapping, один resolver и owner для aliases.
- [x] F42-02: понятные имена новых загрузок и metadata admission magnets.
- [x] F42-03: безопасные move/rename/conversion и повтор после сбоев (macOS/Linux; Windows gate отдельно).
- [x] F42-04: read-only locate и применение с сохранением предыдущих данных.
- [x] F42-05: inventory/volume recovery после рестарта; ограничения marker и platform evidence описаны выше.
- [x] F42-06: состояния файлов, selected/whole/verification progress API/GUI.
- [x] F42-07: ограниченная очередь проверки, приоритет стриминга, измерения.
- [x] F42-08: upgrade/journal compatibility, retained ownership и документация.
- [x] Regression гарантий предыдущих фаз.
- [ ] Финальный make check и native Linux/macOS/Windows suites.
- [x] README/wire/state/concurrency contracts и итоговый отчёт.

## Ограничения и отклонения

Native Windows недоступен в этой среде; вместо него получена только cross-compilation и подготовлен CI workflow. Это предусмотренный планом незакрытый пункт, а не успешная platform-приёмка. Следующая фаза не начиналась. Рабочий NAS и пользовательские загрузки не использовались.

Прочие границы: измерение synthetic local FS, не HDD/NAS; TotalAlloc не peak RSS. Linux прогон использовал установленный Go 1.26.2/CGO=0, macOS — проектный Go 1.25.8 с race; Windows CI должен отдельно подтвердить свои FS semantics. Manifest ограничен 65 536 entries; durable JSON сохраняется пакетами, но суммарный объём переписывания большого journal не заявляется линейным. Marker не доказывает отсутствие клонирования/внешних правок; corrupted initial enrollment требует offline inspection/backup. Весь accepted storage API отвечает 202, включая идемпотентный повтор и reconcile; контракт уточнён по реализации.

## Исправление Linux CI: отмена SQLite-запроса при shutdown (2026-10-25)

В присланном CI-стеке `runLocationOperations` падает в `PendingStorageOperations` после сообщения `closing torrent engine`. Причина воспроизведена в закреплённом `modernc.org/sqlite v1.21.1`: отмена контекста может остановить цикл `stmt.query` до создания `driver.Rows`, после чего драйвер возвращает `(nil, nil)`. `database/sql` принимает результат как успешный и падает при обращении к внутреннему `rowsi` из `Rows.Next` и затем `Rows.Close`. Проверка `rows == nil` на уровне repository не защищает от этого: внешний `*sql.Rows` уже создан. `Engine.Close` отменяет задачи и ждёт их через `wg`; менять этот порядок или отключать cancellation не требуется.

Что изменено:

- `go.mod`, `go.sum`: SQLite-драйвер обновлён до `v1.40.1`, `modernc.org/libc` — до требуемой им `v1.66.10`; выполнен `go mod tidy`. Версия поддерживает проектный Go 1.25.8. В исходнике драйвера проверены возврат ошибки отмены вместо nil rows и закрытие уже созданных rows при отмене. Последнее предотвращает удержание read transaction и блокировку WAL checkpoint; [upstream MR !81](https://gitlab.com/cznic/sqlite/-/merge_requests/81).
- `internal/repository/sqlite_test.go`: тест вызывает настоящий `driver.QueryerContext` через `sql.Conn.Raw` с отменённым контекстом, моделируя отмену после проверки контекста в `database/sql`. Проверяет ошибку отмены и последующую работу соединения. На старой зависимости тест падает; отдельный диагностический цикл получил `(nil, nil)` на итерации 3841.
- `internal/repository/storage_operations_test.go`: по 1000 конкурентных отмен для пустой и непустой очереди; после каждой отмены выполняется обычный запрос. После серии проверяются сохранность очереди, запись и `wal_checkpoint(TRUNCATE)`. Используется один слот пула. Помимо `context.Canceled`, для отменённого запроса допустим `SQLITE_INTERRUPT`: отмена во время `Rows.Next` может проявиться через SQLite раньше, чем через `database/sql`. Ошибка следующего запроса всегда считается сбоем.

Проверки:

- [x] Старый драйвер: воспроизведён недопустимый результат `(nil, nil)`; regression-тест отмены падает.
- [x] Новый драйвер: начальная проверка SQLite/cancellation с `-race` на macOS проходит.
- [x] Linux arm64: SQLite/cancellation suite прошёл 20 раз; API suite прошёл полностью три раза. Исполнялись статические тестовые бинарники, собранные **Go 1.25.8**, `CGO_ENABLED=0`, в локальном контейнере `golang:1.26-alpine` без сети, с read-only mount только тестовых бинарников. Версия Go в имени контейнера не является версией сборки тестов.
- [x] Windows amd64: API test binary собран с Go 1.25.8 и `CGO_ENABLED=0`.
- [x] Финальный `make check` на macOS arm64 / Go 1.25.8: exit 0; root/fork tests и race, vet, 40 frontend tests, frontend/backend build. Root API race — 16.484 s, repository — 73.182 s, usecase — 183.816 s. Окончательная версия SQLite/cancellation-тестов дополнительно прошла с `-race -count=1 -timeout=90s` за 45.871 s.

В первом Linux stress-прогоне тест ошибочно ожидал только `context.Canceled` и получил `SQLITE_INTERRUPT`. Проверка уточнена по [контракту SQLite](https://www.sqlite.org/rescode.html#interrupt) и усилена обычным запросом после каждой отмены; повторные 20 прогонов прошли. Production workaround, `recover`, отключение отмены и изменение миграций не добавлялись.

Это исправление регрессии текущей фазы, следующая фаза не начиналась. Локальный Linux прогон без race и Windows cross-compile не заменяют native CI; прежний gate native Windows остаётся открытым. Логи полного check/API: `/tmp/tsh-sqlite-cancel-check.log`, `/tmp/tsh-sqlite-cancel-linux-api.log`.

## Исправление Windows CI: storage move fixtures (2026-10-25)

Пользователь прислал падение `TestStorageMoveEXDEVAndPostRenameFlushRetry/exdev`: fixture возвращала `syscall.EXDEV`, но Windows rename возвращает `ERROR_NOT_SAME_DEVICE` (17). Production classifier корректно различает платформы; ошибка была в инъекции теста.

- `internal/usecase/storage_move_test.go`: инъекция использует native errno для текущей ОС и оборачивает его в `os.LinkError`, дополнительно проверяя распознавание вложенной ошибки реального rename. Проверки завершения операции, fallback в copy и сохранности байтов оставлены без ослабления.
- В соседнем `TestStorageMoveSQLFailureAfterPublication` исправлен путь SQL-триггера: `filepath.FromSlash` вместо Unix-only литерала, чтобы fault injection срабатывала и на Windows.
- [x] Оба affected теста прошли на macOS с `-count=3`.
- [x] Полный `go test ./...`, `go vet ./...`, `go build ./...`, `git diff --check` прошли.
- [x] Windows amd64 / CGO=0: usecase test binary собран.
- [ ] Повторный native Windows CI; итоговый Windows DoD остаётся открытым. Production-код и архитектура не менялись, следующая фаза не начиналась.

## Исправление Windows CI: cleanup непустого source (2026-10-25)

Следующий присланный Windows failure — `TestStorageMoveCleanupPreservesRecreatedSource`: после корректного сохранения заново созданного файла cleanup ошибочно завершался с `target_exists`. Причина в production-коде: проверялся только Unix `syscall.ENOTEMPTY`, а Windows `os.Root.Remove` возвращает обёрнутый `ERROR_DIR_NOT_EMPTY` (145).

- `internal/usecase/storage_move.go`: единый platform-aware `isDirectoryNotEmpty` применяется при удалении quarantine, вложенных source directories и source root. Непустые каталоги сохраняются; permission и прочие ошибки по-прежнему останавливают cleanup. Рекурсивное удаление не добавлялось.
- `internal/usecase/storage_move_test.go`: новый тест проверяет настоящий `os.Root.Remove` непустого каталога на текущей ОС и отклонение посторонних ошибок, включая wrapped permission error. Существующий integration-тест сохраняет проверки новых внешних байтов, перенесённых исходных байтов и retained ownership.
- [x] `go test ./...`, `go vet ./...`, `go build ./...` прошли на macOS.
- [x] Оба affected теста прошли с `-race -count=3`.
- [x] Windows amd64 / CGO=0: usecase test binary собран.
- [ ] Native Windows CI после исправления; итоговый Windows DoD остаётся открытым. Это исправление текущей фазы, следующая фаза не начиналась.
