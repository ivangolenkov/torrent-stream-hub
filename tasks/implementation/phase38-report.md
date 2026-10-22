# Фаза 38: Корректность API, Прогресса И Фоновых Операций

Дата: 2026-10-22. Статус: реализовано и проверено, DoD закрыт в указанном объёме. Commit/push не выполнялись; следующие фазы не начинались.

## Реализованные Блоки

- **F18:** принятая HTTP recheck-команда возвращает 202 и runtime operation ID/status/error. Используется общий singleflight lifecycle с автоматическим recheck; Pause/Delete/Close и читатели учитываются. Во время проверки закрыты download/upload и API readiness, результат не восстанавливает старое намерение. Operation history не сохраняется в SQLite: после рестарта отсутствующая операция не считается успешно выполненной.
- **F19:** typed validation errors для enum/index/missing torrent/file/metadata-pending; обязательный JSON priority, проверка RowsAffected, конфликты storage/deletion. Сохранены persist-before-apply и metadata-pending known-file priorities из фаз 36–37.
- **F20/F21:** миграция `0008_progress_observations.sql`, общий persisted mapper, piece/offset validity. Текущие verified bytes считаются по завершённым проверенным piece ranges, могут уменьшаться. SQLite перестала применять MAX к текущим bytes; observation timestamp защищает от старого snapshot. Старый максимум выделен в `legacy_downloaded`, не выдаётся за cumulative network traffic. DB-only данные помечены `last_known`/`legacy`, не дают readiness; UI показывает provenance.
- **F22:** cached-metainfo inactive restore без tracker/source/webseed fallback. Непроверенный Paused-файл проверяется локально перед открытием; неполный не докачивается. В форке добавлен начальный `DisableNetworking` и явный односторонний `EnableNetworking`, независимый от download gate. Resume возвращает discovery по явной команде.
- **F23:** owned periodic sampler раз в секунду; API/BTHealth/watchdog не изменяют baseline. Нулевой/обратный интервал и counter reset обработаны; watchdog не принимает stale/missing samples за нулевую скорость.
- **F24:** persisted `torrent_order` задаёт FIFO, startup восстанавливает в этом порядке, runtime сортирует кандидатов. Selection completion/Skip All не занимают background slot; частичная selection не становится полным Seeding. При смене priorities выполняется scheduler reconcile. Нулевой background limit и stream-driven admission сохранены.
- **F25:** preload singleflight generation/file, 4 task slots и 128 retained entries, timeout 2 минуты, TTL 10 минут с минутным sweeper. Slots живут до освобождения reader/lease, даже если state старого поколения уже заменён. Только service-owned tasks; HTTP disconnect не отменяет общую задачу. Historical downloaded больше не считается доказательством cache readiness.
- **F26:** latest-only mailbox, idempotent subscription cleanup/Close, write+flush error handling и per-write deadline 10 секунд. Heartbeat 15 секунд; неподдерживающий deadlines/flushing writer получает 503. Shutdown закрывает SSE до HTTP Shutdown, не меняя timeout видео.
- **F27:** pieces polling отменяется при hash/tab switch/unmount; request version и AbortController отвергают поздние ответы, нет overlapping polls. Ошибки действий/приоритетов отображаются, Promise rejection перехватывается, delete dialog остаётся при ошибке. REST не перезаписывает более новый SSE; `command_at_ns`/`snapshot_at_ns` и confirmed priorities защищают от pre-command snapshot, включая совпавшее старое значение. Нулевые проценты файлов finite.
- **F28:** hash сохраняется до poster update, nil/error result не разыменовывается.
- **F29:** prepared file INSERT внутри одной транзакции, предварительная проверка nil/duplicate/invalid файлов, при ошибке rollback. Snapshot не перезаписывает persisted priorities.
- **F30:** completed cache fast path подчиняется fake limit и verified readiness; checking возвращает пустой cache. Нулевой лимит отключает виртуальное окно, отрицательный нормализуется в ноль.

## Проверки

Добавлены repository tests больших списков/rollback, progress ordering/reopen/upgrade и priorities; engine tests verification/progress, sampler/recheck/selection/cache; usecase tests реального reopen Paused/FIFO и latest SSE; API tests priorities/recheck/SSE failures; TorrServer tests poster failure и concurrent preload limits/TTL; fork offline-mode test; frontend polling/store/progress tests.

Все перечисленные ниже проверки успешны. Frontend: **12 тестов** в четырёх файлах, production build успешен. В Go regression suites также сохранены storage/migration, verified read/upload, DiskFull, priority restore, concurrency и deletion проверки предыдущих фаз.

```sh
go test -count=1 -timeout 120s ./...
go test -race -count=1 -timeout 120s ./internal/... ./cmd/...
go test -race -count=3 -run 'TestConcurrency|TestDeletion|TestRecheck|TestFailedRecheck|TestSampler|TestSelection|TestPartialSelected|TestCacheCompleted|TestPreload|TestSSE|TestProgress|TestPausedVerifiedRead|TestQueueOrder|TestPriority|TestLargeFile' -timeout 120s ./internal/engine ./internal/usecase ./internal/repository ./internal/delivery/http/api ./internal/delivery/http/torrserver
go vet ./...
go build -o <approved-temporary-directory>/phase38-hub ./cmd/hub
# pkg/anacrolix-torrent, без -skip:
go test -count=1 -timeout 120s . ./storage
go test -race -count=1 -timeout 120s . ./storage
# web:
npm test
npm run build
# Корень:
git diff --check
```

Root/storage race прогон выполнен также с timeout 120s. Логи в approved temporary directory `.../T/opencode/`: `phase38-final-tests.log`, `phase38-final-race.log`, `phase38-final-repeat.log`, `phase38-fork.log`, `phase38-fork-race.log`. macOS race linker может выдавать `malformed LC_DYSYMTAB`; data races в проверенных путях не обнаружены.

### Что Закреплено Тестами

- Prepared statement сохраняет 6 000 файлов (выше прежнего bind limit); реальный SQLite trigger прерывает середину списка и откатывает parent/files. Nil/invalid/duplicate files отвергаются до записи, High/Skip остаются authoritative.
- Миграция восстанавливается из реальной pre-0008 схемы; legacy bytes/unknown offsets не становятся verified. Реальные reopen сохраняют observed source, metadata и FIFO. Старый snapshot и metadata-pending snapshot не возвращают bytes/не стирают известный размер.
- Реальная замена проверенного payload уменьшает runtime progress; SyncWorker и поздний repository save не возвращают прежний процент. Прежний тест запрета любого уменьшения progress заменён по F21: сохраняется отдельная legacy-оценка, а не ошибочное требование MAX для текущих bytes.
- Recheck проверен на HTTP 202 + operation, 404/409, duplicate singleflight, Pause, verifier failure, Close cancellation. Отдельный regression запрещает старую readiness и Resume после failed verifier. Recheck и read-настроечные команды не подменяют download gate.
- Реальный reopen Engine/SQLite позволяет прочитать Paused-файл и скачать его без networking/download admission. Fork test доказывает исходный offline gate и отказ прямого DHT announce до explicit enable, без изменения payload gate при enable.
- 100 API/health чтений не меняют sampling baseline; проверены delta/elapsed, reverse interval и counter reset. FIFO переживает reopen и Resume младшего кандидата; Skip All, повторное включение selection и действительно частично завершённый payload не занимают лишний слот/не становятся полным Seeding.
- 20 параллельных одинаковых preloads используют один warmup. Проверены capacity 4, TTL через управляемое время без обращения к ключу, error status, lease conflict с Delete, shutdown/join/token release, отказ после Close и owner-check позднего результата.
- SSE publisher оставляет только latest и обслуживает нового подписчика текущим snapshot; concurrent publish/remove/Close безопасны. Handler завершает write/flush failure, blocking deadline-aware writer, idle shutdown; unsupported writer получает 503. В тесте timeout уменьшен, production default 10s.
- Frontend fake timers/deferred fetch проверяют hash switch/unmount/abort, отсутствие overlapping polls и обратный порядок ответов. Store отвергает старый REST после SSE и pre-command SSE с совпавшим старым priority. Ошибки действий/recheck показаны в обеих раскладках; empty/invalid percentages finite.
- Poster update с реальным trigger failure возвращает 500 без nil panic. Completed cache obeys fake limit/remaining bytes/zero; существующие partial/offset/cache suites проходят.

### Основные Новые Файлы

- Engine: `progress.go`, `speed_sampler.go`, `selection.go`, `recheck.go`, `inactive_verify.go`, `progress_test.go`, `phase38_runtime_test.go`.
- Repository: `migrations/0008_progress_observations.sql`, `errors.go`, `progress.go`, `large_files_test.go`, `priority_validation_test.go`, `progress_test.go`.
- Usecase: `inactive_restore.go`, `recheck.go`, `phase38_restore_test.go`, `progress_sync_test.go`, `sse_latest_test.go`.
- HTTP: API `priority_validation_test.go`, `recheck_test.go`, `sse_lifecycle_test.go`; TorrServer `phase38_test.go`.
- Fork: `offline_mode_test.go`, изменения AddTorrentOpts/initial networking gate в `client.go` и одностороннего enable/DHT guard в `torrent.go`.
- Frontend: `web/src/progress.ts`, `web/tests/progress.test.ts`, `BottomPanel.test.ts`, `torrentStore.test.ts`; расширены types/client/store/components и `TorrentTable.test.ts`.

## Решения И Пределы Гарантий

- Сохранены глобальные Lifecycle/admission locks: не выполнен actor/FSM rewrite. Локальная проверка Paused-файла может задержать другие application-команды на медленном диске.
- Offline networking — расширение форка, необходимое для F22; это не закрытие политики private discovery F09. Отключение networking после уже активного discovery не реализовано: новый флаг только для начального открытия и последующего явного включения.
- Повторная recheck-команда singleflight только пока операция активна; последующая команда после завершения создаёт новую операцию. У операции нет выдуманного точного процента и durable history.
- Observation ordering и UI barriers используют время snapshot/ответа в наносекундах при стабильных серверных часах; сохранённое значение не является readiness. Скачки wall clock назад не моделировались, точная сортировка наблюдений при таком скачке не гарантируется. Существующие ограничения внешних writers, filesystem syscall cancellation и SQLite/filesystem границ фаз 34–37 сохраняются.
- Preload limits фиксированы и документируются как defaults реализации, не как общая защита HTTP от перегрузки. Ручной Delete при активной transfer lease остаётся конфликтом по существующей политике; отмена engine generation/Close завершает warmup.
- Реальные NAS/Smart TV, физический ENOSPC/power-cut и все платформы не проверены; optional backend limitations не выдаются за успешную приёмку.

Дополнительно: memory/latency benchmarks больших torrents не проводились. Piece progress сканирует state runs; sampler не означает, что все API чтения стали O(1). Временные лимиты проверены с управляемым writer/verifier, не являются обещанием прерывания произвольного OS syscall. Optional libpossum/FUSE зависимости сохраняют ограничения окружения из отчётов 35–37; production их не использует. Прогресс recheck и история network traffic не выдуманы из устаревших persisted counters.

## Definition Of Done

- [x] F18: исполняемый singleflight recheck с наблюдаемым lifecycle и gates.
- [x] F19: end-to-end priority validation, missing/pending/conflict и сохранение intent.
- [x] F20: согласованный persisted mapper, provenance, known metadata и finite progress.
- [x] F21: уменьшаемые verified bytes, отдельная legacy история, stale snapshot protection.
- [x] F22: безопасное локальное чтение Paused после reopen без скрытого старта сети/докачки.
- [x] F23: один speed sampler, API/watchdog не меняют baseline.
- [x] F24: стабильный FIFO и selected completion без лишнего слота/ложного Seeding.
- [x] F25: bounded generation-safe preload singleflight, TTL и shutdown cleanup.
- [x] F26: latest-only SSE, deadlines/errors и lifecycle подписок.
- [x] F27: lifecycle polling, stale response barriers, видимые ошибки действий.
- [x] F28: poster failure без secondary panic.
- [x] F29: большие списки файлов в одной безопасной транзакции.
- [x] F30: единый fake cache limit и verified readiness.
- [x] Upgrade/API/TorrServer и регрессии фаз 34–37 проходят; CORS/refcount/debounce сохранены.
- [x] Tests/race/vet/build проходят; README/concurrency contract обновлены, пределы проверок указаны.
- [x] Этот отчёт фиксирует реализацию, приёмку, решения и ограничения.
