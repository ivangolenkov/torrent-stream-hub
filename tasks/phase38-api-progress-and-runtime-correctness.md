# Фаза 38: Корректность API, Прогресса И Фоновых Операций

## Статус И Зависимости

- Статус: реализовано и проверено; DoD закрыт с описанными в отчёте пределами гарантий. Отчёт: `tasks/implementation/phase38-report.md`.
- Основание: `tasks/draft/codebase-audit-2026-10-21.md`, **F18–F30** (P2).
- Предусловие: завершённый и проверенный DoD фаз 34–37. Сохранить rooted storage, verified readiness, независимые пользовательские priorities, sticky DiskFull, generation-safe reconcile, task admission и durable deletion.
- Ссылки аудита относятся к старому коду. Перед реализацией повторно проверить каждую находку; уже исправленные части закрывать regression-тестами, а не повторным рефакторингом.
- Фаза большая: выполнять внутренние блоки последовательно, с проверкой каждого блока; это одна фаза с общим итоговым DoD.

## Цель И Границы

Сделать ответы API достоверными, команды исполняемыми, прогресс независимым от исторических счётчиков, а polling, SSE, preload и sampling — ограниченными и управляемыми по жизненному циклу.

В scope входят все F18–F30, необходимые изменения Go/SQLite/Vue, миграции совместимости, диагностика и regression-тесты. Смежные frontend-ошибки действий и деление на ноль из раздела 3 аудита входят в части затронутых экранов.

Не входят auth, dependency upgrades, полный actor/FSM rewrite, перенос всех locks из repository, генерация всех API-типов, возврат HardRefresh/Recycle и полный A05/A06. F09/F11/F12 не считать закрытыми этой фазой: preload admission и SSE deadlines решают только соответствующие части нагрузки, не заменяя общий HTTP/security hardening.

## Проверенная Исходная База

- `TorrentAction` всё ещё содержит no-op `recheck` с успешным ответом.
- В фазах 36–37 уже добавлены проверки priority enum/index, persist-before-apply, metadata-pending known-file priorities и атомарный runtime bulk apply. F19 требует проверки end-to-end ошибок, repository `RowsAffected` и политики неизвестных файлов, а не повторного внедрения этих механизмов.
- `torrent_repo.go` сохраняет torrent/file downloaded через `MAX` и собирает один INSERT со всеми файлами. Persisted mapper не восстанавливает всю runtime-модель.
- Scheduler сериализован и защищает slots, но обходит map и проверяет completion по всему payload.
- Recheck уже имеет generation context и task ownership; HTTP-команда должна использовать общий механизм, а не запускать второй независимый verifier.
- TorrServer preload запускает отдельную goroutine на каждый запрос; TTL очищается при обращении к ключу. SSE использует очередь из 10 snapshots и не проверяет write/flush errors.
- `BottomPanel.vue` не очищает pieces timer при unmount и не защищает ответ от смены выбранного hash. Completed-ветка cache обходит fake limit.

## Общие Контракты Перед Изменениями

1. Зафиксировать владельцев desired state, исторической статистики, текущих verified bytes и выполняемых операций. REST, SSE и TorrServer должны использовать согласованные mapper/helpers, не переопределять смысл полей в разных handlers.
2. Новые asynchronous операции принадлежат сервису и поколению торрента. Регистрация атомарна относительно shutdown; Delete/re-add инвалидирует старые callbacks. Не ждать tasks под locks, нужными для их завершения.
3. Сохранить имена endpoint и существующие поля совместимости. Дополнительные operation/validity поля допустимы; изменения смысла progress/downloaded описать в README и тестах TorrServer.
4. Ошибки различать типизированно: неверный ввод — 400; отсутствующий torrent/file — 404; невозможная сейчас операция/metadata pending/storage block — 409; отказ persistence/internal — 500. Для ограничения ресурсов — документированный 429/503. Не возвращать успех для нулевого UPDATE или отклонённой фоновой задачи.
5. SQLite остаётся единственной БД, WAL и per-connection PRAGMA сохраняются. Новую схему вводить миграцией с upgrade-тестом, не переинтерпретировать legacy bytes как проверенные.

## F18: Настоящая Команда Recheck

- Реализовать usecase/API-команду поверх общего engine verification lifecycle. HTTP возвращает 202 только после принятия операции; missing — 404, deletion/migration/несовместимая активность — 409. Удалить no-op успех.
- Добавить наблюдаемый status операции: pending/running/succeeded/failed/cancelled, идентификатор, доступный прогресс и причина ошибки. Не придумывать точный процент, если библиотека его не предоставляет.
- Повторный запрос во время текущей проверки не запускает второй verifier: возвращает ту же операцию. HTTP disconnect после принятия не отменяет сервисную операцию; Delete/Close отменяют её. После рестарта незавершённая операция не отображается как успешно выполненная.
- Проверка не запускает payload download и не снимает Pause/DiskFull. Для Paused использовать неактивное восстановление metadata. Политику активных readers определить консервативно: конфликт вместо проверки одновременно с обслуживанием файла.
- Перед проверкой приостановить download/upload и отозвать прежнюю readiness в проверяемом объёме; во время неё не выдавать старые verified bytes за новый результат. Завершение применяет только актуальное намерение и slot policy, не старый state.

## F19: Полная Валидация Приоритетов

- Проверять присутствие обязательного JSON-поля, enum, index и существование torrent/file до persistence; omitted priority не считать неявным Normal/Skip. Проверять `RowsAffected`, различая идемпотентный повтор и отсутствие записи.
- Для metadata-pending сохранять настройки известных persisted файлов. Неизвестный индекс не принимать «на будущее»: вернуть 409 при неизвестном manifest, 404 при известном manifest без файла.
- Bulk до появления списка файлов не должен успешно теряться: вернуть 409. Для известного списка сохранить транзакционно и применить одной runtime-командой.
- Ошибка persistence не меняет runtime; ошибка runtime apply не стирает committed intent и не открывает gate. Повтор/restore согласует сохранённое намерение. Проверить конфликты с Delete, recheck и lazy restore.

## F20–F21: Persisted Mapper И Честный Прогресс

- Выделить единый persisted → API mapper. Рассчитывать progress из доступных согласованных значений, ограничивать диапазон, обрабатывать size=0 и unknown size без NaN/Infinity.
- Восстанавливать offsets/piece metadata только из достоверного metainfo/сохранённых полей. Не подставлять нулевые значения как доказанно известные; обозначать validity/unknown так, чтобы frontend не показывал ложную карту pieces.
- Разделить исторический transfer counter и текущие verified bytes. `MAX(old,new)` допустим для документированной исторической метрики, но не для текущего состояния payload. Verified значение может уменьшаться после recheck, удаления/повреждения данных.
- Сохранённый verified snapshot после рестарта является last-known, не текущей readiness. До новой проверки не разрешать direct read/upload/download fast path на основании БД. UI явно отличает last-known/unknown от подтверждённого состояния.
- Определить точный смысл torrent/file `downloaded`, `progress` и новых полей для REST/SSE/TorrServer. Legacy downloaded перенести консервативно: не считать его накопленным network traffic без доказательств и не маркировать current verified.
- SyncWorker не возвращает устаревший максимум и не перезаписывает intent priorities, deletion или disk latch. Ошибки migration/save оставляют транзакционно целостные данные.

## F22: Безопасное Чтение Paused После Рестарта

- Использовать единый inactive restore/lazy restore для Paused torrent с сохранённым metainfo, mapping и priorities. Не загружать payload и не включать discovery/upload как побочный эффект открытия.
- Проверять данные в текущем открытии перед direct read; filename, размер, старый progress и Bolt completion недостаточны. Недостающий metainfo — явный диагностируемый конфликт, без неявного получения metadata через сеть.
- Проверенный полный файл обслуживать stream/download по прежним Range/HEAD контрактам, сохраняя Paused. Неполный/повреждённый файл не должен начинать докачку; пользователь должен явно Resume.
- Конкурентные lazy opens используют один runtime generation. Resume/Delete/recheck и старые callbacks не обходят initialization, ownership, deletion или disk gate. Temporary transfer leases сохраняются.

## F23: Один Периодический Speed Sampler

- Один engine-owned sampler обновляет baseline и immutable snapshots со скоростью/временем измерения. GetTorrent/list/BTHealth/watchdog/SSE читают snapshot без изменения baseline.
- Использовать монотонный интервал, обработать первый sample, нулевой интервал и сброс library counters. Не смешивать разные поколения одного hash.
- Watchdog принимает решения по свежим samples; stale/отсутствующий sample не считать доказанной нулевой скоростью. Sampler отменяется и присоединяется при Close.

## F24: Детерминированная Очередь И Selected Completion

- Задать устойчивый порядок фонового admission: persisted FIFO с уникальным tie-breaker, сохраняющийся после рестарта. Рассмотреть существующий `torrent_order` вместо параллельной очереди; temporary eviction semantics не менять побочно.
- Повторный Resume/duplicate add не переставляет запись скрыто. Для новых/legacy записей определить backfill/order и правила re-add; проверить одинаковые timestamps.
- Background completion определяется выбранными Normal/High файлами по проверенным данным. Skip All и завершённая selection не занимают download slot; metadata-pending и recheck reservations учитываются по контракту фазы 37.
- Выбранная selection complete не означает полный torrent/Seeding: частичный payload не объявлять полностью готовым. Добавить отдельный признак/причину ожидания при необходимости, без ложного 100% всего payload.
- Переход Skip → Normal/High повторно делает torrent кандидатом без обхода Pause/DiskFull. Изменение selection, recheck и завершение освобождают/выделяют слот атомарно. Сохранить ноль как запрет background promotion и отдельный stream-driven admission.

## F25: Ограниченный И Дедуплицированный Preload

- Singleflight по runtime generation/file: повторный запрос использует текущий warmup/status, а не создаёт reader, goroutine и transfer lease на каждый вызов.
- Задать конечный общий лимит warmups и ограничение retained entries; конфигурацию/defaults и ответ при насыщении описать явно. Разные targets согласовать без неограниченного роста задач.
- У warmup есть сервисный context, timeout, generation cancellation, учтённый shutdown и ровно один release всех leases/readers. HTTP disconnect не отменяет общую принятую задачу других клиентов.
- Периодический TTL cleanup работает без повторного запроса к ключу; не удаляет ещё работающую задачу без отмены/освобождения ресурсов. Ошибки и отмена не выдаются за completed.
- Delete/re-add не наследует старый preload result. Completed fast path требует verified readiness, а не persisted/file downloaded. Не обходить Paused/DiskFull и не снимать stream refcount/debounce чужих сессий.

## F26: SSE С Backpressure И Shutdown

- Перейти на latest-snapshot mailbox ограниченного размера: медленный клиент получает актуальный snapshot вместо очереди устаревших. Broadcast не блокирует engine/SQLite и других клиентов.
- Проверять write и flush errors, задавать per-write deadline через поддерживаемый HTTP-механизм. Не вводить короткий глобальный WriteTimeout для video streaming.
- SSE handler завершается по request cancellation, write timeout/error и service shutdown. Disconnect/remove/close идемпотентны, нет send-on-closed гонки.
- Определить поведение неподдерживающего deadlines/flushing writer и heartbeat для idle соединений. Новая подписка получает доступный актуальный snapshot; ошибка БД не публикует ложный пустой список.

## F27: Frontend Polling И Устаревшие Ответы

- При unmount, закрытии inspector, смене hash/tab очищать pieces timer и отменять текущий fetch через AbortController.
- Hash + request version проверять перед записью результата; поздний ответ/ошибка старого запроса не изменяет новый torrent. Не запускать перекрывающиеся polls без ограничения.
- Для затронутых Pause/Resume/Delete/Priority/Recheck показывать понятную ошибку, ловить Promise rejection и защищать optimistic state от stale REST/SSE. Нулевой размер не приводит к делению на ноль.
- Дополнить frontend API/types новыми operation/progress полями; unavailable/last-known/rechecking не выглядят как подтверждённая готовность.

## F28–F30: Локальные Исправления

- **F28:** сохранить hash до `UpdateMetadata`; при ошибке не читать nil result, вернуть корректную ошибку без panic. Протестировать poster persistence failure с nil torrent result.
- **F29:** заменить неограниченный multi-row INSERT на prepared statement или ограниченные batches внутри общей транзакции. Не перезаписывать priorities snapshot-значениями; обработать nil file/невалидные данные до записи. Ошибка в середине batch откатывает torrent и все files.
- **F30:** единый cache fake limit для partial/completed веток; `virtual_cache_bytes` не больше непрерывно проверенных bytes, остатка файла и положительного лимита. Зафиксировать семантику нулевого/отрицательного значения через config validation/default, не допускать отрицательного результата. `is_fully_downloaded` не зависит от fake limit и не доверяет старому progress.

## Порядок Работ И Основные Файлы

1. Повторная проверка F18–F30, фиксация API/error/progress/queue contracts и regression fixtures. До миграции определить legacy mapping и значения unknown.
2. **Данные:** F29, F20/F21 и repository-часть F19 — `internal/repository/torrent_repo.go`, loaders/migrations, `internal/models`, `internal/usecase/torrent_uc.go`, `sync_worker.go` и TorrServer mapper. Проверить upgrade/reopen.
3. **Runtime:** F23, F24 — `internal/engine/health.go`, `engine.go`, `resource_manager.go`, priority/download gate paths и watchdog. Сначала независимый sampling, затем queue/selection reconciliation.
4. **Операции:** F22, F18 и оставшаяся F19 — restore/metainfo/storage verification, stream source, usecase и HTTP handlers. Общий inactive restore и единый verifier lifecycle.
5. **Сервисные задачи:** F25/F26 — TorrServer preload coordinator, API SSE, SyncWorker subscription lifecycle и shutdown wiring в `cmd/hub`/router по необходимости.
6. **Интерфейс и совместимость:** F27/F28/F30, Vue store/client/components, cache и тесты; README с семантикой полей, errors, recheck/retry и настройками лимитов.
7. Полный regression suite, обновление `internal/engine/CONCURRENCY.md` при изменении ownership; итоговый отчёт `tasks/implementation/phase38-report.md` с результатами каждого F18–F30 и DoD.

## Обязательные Приёмочные Сценарии

| Находка | Проверки |
| --- | --- |
| F18 | 202 с наблюдаемой операцией; missing/conflict; duplicate singleflight; Paused/active; повреждение снижает verified bytes; Delete/re-add/Close и ошибка verifier/persistence; отсутствие stale Allow/upload/readiness. |
| F19 | omitted/invalid enum/index; missing torrent/file; metadata pending с известным/неизвестным manifest; single/bulk; 0 rows; DB failure до apply; runtime failure после commit; reopen без потери priorities. |
| F20 | DB-only Paused с ненулевыми bytes; single/list/SSE согласованы; offsets/unknown metadata; size=0; last-known после рестарта; finite progress во frontend. |
| F21 | Удаление/подмена payload и recheck уменьшают текущий verified progress; история не становится readiness; legacy migration/reopen; stale SyncWorker snapshot не возвращает старый максимум. |
| F22 | Реальный reopen, Paused verified file + HEAD/Range/download без payload/discovery; incomplete/corrupt/missing metainfo; одновременные opens/Resume/Delete; DiskFull и pending deletion. |
| F23 | Одинаковые counter/time samples при 0/1/100 API reads дают одинаковую скорость; reset/первый sample/stale sample; watchdog не зависит от polling; Close прекращает sampler. |
| F24 | Одинаковый порядок при разных порядках map insertion и после reopen; лимиты 1/N/0; Skip All/selected complete освобождают слот; Skip → Normal; partial не становится полным Seeding; concurrent commands/recheck. |
| F25 | N одинаковых запросов создают один warmup/lease; saturation разными файлами; TTL без обращений; timeout/ошибка/Close; Delete/re-add и поздний callback; completed verified fast path. |
| F26 | Slow/blocking/disconnected writer; write/flush failure; latest вместо backlog; независимый быстрый клиент; shutdown idle stream; subscribe/unsubscribe/broadcast races и неподдерживаемый writer. |
| F27 | Fake timers + deferred fetch: unmount, hash/tab switch, ответы в обратном порядке, abort; нет запросов после unmount; ошибки действий видны; stale REST/SSE не откатывают новую selection. |
| F28 | Fault injection poster update возвращает nil/error: нет panic, корректный ответ и лог с исходным hash. |
| F29 | Torrent с числом файлов выше прежнего SQLite bind limit; bounded batches/prepared statement; failure посередине и rollback; сохранённые High/Skip не перезаписаны. |
| F30 | Partial/completed, limit меньше/больше остатка, offset<0/EOF/beyond EOF, пустой файл и config boundary; после потери verification cache не сообщает старую готовность. |

Использовать управляемые часы, barriers, fake verifier/counters и writers с cancellation/deadline support. Sleep и повторные прогоны не заменяют детерминированных interleavings. Не зависеть от внешнего swarm и не заполнять физический диск.

- Обязательные проверки: `go test ./...`, `go test -race ./internal/... ./cmd/...`, `go vet ./...`, `go build ./cmd/hub`; в `web/` — `npm test`, `npm run build`; `git diff --check`.
- Из `pkg/anacrolix-torrent`: `go test . ./storage` и `go test -race . ./storage`, без исключения обязательных suites. Изменения форка только при доказанной необходимости; optional libpossum/FUSE limitations указать отдельно.
- Повторить concurrency/failure suites под race. Сохранить тесты фаз 33–37, особенно verified read/upload, storage migration/aliases, sticky DiskFull, priorities restore и оба deletion modes.
- Реальные NAS/Smart TV и platform-specific проверки выполнить при наличии окружения; недоступные сценарии явно отметить, не выдавать mock/unit тесты за проверку устройств.

## Definition Of Done

- [x] F18: recheck реально принят/выполнен, статус и ошибки наблюдаемы, lifecycle и gates соблюдены.
- [x] F19: single/bulk priorities валидируются end-to-end, missing/pending не дают ложного успеха, intent не теряется.
- [x] F20: persisted API mapper согласован с runtime, progress finite и unknown metadata явно обозначены.
- [x] F21: текущие verified bytes могут уменьшаться, отделены от истории/last-known и не восстанавливаются старым snapshot.
- [x] F22: Paused после рестарта безопасно читает проверенный полный файл без запуска загрузки/discovery.
- [x] F23: один sampler, API и watchdog не меняют baseline чтением.
- [x] F24: стабильная очередь переживает restart, выбранная completion/Skip All не удерживают слот и не имитируют полный Seeding.
- [x] F25: preload generation-safe, дедуплицирован, ограничен, очищается по TTL и завершается при shutdown.
- [x] F26: SSE latest-only, write/flush errors/deadlines и shutdown обработаны без leaks/races.
- [x] F27: polling прекращается при unmount/switch, старые ответы не меняют новый выбор; ошибки действий отображаются.
- [x] F28: poster persistence failure не вызывает вторичную panic.
- [x] F29: большие списки файлов сохраняются без превышения bind limit, атомарно и без потери priorities.
- [x] F30: cache fake limit одинаков для всех веток и не сообщает непроверенные bytes.
- [x] Миграции и API/TorrServer совместимость проверены; гарантии фаз 34–37, CORS `*` и refcount/debounce сохранены.
- [x] Tests/race/vet/build проходят, limits и непроверенные окружения описаны; README/concurrency contract обновлены.
- [x] Создан `tasks/implementation/phase38-report.md`: изменения, статус каждого пункта DoD, команды/результаты тестов и обоснование отклонений.
