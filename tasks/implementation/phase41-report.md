# Фаза 41 — отчёт об имплементации

Статус: **завершена; DoD закрыт**. Локальный `make check` и полные native Linux suites прошли; успешный Windows CI подтверждён пользователем. Cross-compile Windows отдельно не считается исполнением. Фаза 42 не начата.

Основание: [план фазы](../phase41-storage-recovery-and-migration-observability.md) и команды пользователя начать фазу и продолжать до конца без промежуточных остановок. Пользовательский LAN-инстанс, контейнер и payload не изменялись. Изначальные изменения документов фазы 40 сохранены.

## Закрытие приёмки

Пользователь сообщил: «CI windows прошел». Этим закрыт последний открытый пункт native Windows acceptance. Источник результата — подтверждение пользователя; ссылка на run, commit и отдельные логи не предоставлены, агент самостоятельно CI не проверял. Это подтверждение относится к CI, не к установке на пользовательский HDD или проверке рабочего инстанса.

## Результат по задачам

| ID | Реализация и проверка |
| --- | --- |
| F41-03 | Coherent ownership snapshot, отдельная catalog revision, bounded/cancelable retry, path reservations до последнего filesystem effect. Add, включая существующий orphan alias, проверяет reservations. Удалённые legacy keep-data записи сохраняют ownership известных путей через оставшийся metainfo/mapping. Late migration writes fenced по record identity. |
| F41-02 | Durable deletion stages, retry исходного mode/manifest, явные request/join/complete cancellation, partial/unknown status, paused intent. Ошибочный противоположный mode даёт typed 409 и не заменяет предыдущую причину unlink failure. Старый worker проверяет cancellation перед следующим unlink; final SQL delete также отвергает cancel requested. |
| F41-01 | Явный локальный анализ кандидатов, v1/v2/hybrid verifier, неоднозначность без выбора по размеру, explicit whole-file choice. Durable prepared/preserved/published/verified journal, protected alternative, rooted native cross-directory no-replace move, fingerprint/identity/metainfo membership checks, restart/retry. |
| F41-04 | Additive Go/TS JSON contract, typed attachment/conflict errors, список/детали/full/compact SSE, GUI. Layout ready не подменяет attachment; typed importer conflict подтверждает metadata known даже без runtime. Recovery UI объясняет отмену и требует подтверждения; обычные Resume/opposite delete не предлагаются при pending deletion. |
| F41-05 | Queue/stage/file byte counters, независимые heartbeat/last-progress, latest-only публикация. Sampling не читает payload и не пишет journal. Physical safety rereads считаются внутри этапа; неизвестные totals опускаются, ETA/глобальный процент не выдумываются. Повторные migration failures rate-limited с repeat count. |
| F41-06 | Проверены README, Compose и Makefile examples; 15 неработающих env options перечислены для удаления без замены. Recycle/hard refresh не возвращались. |

## Основные изменения

- `internal/repository/migrations/0014_storage_recovery.sql`: ownership catalog version и durable recovery table с record/operation identity, reason, stage, исходным deletion mode, removed-data uncertainty, failure code, journal. Начало deletion и recovery row атомарны; существующие pending manifests импортируются как **unknown**, без сброса mode/paths. WAL/pool не менялись. Старые storage migration journals не переписываются.
- `internal/repository/ownership.go`: один короткий WAL snapshot identity/catalog/mapping/cleanup; metainfo и filesystem вне SQL transaction. `storage_fenced.go` не позволяет позднему результату изменить mapping повторно добавленного hash.
- `internal/usecase/ownership.go`, `temporary_files.go`, `storage_migration.go`: до 16 отменяемых попыток с backoff, path claims, проверка Add/alias publication. Общий admission mutex используется только для короткой registry/SQL publication, не для hash/copy/stat. Migration work ticket сохраняет последовательную HDD-политику; ownership retry будит этот же worker.
- `internal/usecase/deletion_recovery.go`: retry/cancel/forget с ожидаемыми record ID и operation version. Отмена HTTP wait не стирает cancellation; startup завершает её до attachment. Cancelled legacy запись сама не мигрирует/не подключается; explicit verified resume использует тот же migration coordinator и ownership gates.
- `internal/usecase/legacy_recovery.go`: bounded engine-owned heavy recovery task, streamed fingerprints, OS file identity, библиотечная проверка shared v1 boundary pieces и v2/hybrid. Analysis/verification advance operation version; apply retry сохраняет зафиксированный choice и backup destination. После apply payload остаётся paused, runtime не подключается автоматически.
- `pkg/anacrolix-torrent/storage/`: typed `LegacyPartConflictError`; rooted read-only analysis без zero-file creation/импорта/rename; in-memory completion; observational read callbacks. Native Linux/macOS/Windows no-replace move между directory handles, flush/retry support, stable file identity. Нет copy/delete fallback при неподдерживаемой публикации. Существующие ordinary-name, rooted и current-open verification gates сохранены.
- `internal/engine/verify_storage.go`, `cleanup.go`: offline verifier без сети, один hasher; protected `.tsh-recovery`; typed insufficient-space failure. Sticky payload DiskFull не снимается анализом или применением файла.
- `storage_diagnostics.go`, `storage_progress.go`, REST handlers и Vue `StorageRecoveryPanel.vue`: согласованная диагностика и действия, подтверждение частичной отмены/выбора кандидата, retained-copy logical bytes. Версия операции остаётся десятичной строкой, включая значения выше JS safe integer.
- README, `STATE.md`, `CONCURRENCY.md`, [wire contract](../phase41-recovery-contract.md): API, cancellation, retained copies, verification, upgrade rehearsal и downgrade restrictions. Native CI job расширен repository/usecase recovery tests.

## API и persistent compatibility

Общий объект `storage_recovery` используется в REST/SSE. Старые `error`, `storage_error`, `deletion_error` сохранены. Новый `GET /api/v1/torrent/{hash}` возвращает полные детали.

Команды: `POST /api/v1/torrent/{hash}/action`, `action`, `record_id`, `version` (строка); для `apply_legacy` — `choice=final|part`. Retry/cancel/forget deletion возвращают 200 после завершения; heavy analyze/apply/resume verification и scheduled ownership retry — 202, результат через REST/SSE. Неожиданные DB/I/O failures не переводятся безусловно в conflict.

Альтернатива хранится в `<downloads>/.tsh-recovery/<record_id>/<version>/alternative` на том же filesystem. Обычные Resume/delete/temporary cleanup её не удаляют. Retention ledger сохраняется при последующем deletion intent. После удаления записи backup остаётся защищённым; автоматической garbage collection нет.

Upgrade regression строит настоящую БД со схемой 0001–0013, сохраняет ready/legacy/blocked/moving и pending deletion, затем применяет 0014/reopen. Проверены неизменность identity/manifests и unknown unlink progress. Отдельный тест намеренно ломает середину migration 0014, подтверждает SQL rollback новых ownership objects и успешный повтор после устранения injected failure.

Downgrade выполняется восстановлением согласованной pre-upgrade копии payload/metainfo/SQLite, не удалением строки migration из обновлённой БД.

## Детерминированные регрессии

- F41-03 сначала воспроизведён как stale list → `CleanupPaths(B)` → `sql.ErrNoRows`. После исправления barrier между coherent snapshot и validation удаляет B; проверяется безопасный retry, а не игнорирование ErrNoRows. Есть Delete/re-add, overlapping mappings, existing aliases, bounded churn/cancellation и late fenced writes.
- Во время заблокированного настоящего hash reader migration A публикуется SyncWorker snapshot с неизменным last-progress и новым heartbeat. Реальные публичные Delete/re-add/Pause B выполняются, B получает новую identity; A не становится permanent blocked. Прямой worker тестируется без запуска конкурирующего background pass.
- Pending deletion до первого unlink, после удаления части имён, request disconnect, повтор cancel, old manifest uncertainty, запрет revival. После join remaining sidecar сохранён, количество unlink не растёт. Explicit verified resume отменённой legacy записи отдельно проверен.
- Equal/different candidates, одна повреждённая, разные incomplete piece sets без merge, v1 boundary с существующим/отсутствующим соседом, v2/hybrid, zero-length. Настоящие metadata names с `.part` покрыты fork suite.
- Прерывание после prepared journal, перемещения альтернативы, публикации selection и verification. Reopen реального SQLite pool и новая usecase продолжают journal без новых backup copies. Это контролируемые restart boundaries, не модель power-loss hardware.
- Changed candidate, symlink/hardlink, target exists, disk-space failure, flush failure **после выполненного move**. Кандидаты/foreign target не перезаписываются; retry восстанавливается по journal. После изменения до первого move доступен повторный анализ с новой operation version.
- Go/TS round-trip fixture закрепляет reason/actions, exact JSON names, decimal versions, unknown/zero totals, independent clocks, analysis/evidence, alternatives и failure/repeat diagnostics. GUI tests проверяют подтверждение, invalidation по версии, недоступность обычного/opposite delete и возможность cancellation при ожидающем original Delete.

## Проверки и точные команды

Все логи ниже находятся в `/private/var/folders/zg/l0438jl160s4wr89pw16vh4c0000gn/T/opencode/`.

| Среда / команда | Результат |
| --- | --- |
| Baseline, macOS arm64, Go 1.25.8 / Node 22.15.0: `make check` | Успешно; 28 исходных frontend tests. `phase41-baseline.log`. |
| Блоки 2–3: `go test -race ./internal/repository ./internal/usecase` | Успешно. `phase41-block23.log`. |
| Финальная функциональная проверка: `go test -race ./internal/repository ./internal/usecase ./internal/delivery/http/api` | Успешно. `phase41-functional-race.log`. |
| Stress: `go test -race -count=3 ./internal/usecase -run 'TestStorageBlockedIO\|TestLegacyRecovery\|TestDeletionCancel\|TestOwnership'` | Успешно после корректировки единственного владельца migration в тестовом fixture. `phase41-lifecycle-stress.log`. |
| Финальный macOS `make check` | Успешно: root tests/race/vet/build, fork `.`/`./storage` tests/race/vet, frontend 35 tests/build. `phase41-acceptance.log`. |
| Native Linux/arm64, два CPU, Go 1.25.8: `go test -race -count=1 -timeout=240s ./...`; fork: `go test -race -count=1 -timeout=240s . ./storage`; оба `go vet` | Успешно на окончательном коде. `phase41-linux-acceptance.log`. |
| `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -c -o <temp>/phase41-usecase-windows.test.exe ./internal/usecase` | Успешно; **только cross-compile**. |
| Fork: `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -c -o <temp>/phase41-storage-windows.test.exe ./storage` | Успешно; **только cross-compile**. |
| Windows CI | Успешно по подтверждению пользователя «CI windows прошел»; run и логи агентом не проверялись. |
| `git diff --check` | Успешно. |

Linux выполнен локальным Docker, не GitHub runner и не пользовательским NAS. Образ `golang:1.26-alpine` использован с **GOTOOLCHAIN=go1.25.8**, GOMAXPROCS=2, `--cpus 2`, readonly mount исходников; test payload создаётся в ephemeral filesystem контейнера. Установлены gcc/g++/musl-dev для race и go-libutp. Начальный запуск без g++ не собрал go-libutp; после добавления C++ compiler полный прогон прошёл. Зависимости проекта не обновлялись.

Новый GitHub CI run агентом не запускался/не проверялся. `gh` отсутствует; native Windows среды в текущем harness нет. Расширенный `Native storage (windows-latest)` job подготовлен в `.github/workflows/checks.yml`; успешный Windows CI впоследствии подтверждён пользователем, как зафиксировано выше. Предыдущая приёмка CI фазы 40 не использовалась вместо этого подтверждения.

В macOS race linker остаётся `LC_DYSYMTAB` warning при exit 0. Первый новый Vitest fixture использовал преобразованный jsdom `import.meta.url`; путь исправлен. Некорректный тестовый single-file hybrid fixture заменён согласованным v1/v2 multi-file fixture; это не объявляется исправлением библиотечного verifier.

## Измерение «много мелких + один крупный»

Команда: `go test -v ./internal/usecase -run 'TestLegacyRecovery|TestStorageProgress|TestMigrationObservedIOProfile|TestDeletionCancel'`, лог `phase41-profile.log`. Apple M3 Max, macOS arm64, Go 1.25.8, локальная временная ФС. **Это не HDD/NAS benchmark**; скорость не используется как CI-порог.

Fixture: 128 файлов по 1 байту + файл 1 MiB; всего 129 файлов, payload **1 048 704 B**. Проверены actual I/O callbacks и SQLite trigger counter.

| Этап | Физически прочитано | Наблюдавшееся время |
| --- | ---: | ---: |
| preflight | 0 payload B | 14 ms |
| checksum | 1 048 704 B | 9 ms |
| copy + проверка копий | 2 097 408 B | 1.447 s |
| offline verification | 2 097 408 B | 2.560 s |
| cleanup safety checks | 2 097 408 B | 42 ms |

Итого **7 340 928 B**, **9 journal updates**, wall time **4.072 s**. Повторные чтения — существующие safety checks/verification, не GUI hash passes. Счётчики обновляются в уже выполняемых reads; публикация не добавляет полного чтения или per-chunk persistence. Неизвестные totals старых journals/повторных проходов не выдаются за нулевой объём. Тест проверяет линейный batch journal budget, не длительность диска.

## Definition of Done

- [x] F41-03 воспроизведён и исправлен безопасным snapshot/retry, без ложного permanent blocked.
- [x] Add/alias/re-add не обходят path reservations; новые recovery requests и поздние writes fenced по identity/version.
- [x] F41-02: retry/cancel через API/GUI, durable restart/disconnect, typed mismatch 409.
- [x] После завершённого cancel нет дальнейших unlink/delete старым worker; partial/unknown и explicit verification сохранены.
- [x] F41-01: analysis/apply через journal, альтернатива сохранена, неоднозначность не разрешается по имени/размеру.
- [x] Проверенные crash/flush/target/changed/link/DiskFull boundaries повторяемы; ordinary names/rooted/no-replace gates сохранены.
- [x] F41-04: REST/SSE/GUI показывают attachment errors независимо от layout и metadata readiness.
- [x] F41-05: очередь, этапы, bytes/current-file и независимый heartbeat при blocked read.
- [x] Нет GUI full reads/per-chunk persistence; I/O/journal measurements приведены выше.
- [x] Upgrade/reopen/rollback/retry сохраняют старые records/manifests; root/fork regression suites прошли.
- [x] F41-06: примеры и список удаления legacy options актуальны.
- [x] Все native storage checks Linux/macOS/Windows подтверждены: Linux/macOS — локальными результатами, Windows CI — сообщением пользователя. Локальный `make check` прошёл.
- [x] README, state/concurrency/wire contracts и этот отчёт обновлены.

## Ограничения и отклонения

- Прежний внешний блокер native Windows CI закрыт подтверждением пользователя, не cross-build. Самостоятельной проверки run/логов агентом не было; push/commit/deploy агентом не выполнялись.
- HDD пользователя не измерялся и не менялся. Вместо обещания ускорения записан воспроизводимый локальный stage/I/O/journal profile. Приёмка на рабочем инстансе должна быть отдельной операцией после rehearsal на копии.
- Heartbeat реализован существующим независимым двухсекундным sampler, не дополнительным worker на операцию. Это сохраняет ресурсные бюджеты; heartbeat не объявляется продвижением syscall.
- Анализ всего torrent для каждого whole-file кандидата консервативен и может быть дорогим на HDD, но выполняется только явной ограниченной операцией, не startup/Resume. Скорость/ETA не обещаются.
- Retained-copy bytes — логическая длина файлов; sparse/compressed allocation может отличаться. Нет автоматического backup deletion. Повтор после OS flush error исследует имена/journal; никакая отмена не выдаётся за мгновенное завершение syscall.
- Моделируются контролируемые границы сбоя и reopen настоящего storage/SQLite; реальные power-loss guarantees и физическая надёжность диска не заявляются.

Дальнейшая разработка фазы 42 не начиналась. Открытых пунктов DoD фазы 41 не осталось; проверка и установка на рабочий инстанс остаются отдельной операцией.
