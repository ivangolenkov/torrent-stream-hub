# Фаза 45: Возврат legacy-публикации completion и best-effort Flush

## Статус и зависимости

- Статус: **реализована локально; make check, повторные race и native Linux/macOS проверки пройдены, новый native Windows CI ожидается**. Фаза пока не объявляется полностью завершённой; результаты — в [отчёте](implementation/phase45-report.md).
- Создана 28 октября 2026 года по согласованному с пользователем плану.
- Зависимость: [фаза 44](phase44-selective-verification-and-download-concurrency.md)
  полностью завершена и принята; финальный CI подтверждён пользователем.
- Реализация начата по отдельной команде пользователя «Приступай к новой фазе».
  Это не разрешение менять развёрнутый NAS: deploy/restart остаются отдельным действием.
- По завершении создать `tasks/implementation/phase45-report.md` с изменениями,
  проверками, DoD и принятыми отклонениями от прежних integrity-контрактов.

## Решение пользователя

**Последующее дополнение по прямому запросу пользователя:** добавить restart-scoped
`--bt-disable-piece-sync` / `HUB_BT_DISABLE_PIECE_SYNC`, default **true** (автоматический
per-piece Sync Flush пропускается). False сохраняет описанный ниже синхронный
best-effort режим. Это дополнение текущей фазы, не переход к следующей при открытом
Windows gate. Hashing, Set errors, range/lifecycle, explicit Flush, namespace sync
и mandatory checkpoint sync не меняются. Standalone storage сохраняет прежний
zero-value default; application передаёт настройку явно. Отчёт дополнен отдельно.

Пользователь отменил предложение полностью убрать per-piece Completion sync
только в managed-режиме и выбрал:

1. **«A + снятие snapshot-проверок»** — возврат legacy-порядка публикации completion
   и удаление дополнительных проверок устаревшего результата в commitCompletion.
2. **«Да, best-effort»** — Flush каждой piece сохраняется для persistent completion,
   но его ошибка становится WARN и не запрещает публикацию.
3. Сохранить этот план как новую фазу, а фазу 44 закрыть с подтверждённым CI.

Итоговый scope — не полный git rollback фаз 40–44 и не удаление кеша/range locks.
Legacy здесь означает выбранную семантику Set/Flush, а не побайтовое восстановление
старого файла. Snapshot-проверки снимаются по отдельному явному решению пользователя;
не следует утверждать, что такой же snapshot-протокол существовал уже в фазе 19.

## Исходное поведение и цель

В текущем runtime persistent completion требует успешного Flush перед Set(true).
Публикация дополнительно сравнивает hashSnapshot и текущие signatures до/после
сохранения hint; stale/revoked результат не становится fresh completion.

В импортированном коде фазы 19 был порядок Set(true) → Flush с WARN при ошибке
Flush. В фазе 40 (`c4477db`) порядок изменён на Flush → Set(true), ошибка Flush
стала блокировать публикацию. В фазах 43–44 появились durable checkpoints и
дополнительное согласование доверия/параллельного I/O.

Цель новой фазы — реализовать выбранный пользователем более слабый publication
contract, а не продолжать усиливать его проверками. Нельзя обещать ускорение только
из-за слова best-effort: **Flush остаётся синхронным вызовом и продолжает ждать I/O**.
Этот план не включает batching, async Flush или полное удаление per-piece fsync.

## Блок 1 — Явный контракт и границы изменения

- Зафиксировать legacy-порядок и принятые риски в README, CONCURRENCY и STATE до
  признания реализации завершённой. Не называть снятие проверок эквивалентным по
  безопасности прежнему strict publication contract.
- Ограничить удаление проверок путём MarkComplete/commitCompletion. Проверки
  rooted containment, volume guard, cache identity на acquisition, restore token,
  checkpoint evidence и ownership вне этого пути не удалять.
- Сохранить hashing полученных pieces, selective startup, full Recheck, engine
  pendingWrites, managed descriptor cache, byte-range admission и Close/Reset joins.
- Сохранить WAL/FULL, pool, DiskFull-пороги/ошибки записи, CORS, Range, QoS и
  refcount/debounce. Не вводить mmap, новые глобальные блокировки или payload queues.
- Не менять сервис/NAS без отдельного разрешения; текущие diagnostics — GET-only.

## Блок 2 — Legacy-публикация completion

Основной файл: `pkg/anacrolix-torrent/storage/file-piece.go`.

1. В commitCompletion выполнить `pieceCompletion.Set(key, true)` до Flush.
   Ошибка самого Set остаётся ошибкой; её не превращать в успешную публикацию.
2. Для persistent PieceCompletion выполнить синхронный best-effort Flush после
   успешного Set. Ошибка Flush — WARN, затем продолжить публикацию. Поведение
   применяется и к managed, и к обычному persistent backend согласно выбранному
   плану. Nonpersistent backend не должен получить новый обязательный per-piece Flush.
3. Убрать обе прежние обязательные ветки sync-before-publication: verified и legacy.
   Диагностический `syncCompletion` можно сохранить как обёртку best-effort вызова:
   count/active/duration/errors должны отражать реально выполненный Flush.
4. Убрать сравнения с hashSnapshot до/после Set/Flush, включая ошибки
   `storage generation changed during verification`, сравнение захваченных/current
   signatures и managed `SameFile`/`sig.valid` gates в этом пути.
5. Для fresh-evidence использовать текущую файловую подпись без сравнения с
   исходным hashSnapshot. Реальные ошибки acquisition/rooted access не подавлять
   под видом отменённой snapshot-проверки.
6. BeginVerification/hashSnapshot сохранить: checkpoint использует наличие записи
   для исключения активного hashing из seal; writes/revoke её стирают. Аудит всех
   читателей обязателен, чтобы удаление сравнений не сломало эти обязанности.
7. Не менять отдельно поведение явного Flush, checkpoint Flush, directory sync
   и namespace promotion/import. Не превращать ошибку durable checkpoint в WARN.

## Блок 3 — Сохраняемые функции и принятые риски

Сохраняются fast resume/checkpoint со своим sync-before-publish, durable revocation
перед внутренней mutation, managed cache, range admission и все diagnostics.
Однако сохранение этих механизмов не означает сохранение прежних гарантий о том,
что опубликованные bytes соответствуют именно последнему успешному hash attempt.

Принятые изменения поведения необходимо отразить явно:

- Запись между hashing и публикацией больше не обязана отклонять старый hash result
  в storage. Engine pendingWrites и range admission остаются, но не доказывают
  безопасность всех late callbacks и прямых backend вызовов. Частота риска неизвестна.
- Отзыв доверия между этапами публикации может быть перекрыт публикацией старого
  результата после снятия соответствующих проверок. Нельзя утверждать обратное
  только потому, что сами revoke callbacks сохранены.
- Если такой stale result попадёт в fresh completion, он потенциально может
  попасть и в следующий checkpoint. Синхронизация checkpoint обеспечивает
  durability, но сама не пересчитывает хеш и не исправляет ложное доверие.
- Ошибка Flush, включая DiskFull/отложенную I/O error на этом этапе, не отменяет
  публикацию. Основные disk-admission и WriteAt error gates остаются; это не
  эквивалент гарантии «любая ошибка диска запрещает completion».
- Hint может сохраниться раньше payload. В приложении hints сами не являются
  restore authority; вне доверенного checkpoint данные требуют hashing при open.
  Для общего non-VerifyOnOpen persistent backend нельзя переносить это утверждение:
  восстановленное старое поведение имеет более слабые crash guarantees.
- WARN вместо возврата ошибки не устраняет задержку самого синхронного Flush.

## Блок 4 — Тесты и диагностика

- Обновить `storage/completion_sync_test.go`, `publication_test.go`,
  `managed-files_test.go`, `managed-ranges_test.go` и затронутые core/engine tests.
  Не удалять целиком suites: изменить только ожидания, противоречащие новому policy.
- Детерминированно проверить порядок Set → Flush, в том числе наблюдение hint
  во время остановленного Flush, WARN/успешную публикацию при Flush error и отказ
  при Set error. Проверить persistent managed, strict и nonpersistent варианты.
- Проверить counters реального best-effort Flush, отсутствие новых per-piece
  filesystem probes из-за polling и сохранение видимости ошибок.
- Старые tests отказа от stale hash заменить явными tests выбранной семантики и
  документированного риска. Не называть принятую stale publication корректным
  hashing результата и не убирать unrelated security/lifecycle assertions.
- Сохранить тесты независимых/пересекающихся диапазонов, shared-file boundaries,
  descriptor bound/eviction/retirement и Close/Recheck join.
- Проверить clean/crash reopen: hints без checkpoint не дают приложению trust;
  старый/partial checkpoint восстанавливает только свой subset; checkpoint payload/
  directory sync и failed seal не обходятся best-effort policy. Разделить проверку
  этих protocol boundaries и риск включения stale fresh evidence, принятый выше.
- Проверить v1/v2/hybrid, fast/verify/full Recheck, Pause/Resume, DiskFull,
  move/delete/storage recovery и HTTP Range в затронутом объёме.
- Выполнить `make check`, повторные fork race и native Linux/macOS проверки.
  Windows cross-compile выполнить локально; native Windows CI обязателен и не
  заменяется cross-compile или результатом CI закрытой фазы 44.

## Блок 5 — Документация и приёмка

- Обновить README, `internal/engine/CONCURRENCY.md`, `internal/usecase/STATE.md`,
  затронутые fast-resume/storage contracts и новый отчёт фазы 45. Историю фаз
  19/40/43/44 не переписывать как будто слабая publication policy действовала всегда.
- В отчёте перечислить изменённые файлы, команды/результаты, ошибки промежуточных
  прогонов, текущий DoD, платформенные blockers и явные изменения integrity policy.
- Если измеряется NAS speed, фиксировать counter deltas/useful bytes и сохранять
  ограничение публичного swarm: без контролируемого A/B не приписывать изменение
  средней скорости только этому патчу. Deploy/restart требуют отдельного разрешения.

## Definition of Done

- [x] F45-01: legacy contract, точный scope и принятые stale/crash/Flush-error риски
  зафиксированы без обещания эквивалентности прежней integrity policy.
- [x] F45-02: persistent completion выполняет Set(true) → synchronous best-effort
  Flush; Flush failure видна как WARN и не блокирует publication; Set errors сохранены.
- [x] F45-03: snapshot/signature comparison gates commitCompletion сняты согласно
  решению пользователя; BeginVerification/hashSnapshot сохраняют обязанности seal.
- [x] F45-04: hashing, cache/range/lifecycle barriers, checkpoint sync/restore gates,
  rooted security, WAL/FULL, disk-admission, Range/CORS/QoS сохранены в оговорённом scope.
- [x] F45-05: unit/barrier/crash/restore tests подтверждают выбранную семантику,
  покрывают её риски и не выдают stale publication за доказанную hash integrity.
- [ ] F45-06: make check/race/Linux/macOS и новый native Windows CI проходят;
  локальные проверки пройдены, Windows core/storage/usecase cross-compile пройден,
  native Windows CI остаётся открытым и не заменяется cross-compile.
- [x] F45-07: документация и `tasks/implementation/phase45-report.md` обновлены;
  runtime/NAS меняются только в отдельно разрешённом объёме.
