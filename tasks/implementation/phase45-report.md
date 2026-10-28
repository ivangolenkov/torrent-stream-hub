# Фаза 45 — legacy completion publication: отчёт

## Дополнение: флаг отключения автоматического per-piece Sync

После анализа скачков на NAS пользователь явно запросил флаг, включённый по
умолчанию: **`HUB_BT_DISABLE_PIECE_SYNC=true` / `--bt-disable-piece-sync=true`**.
Это расширение текущей фазы 45; следующий этап не начинался, Windows gate остаётся
открытым. Нижеописанное исходное решение с обязательным вызовом best-effort Flush
теперь является opt-in режимом (`false`), а не application default.

### Изменения

- `internal/config/config.go`: CLI/env с default true; CLI приоритетнее env.
  Programmatic zero-value Config получает тот же default, пара
  `BTDisablePieceSync=false, BTDisablePieceSyncConfigured=true` сохраняет явное
  включение Sync при повторном ApplyDefaults. Настройка restart-scoped.
- `internal/engine/client_config.go`: передача настройки в file storage.
  `engine.go` пишет эффективное `bt_disable_piece_sync` при создании Engine.
- `storage/file-client.go`, `file-piece.go`: новый `DisablePieceSync` проверяется
  только вокруг автоматического syncCompletion в commitCompletion. При true
  остаются Set/error handling и fresh publication, но Flush не вызывается и его
  timing/error counters не меняются. При false сохраняется Set → synchronous
  best-effort Flush с WARN. Zero-value library opts остаётся false, чтобы не
  менять контракт standalone потребителей библиотеки.
- Явный Flush, namespace promotion/import sync, checkpoint payload/directory sync,
  hashing, snapshot policy, range/cache/lifecycle и SQLite не менялись.
  Никакой новой гарантии mandatory payload sync при каждом Close не заявляется.
- README, sample `docker-compose.yml`, concurrency/state contracts и план фазы
  обновлены. Sample compose задаёт true; развёрнутый NAS не менялся.

### Риск и критерии приёмки дополнения

Default теперь допускает публикацию без попытки per-piece durability. При power
loss могут исчезнуть непопавшие в checkpoint данные; SQLite hint может уже
сохраниться. Hints сами не дают readiness при application reopen, checkpoint
по-прежнему требует обязательной синхронизации и validation. Это не отменяет ранее
принятый stale-callback риск и не доказывает устранение всех скачков скорости.

- [x] CLI/env default true, true/false и CLI precedence протестированы.
- [x] Programmatic default и explicit false после ApplyDefaults протестированы.
- [x] Engine integration с persistent completion проверяет реальную hash/publication
  в обоих режимах и zero/nonzero completion sync counters.
- [x] Storage matrix managed/verified/legacy/nonpersistent: skip только automatic
  Flush; hint и readiness публикуются, explicit Flush по-прежнему вызывает I/O и
  возвращает injected error. Ошибка Set не подавляется.
- [x] При включённом флаге checkpoint payload/directory sync failures запрещают
  publication; namespace promotion sync failure сохраняет .part и возвращает error.
- [x] Целевые tests config/engine/storage — PASS.
- [x] Полный fork race ×3 — PASS (26.339/7.008 s),
  `piece-sync-fork-race.log`; config/engine targeted race ×3 — PASS,
  `piece-sync-policy-race.log`.
- [x] Native Linux arm64 полный storage ×3 — PASS (`piece-sync-storage-linux.log`).
- [x] Новый default проверен real SQLite hints crash/reopen matrix v1/v2/hybrid
  с valid/corrupted bytes: race ×3 — PASS (`piece-sync-hints-race.log`), native
  Linux ×3 — PASS (`piece-sync-hints-linux.log`). Hints без checkpoint не обходят
  hashing даже после публикации без per-piece Flush. Native Linux engine policy
  integration ×3 — PASS (`piece-sync-engine-linux.log`).
- [x] Windows amd64 engine/storage cross-compile — PASS; это не native execution.
- [x] Полный `make check` — PASS (`piece-sync-flag-check.log`): root/fork tests
  и race, frontend tests/build, vet и application build. Usecase race 303.388 s,
  fork race 8.658/2.742 s; macOS linker warnings не привели к ошибкам.
- [ ] Новый native Windows CI ожидается; NAS performance A/B не выполнялся.

Историческая корректировка анализа: per-piece Flush был ещё до фазы 40 (фаза 19),
а фаза 40 изменила persistence и порядок/error policy. Поэтому прежнее объяснение
«до фазы 40 Flush не было» ошибочно; отсутствие ранних latency-счётчиков также не
доказывает отсутствия задержек. Флаг — отдельный проверяемый эксперимент, не
доказательство причины замедления.

## Статус и разрешённый объём

Реализация выполнена локально по команде пользователя после сохранения
[плана фазы 45](../phase45-legacy-completion-publication.md). Фаза 44 остаётся
закрытой с подтверждённым пользователем CI; этот результат не заменяет новый
Windows CI для фазы 45. Локальные финальные проверки пройдены, native Windows пока открыт.
Агент не менял NAS, конфигурацию сервера и payload, не выполнял deploy/restart,
не делал commit/push. Ранее согласованные phase44 docs не откатывались.

## Что реализовано

- `pkg/anacrolix-torrent/storage/file-piece.go`: commitCompletion берёт существующий
  payload read lease для Set(true), освобождает его, затем persistent backend
  выполняет синхронный `syncCompletion`/Flush. Ошибка Set возвращается до Flush.
  Ошибка Flush остаётся в counters и WARN `error flushing completed piece`, но
  сама не запрещает публикацию. Nonpersistent backend не получил новый Flush.
- VerifyOnOpen publication после Flush получает full-piece read lease, CURRENT
  signature и interns/marks fresh под freshMu. Сравнения с hashSnapshot до/после
  Set/Flush, captured `SameFile`/`sig.valid` gates и ошибки stale snapshot удалены.
  Ошибки rooted acquisition/current evidence не превращены в WARN. Namespace
  promotion/import и явный Flush не менялись; promotion всё ещё может вернуть ошибку
  ПОСЛЕ публикации freshness в commitCompletion.
- MarkComplete сохраняет lifecycle read lease до окончания best-effort sync и
  publication/promotion. Во время Flush payload lease не удерживается: соседние
  и совпадающие writes могут пройти, как в согласованном плане. Byte-range gate,
  cached fd lifetime, Close/Reset joins, guards, hashing и engine pendingWrites
  не менялись. Async Flush/group commit/new queues/buffers/mmap не добавлялись.
- Все production-ссылки hashSnapshot проверены: BeginVerification создаёт запись;
  writes/acceptFileWrite/revoke/MarkNotComplete стирают; guard failure/reset/Close
  очищают; markFresh consumes; checkpoint.go исключает присутствующие записи.
  HashSnapshot остаётся in-flight evidence для seal, но больше не является
  разрешающим token публикации callback.
- README, `internal/engine/CONCURRENCY.md`, `internal/usecase/STATE.md` и контракты
  фаз 42/43 дополнены явной текущей policy. История более строгих гарантий не
  переписана задним числом. Поля Go/TS diagnostics не менялись: completion sync
  errors теперь могут расти без отказа от publication; checkpoint sync туда не входит.

## Принятые изменения integrity policy

Это намеренное ослабление по выбранному пользователем варианту «A + снятие
snapshot-проверок», не эквивалентная безопасная оптимизация.

1. Late callback может re-grant freshness после записи/revoke, не проверяя хеш
   новых bytes. PendingWrites/range admission стабилизируют отдельные шаги, но
   не доказывают актуальность старого hash result между ними.
2. Stale fresh completion может попасть в следующий checkpoint. Его mandatory
   sync обеспечивает durability, но не пересчитывает и не исправляет hash.
3. ENOSPC/другая ошибка именно completion Flush — WARN-only. Disk-admission,
   WriteAt errors, известная volume failure и namespace/checkpoint errors остаются
   отдельными gates. Нельзя утверждать «любая ошибка диска запрещает completion».
4. Hint может попасть на диск раньше payload. В приложении hints недоверенные при
   reopen и сами не дают readiness; общий non-VerifyOnOpen backend теперь имеет
   более слабую legacy crash policy, и это ограничение на него не переносится.
5. Best-effort НЕ означает async/omitted: вызов Flush остаётся синхронным и может
   долго блокировать завершение piece. Снижение WriteAt/fsync pauses и SQLite
   persistence failures этим изменением не объявляется достигнутым.

## Тесты

### Новые проверки

- `storage/completion_policy_test.go`: persistent managed/verified/legacy matrix
  с остановленным реальным Flush. Hint доступен до его возврата, sync counters
  active, lifecycle остаётся joined; fresh publication для VerifyOnOpen ждёт
  возврата Flush. ENOSPC даёт WARN, error counter и successful publication.
- Set failure не вызывает Flush и не публикует completion; nonpersistent не
  вызывает новый per-piece sync. Explicit Flush продолжает возвращать error.
- Payload и directory sync failure в checkpoint по-прежнему запрещают publish,
  даже после successful best-effort completion с Flush error.
- `TestLegacyStaleCallbackCanEnterCheckpoint`: реальные SHA-1 входы до/после
  write различаются, old callback всё равно публикует readiness и seal включает
  piece. Это canary принятого integrity risk, НЕ доказательство hash correctness.
- `internal/usecase/legacy_completion_crash_test.go`: subprocess с реальным
  managed app backend/SQLite ждёт committed complete hints, убирает test checkpoint
  и останавливается на барьере. Родитель убивает процесс без shutdown/final seal;
  на reopen v1/v2/hybrid hints не обходят actual hashing. Valid bytes дают fresh
  completion, same-size corrupted bytes не дают ни downloaded, ни restored trust.
  Kill сам по себе не моделирует потерю page cache/power cut: corruption — явная
  тестовая подмена после kill. Продакшен checkpoint protocol этим setup не меняется.
- `openResumeApplication` в `resume_runtime_test.go` получил opt-in real persistent
  completion для этого сценария; прочие fixtures сохраняют прежний default Map.

### Обновлённые ожидания

В `completion_sync_test.go`, `managed-files_test.go`, `managed-ranges_test.go`,
`verification_concurrency_test.go`, `verified_file_test.go` и
`verified_persistent_test.go` заменены только прежние stale/sync-error assertions,
противоречащие новому policy. Проверки read/write range exclusion, отзыва trust
самой записью до нового callback, guards, alias/publication no-replace, namespace
sync failures, descriptor Close и checkpoint сохранены. В tests явно написано,
что успешная stale publication — принятый риск, а не новая hash-проверка.

## Проверки и промежуточные неудачи

- Первый storage-прогон после runtime change закономерно остановился на старых
  stale/sync-error expectations (`phase45-storage-initial.log`). Они обновлены
  по новому контракту, не удалены целые suites.
- Новый crash test сначала не доходил до барьера: прежний app fixture использовал
  Map completion, не SQLite. Добавлен opt-in реального SQLite, parent сохраняет
  stdout diagnostics дочернего процесса. Затем обнаружена ошибка fixture identity:
  pure v2 нельзя искать по SHA-1 `MetaInfo.HashInfoBytes`. Тест теперь использует
  реальный persisted canonical record hash и явно проверяет attachment перед Verify.
  Runtime не менялся ради этих тестовых ошибок; assertions untrusted hints сохранены.
- [x] Полный fork race ×3 — PASS (`phase45-fork-policy-race.log`, 23.404/7.001 s);
  повторный полный fork race с последним explicit Flush test — PASS
  (`phase45-fork-final-race.log`, 23.696/7.280 s); финальный fork vet — PASS.
- [x] Новый application hints crash/reopen race ×2 — PASS (12.105 s,
  `phase45-hints-crash-race.log`); финальный повтор ×3 — PASS
  (`phase45-hints-final-race.log`, 15.960 s).
- [x] Native Linux arm64 полный storage ×3 — PASS (`phase45-storage-linux.log`);
  полный `TestResumeRuntime` включая clean/partial/crash/pre-attach/periodic/recheck
  и новый real SQLite hints crash — PASS (`phase45-usecase-linux.log`).
  Один повтор после добавления explicit Flush test превысил tool timeout 150 s
  без финального PASS/Go panic в логе; он не принят за успешный. Повторный полный
  storage ×3 с verbose output и test timeout 90 s завершился PASS
  (`phase45-storage-linux-final.log`). Причина tool timeout не установлена;
  утверждать доказанный runtime deadlock или аппаратную причину оснований нет.
- [x] Windows amd64 storage/core/usecase cross-compile — PASS. Linux core cross-compile
  и targeted native core execution ×3 — PASS (`phase45-core-linux.log`).
- [x] Финальный `make check` — PASS (`phase45-check.log`): root tests/race, fork
  tests/race, 44 frontend tests/build, vet и application build. Usecase race
  301.309 s; fork race 9.899/4.195 s. Runtime function после старта не менялась;
  последний explicit Flush test и fixture cleanup проверены отдельными последующими
  прогонами. macOS linker LC_DYSYMTAB warnings не привели к ошибкам.
- [ ] Native Windows CI этого revision нужен; локальной Windows execution нет.
- [ ] NAS deploy/performance validation не выполнялись и не заявляются.

## Статус DoD

- [x] F45-01: legacy contract/scope и stale/crash/Flush-error риски описаны явно.
- [x] F45-02: Set(true) → synchronous best-effort Flush, WARN/errors counters,
  Set failure и nonpersistent scope реализованы и проверены.
- [x] F45-03: snapshot/signature callback gates сняты, in-flight seal обязанности
  BeginVerification/hashSnapshot сохранены и проверены аудитом/тестами.
- [x] F45-04: отдельные hashing/cache/range/lifecycle/checkpoint/rooted/restore
  механизмы сохранены в согласованном scope; остальные subsystem code не менялись.
- [x] F45-05: unit/barrier/crash/restore tests подтверждают новую policy и её риски.
- [ ] F45-06: make check/race/native Linux/macOS пройдены; native Windows CI
  этого revision пока ожидается, cross-compile не закрывает gate.
- [x] F45-07: документация и этот отчёт обновлены; NAS не менялся.

### Отклонения

Непринятый Windows gate не замещён cross-compile/CI фазы 44. Scope runtime не
расширялся: SQL persistence diagnostics, batching, async I/O и удаление per-piece
sync не реализовывались. Метод internSignatureLocked по-прежнему использует identity
для выбора shared signature, но не отклоняет callback по старому hash snapshot.
Это сохранение структуры evidence/restore, не скрытое восстановление снятого gate.
