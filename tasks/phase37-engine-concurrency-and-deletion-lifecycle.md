# Фаза 37: Конкурентность Engine И Жизненный Цикл Удаления

## Статус И Зависимости

- Статус: реализовано и проверено; DoD закрыт. Отчёт: `tasks/implementation/phase37-report.md`.
- Основание: `tasks/draft/codebase-audit-2026-10-21.md`, **F13–F17** (P1); связанные рекомендации A01–A02, A04.
- Предусловие реализации: завершённый и проверенный DoD фаз 34–36. Использовать их storage mapping, verified readiness, durable cleanup и единый payload gate.
- Описания и номера строк аудита относятся к состоянию до этих фаз. Перед исправлениями повторно проверить актуальный код: не считать уже устранённые окна открытыми и не объявлять находку закрытой без regression-тестов.

## Цель И Границы

Сделать конкурентные команды и callbacks предсказуемыми: завершённая Pause/Delete не отменяется устаревшим действием, scheduler не превышает лимит, stream overlays согласованы с актуальными сессиями, Engine владеет всеми своими фоновыми задачами, а неудачное удаление не приводит к восстановлению загрузки после рестарта.

В scope входят сериализация переходов, generation/session tokens, reconcile приоритетов, lifecycle задач и восстановление deletion intent. Полный actor/FSM rewrite не обязателен: допустимо расширить существующие механизмы, если их корректность доказана тестами. Выбор и порядок locks документируются до изменения конкурентных путей.

Не входят redesign очереди/selected-files completion F24, preload singleflight/TTL F25, реализация HTTP recheck F18, paused lazy streaming F22, speed sampler F23, auth, dependency upgrades и остальные находки аудита. Внутренний recheck входит только в части конкурентности, отмены и ownership. Изменения смежного поведения, необходимые для F13–F17, обосновывать в отчёте.

## Исходная База После Фаз 34–36

- `admissionMu` уже сериализует scheduler и основные переходы payload admission. F14 требует проверки всех путей выделения слотов, а не обязательного введения второго scheduler worker.
- `admitPayloadLocked` — единственный engine Allow; sticky disk latch и writer-error version запрещено обходить через новый reconcile.
- Base priorities сохраняются отдельно от snapshots. Overlay и Pause/DiskFull не должны записывать временные приоритеты в SQLite.
- `BeginCleanup`/`SaveCleanupPaths` уже сохраняют intent и manifest до unlink для удаления payload. F17 — проверка и завершение этого протокола, включая ошибки и рестарт, а не создание параллельного журнала без необходимости.
- Direct stream handles уже привязаны к экземпляру `ManagedTorrent`. Аналогичную защиту нужно проверить для обычных sessions, preloads, debounce и фоновых callbacks.

## Общая Модель Конкурентности

1. Определить владельца каждого изменяемого поля Engine/ManagedTorrent/StreamState и единый порядок locks. Отдельно разобрать `sourceURI`, state/error, initialization, priorities, generations, readers и applied ranges.
2. Runtime generation идентифицирует экземпляр торрента, а не только infohash. Удаление и повторное добавление того же hash создаёт новое поколение; старые callbacks не имеют над ним полномочий. Допустима проверка идентичности экземпляра вместо числового generation.
3. Версия команд/reconcile внутри поколения отличает актуальное намерение от устаревшего snapshot. Проверка должна защищать фактический side effect, а не только запись результата после него.
4. Для каждой операции определить точку вступления в силу и постусловие успешного ответа. SQLite и библиотека не образуют общей транзакции: порядок persist/apply и безопасное состояние при частичном отказе описать явно.
5. Не держать locks, которые нужны callback/task для завершения, во время ожидания этой задачи, Drop или shutdown. Не допускать обратного захвата engine/library/stream/repository locks.

## F13: Согласованные Команды, Состояние И Приоритеты

- Сериализовать Pause/Resume, scheduler activation, metadata/recheck completion, изменение base priorities и применение overlay в объёме их общих side effects.
- Устаревший metadata/recheck/scheduler callback не может после завершённой Pause вернуть background priorities или открыть payload admission. Завершённая Delete запрещает любые изменения удалённого поколения.
- Считать effective priorities из актуального пользовательского намерения и overlay; не восстанавливать их из старого snapshot. Повторное применение идемпотентно.
- Сохранить gate фазы 36: overlay не разрешает загрузку при Paused, initialization, deletion/migration block или disk latch. Verified disk read и разрешённый upload не приравнивать к payload download.
- При ошибке persistence/application не возвращать ложный успех и не открывать gate. Сохранённые Normal/High/Skip переживают повторную команду и рестарт; частичный runtime apply должен иметь безопасный путь reconcile.
- Отдельно определить поведение overlays при Pause/Resume: refcount/debounce не сбрасываются ради снятия запрета, а старая операция не отменяет более новую команду.

## F14: Атомарное Выделение Слотов Scheduler

- Инвентаризировать все вызовы `manageResources` и пути активации: startup, add, Resume, metadata, recheck, periodic monitor и callbacks.
- Подсчёт активных фоновых загрузок, выбор кандидатов и выделение/освобождение слота должны составлять одну сериализованную операцию либо использовать защищённые reservations.
- Для положительного лимита число активных фоновых загрузок и незавершённых reservations не превышает `MaxActiveDownloads`. Семантику нулевого значения сохранить и явно описать.
- Ошибка admission/metadata/persistence и Pause/Delete освобождают reservation ровно один раз. Повторный scheduler pass/Resume не создаёт второй слот тому же поколению.
- Stream-driven download сохранить отдельным от фоновых слотов по действующему контракту. Overlay queued-торрента не включает всю base selection как дополнительную фоновую загрузку.
- Не менять порядок очереди и критерий завершения выбранных файлов ради закрытия F14: это F24.

## F15: Сериализованный Reconcile Stream Overlays

- Session token связывает session ID, torrent generation и file index; cleanup/release работает только со своей сессией. Повторные cancel/release безопасны и не уменьшают refcount ниже нуля.
- Add/remove stream/preload, refresh и debounce выполняют согласованный reconcile на торрент. Учитывать пересечение pieces разных файлов: reconcile одного файла не должен снять приоритет, необходимый другому.
- Effective overlay — объединение актуальных ranges всех подходящих сессий поверх base priorities. `AppliedRanges` соответствует реально применённому результату; устаревший refresh не может оставить внешние priorities новее/старее записанного состояния.
- Callback debounce проверяет актуальный token/version непосредственно при cleanup. Новая сессия между истечением таймера и reconcile отменяет право старого callback удалить её state или ranges.
- Delete/Close инвалидируют timers, sessions и ожидающие refresh удалённого поколения. Старый cleanup после re-add того же hash не меняет новое поколение, даже если ID/file index совпали.
- Сохранить reference counting, debounce при Seek и range-aware QoS. HEAD/пропущенные QoS-запросы и accounting-only direct streams не создают payload overlays. Disk latch не снимается пересчётом overlay.
- При ошибке add/reconcile rollback удаляет только свою сессию; параллельная новая сессия не теряется. Проверять не только bookkeeping, но и фактические library piece/file priorities.

## F16: Ownership И Завершение Фоновых Задач

- Инвентаризировать engine-owned goroutines/timers: metadata watchers, recheck/verification, watchdog/discovery, storage/error tasks, stream cancellation watchers, preload cleanup, debounce и callback-triggered reconcile.
- Каждый task принадлежит Engine и, где применимо, torrent generation; получает отмену при Delete/Close. Metadata watcher ожидает одновременно metadata, закрытие торрента и engine cancellation.
- Убрать `context.Background()` из длительных дочерних операций, которым требуется отмена. Engine root context сам по себе допустим; context запроса не заменяет lifecycle Engine для задач, переживающих запрос.
- Регистрация task атомарна относительно начала shutdown: после закрытия task admission нельзя выполнить новый `WaitGroup.Add`, racing с `Wait`, или запустить неучтённую goroutine. Вложенные задачи и timer callbacks подчиняются тому же правилу.
- Close идемпотентен: закрыть admission, отменить contexts/timers/readers, завершить принятые задачи и только затем закрыть используемые ими ресурсы в обоснованном порядке. Delete аналогично завершает/инвалидирует задачи своего поколения.
- Request cancellation, Delete и Close завершают stream watchers даже если исходный request context не отменён вызывающей стороной. После Close нет новых reconcile/persistence side effects от engine-owned tasks.
- Исправить несогласованную защиту полей (`sourceURI`, recheck state и найденные аналоги). Race detector дополняет, но не заменяет проверку логических гонок.
- Ограничение: context не гарантирует прерывание уже начатого filesystem syscall. Проверять bounded shutdown на управляемых ожиданиях и документировать границы для зависшего I/O; нельзя объявлять безопасным закрытие storage под продолжающей работать задачей.

## F17: Durable Intent Ручного Удаления

- Использовать единый recovery-протокол для ручного и автоматического удаления payload: commit deletion intent до необратимого Drop/unlink; после остановки writers зафиксировать окончательный проверенный manifest до первого unlink.
- Если intent не сохранён, не начинать разрушительную часть и вернуть ошибку. Если ошибка произошла после commit, запись остаётся pending deletion, не обычной активной загрузкой.
- Startup, Resume, duplicate add, metadata callbacks и SyncWorker не восстанавливают download pending-deletion записи и не стирают её intent. Recovery работает и при отсутствии runtime/metadata, не считая неполный manifest доказанно пустым.
- Ошибки Drop, проверки ownership, unlink, сохранения manifest и финального удаления БД сохраняют безопасное, диагностируемое и повторяемое состояние. Уже отсутствующий файл допускается при повторе; чужие файлы, aliases и служебные данные защищены правилами фаз 34–35.
- Финальное удаление torrent record, mapping/aliases, latch и cleanup intent согласовать транзакционно там, где это SQLite-данные. При отказе финального commit повтор операции безопасен, даже если payload уже удалён.
- Для `deleteFiles=false` явно сохранить семантику: удаляется запись/runtime без unlink payload. Проверить ошибку БД после остановки runtime: нужен durable intent режима удаления либо эквивалентный доказанный протокол, исключающий нежелательное восстановление после рестарта. Payload при retry/startup не должен удаляться из-за потери режима.
- Определить, какие pending операции автоматически повторяются, а какие требуют ручного retry; необратимо небезопасный manifest не должен запускать бесконечные destructive retries. Success возвращается только по завершении операции, если API явно не определяет отдельный pending-ответ.

## API И Документация

- Сохранить endpoint names, TorrServer-совместимость и CORS `*`. Pending deletion конфликтует с активацией; missing — 404, конфликт — 409, внутренняя ошибка — 500 по действующим HTTP-контрактам.
- В REST/SSE и Web GUI pending deletion и причина ошибки не должны выглядеть как обычная активная загрузка. Если существующих полей недостаточно, добавить минимальную диагностику и тесты отображения, не перепроектируя UI.
- README: командные гарантии Pause/Resume/Delete, поведение повторного удаления и рестарта, shutdown и пределы отмены in-flight I/O. Документировать внутренние ownership/lock order и generation/reconcile invariants рядом с кодом или отдельным разделом отчёта.

## План Работ И Файлы

1. Повторная проверка F13–F17 на базе фазы 36; таблица владельцев полей, задач и locks. Для уже закрытого участка — доказательство тестом вместо повторного рефакторинга.
2. Детерминированные regression-тесты опасных interleavings с barriers/fake timers до изменений.
3. F13/F14: `internal/engine/engine.go`, `restore.go`, `priority.go`, `resource_manager.go`, `metainfo.go`, `download_gate.go`, `verify_storage.go` и recheck paths.
4. F15: `internal/engine/stream.go`, `stream_source.go` и связанные stream/range tests; generation-aware sessions и единый reconcile.
5. F16: engine task lifecycle, `swarm_watchdog.go`, metadata/recheck/storage tasks и shutdown wiring в `cmd/hub`, если требуется. Проверить все прямые `go`, `AfterFunc` и запуск задач из callbacks.
6. F17: `internal/usecase/torrent_uc.go`, `temporary_files.go`, `temporary.go`, `sync_worker.go`, `internal/repository/cleanup.go` и transaction/delete paths. Добавлять SQLite migration только при необходимости, с upgrade-тестом и сохранением существующих pending manifests.
7. API/UI diagnostics, README, полный regression suite и отчёт `tasks/implementation/phase37-report.md` после реализации. Форк менять только при доказанной необходимости с отдельной проверкой вложенного модуля.

## Тестирование И Приёмочные Сценарии

Использовать barriers/channels и управляемые часы/timers; `Sleep` и многократные прогоны не считать доказательством порядка операций. Проверять журнал side effects и library priorities/admission, а не только конечный state.

| Находка | Обязательные сценарии |
| --- | --- |
| F13 | Pause между snapshot и apply scheduler/metadata/recheck; Resume и изменение priorities во время старого reconcile; Delete/re-add перед callback; ошибка persistence. После точки успешной Pause/Delete нет запрещённого Allow/priority apply старой команды. |
| F14 | Одновременные monitor/add/Resume/metadata passes при лимитах 1 и N; отказ admission и Pause/Delete в момент reservation; повторный Resume; stream-only queued torrent. Максимум слотов проверяется на всём interleaving. |
| F15 | Два refresh с обратным порядком apply; cancel последней сессии одновременно с новой; просроченный debounce; intersecting ranges разных файлов; stream+preload+direct accounting; двойной release; Delete/re-add с поздним cleanup; ошибка reconcile и rollback. |
| F16 | Delete до GotInfo; Close во время recheck/watchdog/storage callback; начало новой задачи одновременно с Close; живой request context при shutdown; уже запущенный timer callback. Все управляемые tasks завершаются, stale callbacks не меняют state, repeated/concurrent Close безопасен. |
| F17 | Ошибки intent commit, Drop, manifest commit, unlink после части файлов и финального DB commit; reopen SQLite/Engine на каждой границе. Pending deletion не скачивается, retry безопасен; оба режима deleteFiles и защита shared/alias paths сохранены. |

- Сохранить regression-тесты фаз 33–36: temporary cleanup, storage safety/migration, verified read/upload, High/Skip restore, sticky DiskFull, writer generation и reader cancellation.
- Repository integration tests — временная SQLite с WAL, реальное reopen и fault injection (например, triggers). Не заполнять физический диск и не зависеть от внешнего swarm.
- Проверки: `go test ./...`, `go test -race ./internal/... ./cmd/...`, `go vet ./...`, `go build ./cmd/hub`, `npm test` и `npm run build` в `web/`, `git diff --check`.
- Затронутые root/storage пакеты форка: `go test . ./storage` и `go test -race . ./storage` из `pkg/anacrolix-torrent`, без исключения обязательных тестов. Ограничения optional libpossum/FUSE описывать отдельно, не выдавать за успешную проверку.
- Дополнительно повторить concurrency regression suite под race; зафиксировать команды, результаты, пропуски и пределы проверок в отчёте.

## Definition Of Done

- [x] F13: команды и side effects согласованы; устаревшие callbacks не отменяют Pause/Delete и не восстанавливают старые priorities/admission.
- [x] F14: все выделения фоновых слотов сериализованы; конкурентные проходы не превышают лимит и не теряют reservations при ошибках.
- [x] F15: reconcile сохраняет объединение актуальных overlays; session/generation tokens защищают от stale refresh, debounce и cleanup после re-add; refcount/debounce сохранены.
- [x] F16: определены ownership и lock order; все engine-owned tasks/timers отменяются и учитываются shutdown, нет регистрации задач после закрытия admission и логических/data races в проверенных путях.
- [x] F17: оба режима ручного удаления имеют безопасный durable recovery; ошибки не приводят к загрузке после рестарта, повтор не удаляет чужие/сохранённые данные.
- [x] Гарантии фаз 34–36 не ослаблены: rooted storage, verified readiness, сохранённые priorities и sticky disk gate действуют для всех новых путей.
- [x] API/UI показывают конфликты и pending deletion без ложного успеха; CORS `*` и совместимость сохранены.
- [x] Детерминированные concurrency/failure/restart тесты, race, vet и сборки проходят; ограничения in-flight I/O и непроверенных окружений явно описаны.
- [x] README обновлён; создан `tasks/implementation/phase37-report.md` с реализацией, результатами каждого пункта DoD и обоснованием отклонений.
