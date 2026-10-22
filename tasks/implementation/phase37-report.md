# Фаза 37: Конкурентность Engine И Жизненный Цикл Удаления — Отчёт

Дата: 2026-10-22. Основание: `tasks/phase37-engine-concurrency-and-deletion-lifecycle.md`, F13–F17 аудита. Фазы 34–36 завершены; их regression suites сохранены. Следующие фазы не начинались; commit/push не выполнялись.

## Что Сделано

### F13: Сериализация Команд И Применения Приоритетов

- Расширена существующая сериализация `admissionMu`, без actor/FSM rewrite. Registration/Delete, Pause/Resume/restore, scheduler, metadata/recheck/discovery и stream reconcile применяют side effects под одним lock. Документ ownership/lock order: `internal/engine/CONCURRENCY.md`.
- `internal/engine/engine.go`: recheck теперь принадлежит Engine и runtime context, ждёт metadata/закрытие/отмену. На время проверки закрывает payload gate, очищает фактические priorities/overlay bookkeeping и держит reservation. `finishRecheck` проверяет идентичность поколения и вычисляет разрешение из текущего state, а не восстанавливает snapshot до Pause/Delete.
- `priority.go`, `torrent_uc.go`: single/bulk priority commands сериализованы с Pause и overlays. Неактивному runtime не включаются base file priorities; сохранённое намерение остаётся в map/SQLite. Bulk известных файлов применяется одной runtime-командой, включая metadata-pending. Индексы/enum проверяются до изменения map.
- `metainfo.go`, `swarm_watchdog.go`, `restore.go`: metadata callback проверяет текущий экземпляр под admission lock, discovery не восстанавливает priorities/активность Paused/initializing/rechecking runtime. FinishRestore не оставляет активные priorities у неактивного runtime. SourceURI больше не записывается под одним lock и не читается под другим.
- Общий `admitPayloadLocked` и writer-error generations фазы 36 сохранены. Stream reconcile не является дополнительным Allow; DiskFull не снимается callbacks/отменой stream.

### F14: Scheduler Slots

- `resource_manager.go`: `manageResources` уже был сериализован фазой 36; второй worker/система reservations не добавлялись. Проверены все engine Allow/priority apply paths.
- Downloading state служит reservation, включая ожидание metadata и активный recheck. Инициализируемый Downloading также учитывается, чтобы следующий проход не выделил его слот повторно.
- После ошибки admission счётчик учитывает фактический новый state (например, DiskFull/Error), а `downloadAllStarted` не остаётся ложно успешным. Pause/Delete/завершение освобождают слот для следующего прохода.
- Положительный лимит соблюдается для фоновых загрузок; ноль по прежней семантике не продвигает background queue. Stream-driven queued admission оставлен отдельным: file base selection остаётся None, действуют только reader/stream overlays. Порядок очереди и selected-files completion F24 не менялись.

### F15: Generation-Safe Stream Reconcile

- `stream.go`: session IDs монотонны в пределах StreamManager, не сбрасываются при очистке файла/re-add. StreamState содержит owner `*ManagedTorrent`; старые tokens не совпадают с сессиями нового поколения.
- Add/remove и snapshot/apply/publish reconcile сериализованы admission lock. Reconcile вычисляет ranges всех файлов одного runtime и применяет их объединение: очистка первого файла не сбрасывает shared boundary piece второго.
- Base priority apply повторно согласует overlay. Paused/DiskFull/recheck не получают High из старого stream snapshot. Bookkeeping защищено `sm.mu` на всём apply/publish; другие команды не могут вклиниться между snapshot и side effects.
- Debounce callback проверяет identity StreamState и TimerVersion под той же сериализацией, что и новые сессии. Остановленный, но уже поставленный в очередь timer не имеет права удалить новый state. Debounce после последней сессии сохраняет ranges; reference counting и Seek-поведение не заменены немедленной очисткой.
- Delete/Close останавливают timers, закрывают Done сессий и удаляют state соответствующего поколения. Повторные release/cancel и stale rollback безопасны. Accounting-only/HEAD/SkipQoS не создают payload ranges.

### F16: Owned Tasks И Shutdown

- `beginWork` регистрирует WaitGroup work под `storageTaskMu` с проверкой root cancellation. Все background launches используют этот gate; прямые watchdog/DHT/recheck goroutines заменены учтёнными tasks. Синхронные Add/Restore/AddPaused также учитываются, чтобы Close не закрыл client во время их выполнения.
- У runtime есть собственный context: Delete отменяет recheck/session watchers и инвалидирует поколение до Drop. Metadata watcher, уже исправленный в фазе 35, сохраняет ожидание Closed/engine cancellation и дополнительно сериализует application side effects.
- Session watchers ждут request, generation, Engine или session Done; ручной preload release больше не оставляет watcher ждать живой Background-context. DHT tasks ограничены generation/Engine cancellation и таймбоксом.
- Library status callbacks не захватывают map/stream/torrent locks под library lock: независимый `sync.Map` registry фиксирует owner, затем owned task проверяет поколение под admissionMu. Peer callbacks только логируют данные, не вызывая engine-команды. Task admission использует отдельный короткий `storageTaskMu`, под которым не выполняются library calls.
- Close сначала закрывает task admission/cancel, закрывает library download/upload admission, timers/direct streams/sessions, ждёт принятые tasks без application locks и затем закрывает client/storage. Повторный/concurrent Close безопасен. Client/storage pointers остаются неизменными для diagnostic readers; состояние shutdown определяется context, а не nil pointer.
- Уже поставленный runtime timer callback может вызвать только отказавший enqueue после Close; reader AfterFunc только отменяет context. Они не выполняют после закрытия admission state/storage side effects. Delete не делает WaitGroup.Wait под admission lock: отменяет и инвалидирует tasks; Engine Close их присоединяет.

### F17: Durable Deletion В Обоих Режимах

- `0007_deletion_intent.sql`: добавлены `deletion_mode` (`payload`/`keep`) и `deletion_error`; существующие cleanup manifests импортируются как `payload`, без изменения readiness/paths.
- `cleanup.go`: BeginCleanup теперь сохраняет режим атомарно с manifest и Paused state. BeginForget сохраняет `keep` и admission marker **до Drop**, не назначая unlink операции. Проверяется наличие записи/отсутствие конфликтующего режима.
- `torrent_uc.go`, `temporary_files.go`: повтор с другим режимом возвращает conflict. После intent commit ошибки Drop/validation/unlink/финальной БД оставляют pending запись; причина сохраняется отдельно. Если сохранение причины тоже не удалось, возвращается объединённая ошибка, intent не откатывается.
- Существующие startup/Resume/re-add/transfer/migration/SyncWorker checks cleanup marker работают для обоих режимов. Snapshot upsert дополнительно не перезаписывает state/error pending deletion даже при прямом вызове repository, минуя SyncWorker.
- Финальный SQLite delete остаётся транзакционным: record/files/history/disk latch удаляются вместе. Для `payload` освобождаются mapping aliases/root только при отсутствии других torrent records, ссылающихся на root. Для `keep` namespace reservations **сохраняются намеренно**, чтобы последующий add открывал оставленные isolated данные; это уточнение плана, а не потерянный cleanup. Service metainfo не считается payload.
- Ручные pending операции повторяются явно тем же Delete; startup их не активирует и не запускает destructive retry. Автоматический temporary payload cleanup сохраняет прежнюю политику периодического повтора; небезопасный manifest не угадывается.

### API И UI

- `internal/models/torrent.go`, repository loaders и persisted/runtime mapper: REST/SSE возвращают `deletion_mode`/`deletion_error`; pending отображается как Paused, не как активная загрузка.
- `web/src/types/index.ts`, `TorrentTable.vue`: обе раскладки показывают pending mode и причину ошибки. Ошибка удаления возвращает существующий HTTP 500, запрещённый Resume/смена режима — 409, missing — 404. Форматы запросов и CORS не менялись.
- README описывает slots, Pause/Resume/Delete, retry с тем же режимом, reservations kept-data, shutdown и пределы отмены.

## Тестирование

Добавлены `internal/engine/concurrency_test.go`, `internal/usecase/deletion_lifecycle_test.go`, `internal/repository/deletion_test.go`, `internal/delivery/http/api/deletion_test.go`; расширены metadata-pending priority regression и frontend tests.

- Delayed reconcile/discovery/recheck completion/priority command после Pause; stale debounce с новым stream и cleanup/rollback после Delete/re-add; фактические priorities/shared pieces после concurrent refresh и последнего release.
- Параллельные scheduler passes при лимитах 1/3 с barrier в disk probe: проверяется максимум Downloading на каждом probe/admission, затем повторное использование освобождённого слота. Отдельно отказ admission освобождает слот.
- Управляемый verifier проверяет recheck во время Pause/Close и отмену по Delete; metadata-pending recheck/Delete не оставляет вечного ожидания GotInfo. Close со stream/preload на живом Background-context; task-admission barrier доказывает, что Close ждёт принятый task и не принимает следующий; concurrent/repeated Close.
- Настоящие SQLite triggers: intent failure не выполняет Drop, финальный delete failure оставляет mode/error. Реальные reopen Engine/SQLite для `keep`/`payload` доказывают блокировку restore/Resume/add/transfer, безопасный retry и запрет смены режима. Upgrade pre-0007 сохраняет pending incomplete manifest.
- Rooted unlink fault injection возвращает EACCES после первого удаления: оставшийся `.part` и intent переживают ошибку, retry заканчивает операцию. Отказ engine Delete после остановки не запускает unlink. Существующие storage-safety тесты проверяют ошибку commit окончательного manifest, reopen и ownership; они сохранены.
- HTTP regression: failed Delete — 500, subsequent Resume — 409, список показывает диагностику. Frontend: 5 тестов, включая pending deletion в desktop/mobile.

Проверки (корень, кроме явно отмеченных каталогов):

```sh
go test -count=1 -timeout 90s ./...
go test -race -count=1 -timeout 90s ./internal/... ./cmd/...
go test -race -count=5 -run 'TestConcurrency|TestDeletion|TestDBOnlyMetadataPending' -timeout 90s ./internal/engine ./internal/usecase ./internal/repository ./internal/delivery/http/api
go vet ./...
go build -o <approved-temporary-directory>/phase37-hub ./cmd/hub
# pkg/anacrolix-torrent, без -skip:
go test -count=1 -timeout 90s . ./storage
go test -race -count=1 -timeout 90s . ./storage
# web:
npm test
npm run build
# Корень:
git diff --check
```

Логи в approved temporary directory `.../T/opencode/`: `phase37-final-tests.log`, `phase37-final-race.log`, `phase37-final-repeat.log`, `phase37-fork.log`, `phase37-fork-race.log`. Проверки успешны. macOS race linker может предупреждать `malformed LC_DYSYMTAB`; data races не обнаружены.

## Ограничения И Отклонения

1. Сохранён глобальный admission lock, а не реализован actor per torrent. Это минимальное расширение базы 36. Медленный probe/persistence может задержать независимую команду; NAS latency/throughput benchmarks не выполнялись. Тесты используют barriers и прямой вызов уже просроченного debounce token, не ожидание реальных 10 секунд.
2. Отсутствие races утверждается для проверенных путей, не как математическая гарантия всех возможных interleavings. Runtime библиотечные workers завершаются её Close/Drop; не выдаются за собственные engine tasks. Произвольные внешние пользователи raw torrent API могут обойти engine-инварианты и не являются поддержанным command path.
3. Uninterruptible filesystem syscall, зависший input Reader в синхронном Add, физический power-cut/readonly/full volume и реальные NAS/Smart TV/Windows/Linux runtime не проверялись. Fault injection и reopen проверяют программные границы отказа, не атомарность SQLite + filesystem или возможность прерывания любого syscall. Durable intent гарантирован после успешного SQLite commit; служебные записи могут отказать.
4. Keep-data namespace reservations намеренно не удаляются, иначе re-add оставленного isolated root был бы отклонён как неизвестный occupied path. Payload cleanup удаляет только неиспользуемые aliases; неизвестные/чужие файлы не удаляются рекурсивно. Ручные pending deletions требуют явного retry, не обещается автоматическое завершение после устранения ошибки.
5. Форк не менялся. Root/storage suites пройдены без исключений; optional libpossum/FUSE имеют прежние ограничения окружения из отчёта 35 и не объявляются успешно проверенными. F18/F22/F24/F25, auth и dependency upgrades не входят в фазу.

## Definition Of Done

- [x] F13: сериализованы команды/side effects, stale callbacks не отменяют Pause/Delete.
- [x] F14: slots/reservations не превышают лимит при конкурентных проходах, ошибки не удерживают ложный слот.
- [x] F15: объединённый reconcile, session/generation/timer tokens, refcount и debounce.
- [x] F16: документированы ownership/lock order, task admission/cancellation/join и generation invalidation.
- [x] F17: durable recovery обоих delete modes, безопасные ошибки/reopen/retry.
- [x] Regression suites фаз 34–36 сохранены, storage/verified/priorities/DiskFull гарантии не ослаблены.
- [x] API/UI показывают pending/error без ложного успеха, CORS/совместимость сохранены.
- [x] Tests/race/vet/build проходят; пределы in-flight I/O и непроверенных окружений указаны.
- [x] README, concurrency contract и этот отчёт обновлены.
