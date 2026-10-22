# Фаза 36: Восстановление Приоритетов И Надёжный DiskFull — Отчёт

Дата: 2026-10-22. Основание: `tasks/phase36-restore-priorities-and-diskfull.md`, F07/F08 аудита. Перед началом проверен закрытый DoD фазы 35. Фаза 36 реализована и проверена; следующие этапы не начинались. Commit/push не выполнялись.

## Что Сделано

### F07: Восстановление Пользовательского Намерения

- `internal/engine/restore.go`, `engine.go`: общий staged restore для startup, DB-only Resume и восстановления после storage migration. `RestoreTorrent` проверяет соответствие source/infohash, устанавливает library-level запрет загрузки, копирует сохранённые file priorities до регистрации runtime и допуска scheduler. `FinishRestore` допускает активацию только после завершения прикладного сохранения; existing-runtime Resume сначала закрывает admission и перечитывает настройки из БД.
- `internal/usecase/torrent_uc.go`, `storage_migration.go`: startup/Resume используют общий restore workflow. Сохранение runtime-defaults сразу после восстановления и игнорирование ошибок приоритетов удалены. Ошибка БД при Resume возвращается вызывающему коду, runtime остаётся недоступным для payload download. Paused после рестарта остаётся Paused; DiskFull не становится автоматически активным.
- `internal/engine/priority.go`, `metainfo.go`: отложенные Normal/High/Skip применяются после появления metadata до первого Allow. Проверяются значения и индексы; ошибка не подменяется Normal. Ошибка сохранения metainfo также оставляет запрет, диагностируемый Error и необходимость исправления/Resume. Общий gate повторно проверяет metadata persistence перед допуском.
- `internal/repository/torrent_repo.go`: statistics/snapshot upsert сохраняет downloaded, но **не обновляет priority существующих файлов**. Пустой snapshot до metadata не удаляет файловые записи. Приоритеты обновляются отдельными явными операциями, SQLite commit предшествует изменению runtime. Bulk priority для известных сохранённых файлов работает и при metadata-pending; для неизвестного списка возвращается ошибка, не ложный успех.
- Повторный add DB-only записи консервативно отклоняется: для неё необходим Resume через staged restore, а не загрузка с defaults. Re-add записи с disk latch также не является способом снять запрет. Это узкое ограничение admission для сохранения ранее заданного намерения, без редизайна очереди.

### F08: Единый Payload Gate И Sticky DiskFull

- `internal/engine/download_gate.go`, `resource_manager.go`: единственный engine Allow находится в `admitPayloadLocked`, под `admissionMu`. Gate учитывает initialization, inactive states, сохранённый block, свободное место, metadata persistence и синхронную ошибку library writer. Pause/Resume, metadata admission, scheduler и Delete сериализованы с gate; watchdog/discovery больше не вызывает безусловный Allow.
- Проверяется filesystem фактического mapped payload root и существующих родителей metainfo-файлов (включая отдельные вложенные filesystem). Для ещё не созданного каталога используется ближайший существующий родитель. Повторные пути дедуплицируются; порог безопасен к переполнению и допускает ровно настроенный резерв. Ошибка Statfs не означает достаточное место и имеет отдельную причину `disk space unknown`.
- Нехватка места блокирует active, Queued и stream-driven download, не меняя base priorities. Manually Paused не активируются монитором. Освобождение места само не открывает gate. Счётчик фоновых загрузок по-прежнему ограничивает обычный Resume; queued stream overlay не включает всю base selection как дополнительную фоновую загрузку.
- `internal/repository/migrations/0006_download_blocks.sql`, `download_blocks.go`: независимый от snapshots durable `torrent_download_blocks`. Старый StateDiskFull импортируется в latch. Причина сохраняется непосредственно при переходе, а не только SyncWorker; сохранённый latch загружается при restore. Только явный Resume после успешной проверки диска и сохранения очищает его. Ошибка persist оставляет runtime gate закрытым; монитор повторяет неудавшееся сохранение доступного ему blocked-runtime.
- `internal/engine/stream_source.go`, `stream.go`, `engine.go`: запрос недостающих данных проверяет gate перед reader/preload/overlay. Reader context связан с поколением payload admission: disk transition завершает ожидающее чтение с причиной, без бесконечного ожидания. Warmup использует тот же lifecycle. Verified direct stream/download не требует payload admission. Disk gate не запрещает upload проверенных pieces и не сбрасывает QoS refcount/debounce ради возобновления.

### Необходимые Исправления Форка

- `pkg/anacrolix-torrent/client.go`: обнаружено, что `AddTorrentOpts.DisallowDataDownload`/`DisallowDataUpload` существовали, но не применялись. Теперь начальные запреты установлены **до metadata/discovery**. Без этого staged application restore не имел доказанного запрета первых запросов.
- `pkg/anacrolix-torrent/torrent.go`: write error закрывает admission синхронно под client lock даже с пользовательским async observer. Ошибка защёлкивается; `AllowDataDownloadIfNoWriteError` атомарно не допускает автоматического открытия. Явный Resume проверяет поколение writer errors: новая ошибка во время disk probe не стирается. Запоздалый application observer читает текущий latch, а не восстанавливает уже обработанную старую ошибку после успешного Resume.
- Методы чтения текущего запрета/ошибки используются для admission и regression assertions. Payload mmap не добавлялся; механизм verified readiness фазы 35 сохранён.

### API, UI И Документация

- `internal/models/torrent.go`, `health.go`, repository: `download_block` в REST/SSE, независимо от state и обычных статистических снимков. Manually Paused может одновременно иметь disk block; UI показывает обе причины.
- `internal/delivery/http/api/handlers.go`: Resume с disk gate — 409 с причиной, missing — 404, ошибка БД — 500; ложный успех не возвращается. Небезопасный re-add — конфликт.
- `internal/delivery/http/torrserver/handlers.go`: открытие stream с блокировкой недостающих данных — диагностируемый 409. CORS `*`, endpoint names и TorrServer request formats не менялись.
- `web/src/types/index.ts`, `TorrentTable.vue`, `web/tests/TorrentTable.test.ts`: desktop/mobile показывают block и необходимость ручного Resume отдельно от Paused/error.
- `README.md`: startup priorities, sticky DiskFull, ручной Resume, поведение streams, отдельное сохранение latch и пределы гарантии для in-flight/service writes.

## Тестирование

Добавлены:

- `internal/engine/download_gate_test.go`: metadata-pending restore с High/Skip до admission; low-space latch, threshold boundary, unknown disk state; Queued/active запрет и лимит Resume; persistence failure и управляемый persistence barrier; конкурентные scheduler/discovery/admission callbacks после ENOSPC; сохранение refcount, отмена missing-data reader; late observer после Delete; Close во время staged restore; verified direct read при DiskFull; проверка payload subdirectories и metadata persistence failure.
- `internal/usecase/restore_priorities_test.go`: реальное закрытие/повторное открытие SQLite и engine, Paused + Normal/High/Skip, сохранённый block, manual/repeated Resume; SQLite trigger failure при Resume; DB-only metadata-pending и SyncWorker до metadata; восстановление active disk latch, запрет обхода duplicate add.
- `internal/repository/download_blocks_test.go`: snapshots с Normal/default/пустыми файлами не стирают Skip и latch; reopen; upgrade pre-0006 StateDiskFull.
- `internal/delivery/http/api/download_gate_test.go`: 409 при недостаточном резерве, 404 для отсутствующего hash, 500 при настоящей SQLite transaction failure; запрет загрузки после ошибок.
- `pkg/anacrolix-torrent/download_write_error_test.go`: storage Piece writer возвращает `syscall.ENOSPC`; write/error handling и синхронный gate проверяются с заблокированным async observer. Проверяются сохранение upload admission, reader non-bypass, автоматический Allow и новые поколения ошибок во время Resume. Отдельно — применение начальных download/upload options.
- Frontend: отдельный тест block diagnostics в обоих layouts, без подмены Paused статусом DiskFull.

Успешные команды:

```sh
# Корень проекта:
go test -count=1 -timeout 90s ./...
go test -race -count=1 -timeout 90s ./internal/... ./cmd/...
go vet ./...
go build -o <approved-temporary-directory>/phase36-hub ./cmd/hub
go test -race -count=5 -run 'TestDownloadGate|TestRestoredPriorities|TestQueuedTorrent|TestDiskProbe|TestMetadataPersistence' -timeout 90s ./internal/engine
# pkg/anacrolix-torrent, без исключений:
go test -count=1 -timeout 90s . ./storage
go test -race -count=1 -timeout 90s . ./storage
go test -race -count=5 -run 'TestWriteError|TestInitialAdmission' -timeout 60s .
# web:
npm test
npm run build
# Корень проекта:
git diff --check
```

Логи в approved temporary directory `.../T/opencode/`: `phase36-final-tests.log`, `phase36-final-race.log`, `phase36-final-fork.log`, `phase36-final-fork-race.log`, `phase36-final-gate-repeat.log`, `phase36-writer-repeat.log`. macOS linker предупреждает `malformed LC_DYSYMTAB`; data races не обнаружены. Frontend: 4 теста проходят.

## Ограничения И Отклонения

1. Нет полного actor/FSM или queue redesign. Сохранены основной state и пользовательские priorities; добавлена отдельная durable latch table. `admissionMu` используется только для необходимой сериализации admission/disk/restore, а не для всех QoS overlays F15.
2. Fault injection проверяет `syscall.ENOSPC` в storage writer и вызов error handler под library lock. Это не физическое заполнение диска и не полный peer-to-peer transfer с отказом настоящего volume. SQLite failures проверяются настоящими triggers; metadata persistence failure — невалидным config parent; физический readonly volume и реальный power-cut не проверялись.
3. Уже начатый write/fsync/Statfs не может быть надёжно отменён приложением. Gate прекращает новые запросы и отменяет pending requests; ограниченные in-flight операции могут завершиться/ошибиться. Программные Close/reader сценарии проверены, но зависший NAS syscall не обещается прервать по таймеру.
4. На полностью заполненном/readonly диске могут не сохраняться SQLite latch, metadata, completion и logs. Ошибка persist не открывает runtime gate; startup всегда повторно проверяет disk перед payload. Гарантия durable latch действует при успешном SQLite commit, не при неспособном записать filesystem. Между асинхронным observer и durable commit существует crash window; синхронный library gate защищает работающий процесс, а не обещает невозможную атомарность filesystem error + SQLite.
5. Raw library readers при запрете могут читать доступные verified bytes, а при запросе отсутствующих получают ошибку запрета. HTTP disk fast path отдельно проверен при DiskFull. Прерванный missing-data stream требуется открыть заново после Resume; refcount/debounce не используются как средство сброса latch.
6. Полный optional upstream набор (libpossum/FUSE) в этой фазе повторно не объявляется проверенным: действуют ограничения окружения из отчёта 35. Затронутые root/storage пакеты проверены полностью, без `-skip`. NAS/Smart TV/Windows/Linux runtime не проверялись. Dependency upgrades, auth и прочие находки аудита не входили в работу.

## Definition Of Done

- [x] F07: общий startup/Resume restore, base priorities до payload admission, metadata pending и restart.
- [x] SyncWorker/DiskFull/QoS не заменяют пользовательские priorities runtime defaults/overlays.
- [x] F08: library-level gate для reader/overlay/background, ENOSPC и unknown disk state.
- [x] Все engine Allow paths сведены к gate; concurrency/barrier regression-тесты проходят.
- [x] Sticky block переживает restart; free space без Resume не включает download; Resume сохраняет priorities и лимит.
- [x] API/UI показывают причины без ложного успеха; verified disk read не требует payload admission.
- [x] Тесты/race/vet/build успешны; пределы in-flight/service writes описаны.
- [x] README и этот отчёт обновлены.
