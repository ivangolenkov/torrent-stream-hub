# Фаза 42 — контракт хранения

Статус: блоки 1–7 реализованы; macOS и native Linux проверки прошли, native Windows gate остаётся открытым. Совместимость и platform evidence — в [отчёте](implementation/phase42-report.md).

## Mapping и identity

Существующая `torrent_storage` сохраняется для совместимости journals и aliases. К ней добавляются `storage_id`, `revision`, `prefix`, `name_hint`. `storage_id` — случайная стабильная identity владельца пространства, общая для aliases; не равна hash или `record_id`. Строки aliases несут совместимое представление mapping; изменение расположения атомарно обновляет всю группу. Несогласованные строки одного владельца дают конфликт. Root equality не разрешает присоединить другую torrent identity к тому же владельцу.

Layout 0 — legacy shared; layout 1 — прежний `payload/<hash>`; layout 2 — сохранённый понятный `payload/<name>`. `prefix` явно задаёт снимаемый metadata-префикс, только для layout 2; индекс и metainfo пути неизменны. Путь файла получается исключительно через resolver. `revision` увеличивается при изменении расположения/раскладки, а не при telemetry. `state=reserved` с пустым root разрешает получение metadata, но не открытие payload. `name_hint` хранит явное имя каталога до metadata. Old layouts не конвертируются при SQL upgrade.

Все public revisions — десятичные строки int64. `record_id` защищает delete/re-add, `storage_id` — владельца, `mapping_revision` — расположение, recovery `version` — конкретную операцию. Snapshot epoch/sequence остаются отдельными.

## Команды

`POST /api/v1/torrent/{hash}/storage` использует `action`, `record_id`, `storage_id`, `mapping_revision`. Для применения/повтора/отмены дополнительно требуется `version` из recovery. Планы и анализ сохраняются в существующем `storage_recovery.journal`, без второго независимого разрешающего workflow.

| action | Аргументы | Эффект |
| --- | --- | --- |
| `plan_move` | `folder_name` | План root/layout/source/target; без изменения payload |
| `apply_move` | version | Durable очередь перемещения по сохранённому плану |
| `analyze_location` | `path`, `strip_prefix` | Read-only проверка относительного выделенного каталога |
| `apply_location` | version | Повторная проверка evidence, переключение mapping, paused intent, сохранение старых данных |
| `retry_storage` | version | Продолжить ту же interrupted операцию |
| `cancel_storage` | version | Отмена до публикации; во время/после публикации 409 |
| `reconcile_storage` | identity mapping | Явная проверка ожидаемого тома и inventory; не Resume |

Move/locate несовместимы с pending deletion, legacy recovery, другой mutation и активной миграцией. План не разрешает изменившийся target. Apply повторяется идемпотентно для той же version, не создаёт второй journal. Анализ кандидата не импортирует `.part` и не сохраняет completion. Применение location сохраняет прежние пути в durable retained ownership, не удаляет их при обычном Delete.

Принятая команда: HTTP 202 и текущий `storage`, включая повтор уже применённой операции и reconcile; состояние/результат читается из REST/SSE. Identity/version/ownership/stage conflict: 409. Исчерпание очереди: 429 с retryable code. Shutdown: 503. I/O/SQL failure: 500 с типизированной диагностикой; строки ошибок не являются API-кодами. Повторный stale запрос не изменяет новый record. HTTP cancellation прекращает ожидание, но не durable intent.

Move stages: planned → queued → waiting_readers → prepared → copying (если требуется) → publishing → published → committed → cleanup → completed. `cancel_requested` допускается только до publishing, `cancelled` подтверждается после join owner. Перед rename записывается publishing; ошибка syscall/flush не означает, что namespace остался прежним. Restart исследует обе стороны и продолжает журнал без перезаписи.

## Read model

Torrent получает `storage`: `id`, `revision`, `layout_version`, `root`, `prefix`, `status`, `reason`, необязательный `operation`. Операция содержит kind/stage, source/target, version и разрешённые actions. Эти поля наблюдательные, backend проверяет полномочия заново.

`selection`: `size`, `verified_bytes`, необязательный `progress`, `files`, `complete`, `catalog_version`, строковая `revision` desired intent; пустая выборка не имеет процента. `complete` дополнительно учитывает наблюдаемую доступность полных файлов. Старые `progress`/`downloaded` сохраняют прежний знаменатель и семантику. Для Skip All legacy `selection_complete` продолжает означать отсутствие фоновой работы; новая `selection.files=0` объясняет пустую выборку, а состояние не выдаёт её за Seeding.

Файл получает `metadata_path` (полный metainfo storage path, в отличие от совместимого display `path`) и необязательный `status`: `storage_path`, `presence` (unknown/present/missing/unavailable), `reason`, `verified_bytes`, `last_known_bytes`, `last_known_at_ms`, `last_known_source`, `observed_at_ms`, `verification` (unknown/queued/running/completed/failed/cancelled), `checked_bytes`, `verification_total`, `direct_read_ready`, `read_reason`, `effective_priority`. Completed verification означает завершение прохода, а не полную пригодность файла. Готовность чтения — только наблюдение текущего attachment; transfer повторно проверяет gate. False direct-read не запрещает torrent-reader streaming неполного файла.

Torrent содержит исторические `last_known_bytes/last_known_at_ms/last_known_source`, агрегатный `direct_read_ready` для всех файлов и `verification`: attachment, operation ID явного recheck, status, checked_bytes, total_bytes, queued_pieces, active_pieces. Checked bytes — завершённые попытки hashing, включая несовпавшие pieces; verified bytes — подтверждённый текущий payload. V1 boundary bytes учитываются только в пересечении с файлом, v2 padding не входит в пользовательские totals. Неактивная запись не имеет live verification; её status не выдаёт persisted completion за текущую проверку.

REST/SSE используют единую проекцию. Compact event не включает file catalog. `X-Hub-Details` содержит `record_id/catalog_version/desired_revision/mapping_revision/runtime_attachment`; GUI отсекает ответы прежнего attachment/mapping, а обновление inspection/verification инвалидирует выбранные details. Чтение REST не запускает stat/hash: freshness обновляют background workers и реальные операции открытия.

Move preview содержит `extra_bytes` (верхняя оценка полной копии) и `free_bytes` на ФС target во время планирования. Execution повторяет проверку места с резервом DiskFull; preview не резервирует свободные bytes.

Presence/last-known данные привязаны к record/catalog/mapping и не разрешают чтение. Unknown total отсутствует, известный пустой total равен 0. Zero-length readiness требует безопасного реального открытия. Verified bytes и прогресс проверки — разные счётчики. API read не выполняет inventory/hash.

## Бюджеты и доступность

- Один durable storage worker, не более 64 принятых pending move/locate операций; новая операция сверх лимита получает 429. Startup не создаёт goroutine на каждый record.
- Metadata preparation — очередь до 64 сигналов, один worker; saturation сохраняет reserved record и повторяет попытку через bounded scan, а не теряет metadata или разрешает запись.
- Manifest одной storage operation ограничен 65 536 entries, глубиной 128 и 16 MiB текста относительных путей. Обход читает каталоги порциями по 128 entries, hashing использует buffer 128 KiB. Превышение лимита — видимая ошибка до публикации, без усечения manifest.
- Verification: основной client имеет два hasher slots, один на торрент; offline client — один. Оба используют один физический budget: максимум две активные проверки в процессе, одна на live filesystem identity (`st_dev` / Windows volume serial), 64 pending leases. Ожидание занимает существующий slot, не создаёт второго worker на piece. При saturation обязательная проверка остаётся в slot до освобождения очереди или отмены attachment. Piece bitmap объединяет повторные запросы диапазонов.
- Порядок: active reader, readahead, durable selected files, остальная библиотека/offline. После восьми приоритетных выдач — oldest eligible torrent/job; внутри торрента фоновый cursor циклически проходит queued pieces. Один piece с зависимостями на разных ФС атомарно получает все resource keys. Identity разрешается по открытым rooted directory descriptors вне library lock, не сохраняется между запусками. Уже выданный syscall/lease не прерывается ради seek; смена приоритета видна на следующей границе piece, включая уже ожидающие hasher slots.
- Публикация latest-only с текущим record/version; heartbeat ≤5 секунд без дополнительных I/O/SQL на каждый chunk. Существующие transfer limits и debounce независимы.

Expected volume registration, marker и inventory записываются отдельно от completion. На зарегистрированном storage отсутствие ожидаемого marker закрывает writes/cleanup и не создаёт новый marker автоматически. Первичная регистрация старого непроверенного storage требует reconcile. Переносимость и ограничения доказательства физического носителя документируются; marker не заменяет hash-проверку payload.

Reconcile также разрешает enrollment для interrupted deletion/recovery, если не занят общий mutation ticket. В этом случае не сбрасываются journal/version/deletion intent и не запускается inventory/attachment этого workflow; операция продолжится только по собственным правилам retry/cancel. Это позволяет обновить библиотеку, в которой нет ни одной обычной ready записи. Отсутствующий marker уже зарегистрированного тома по-прежнему не создаётся.

## Следующее отдельное решение: trusted fast resume

В этой фазе persistent completion остаётся disposable hint. Все чтение/upload требуют проверки текущего открытия. Измерения показывают пользу приоритета reader без доверия старым bytes; это не отменяет стоимости полного прохода.

Для fast resume понадобится отдельный протокол: checkpoint metainfo/storage/record/mapping generations и piece set; durable dirty/invalidation **до** новых writes, truncate, replacement и move; flush payload **до** durable completion; checkpoint только после завершения всех writers. Crash, dirty shutdown, несовпадение generation/volume/размера/identity или незавершённый journal должны отменять доверие. Нужны barriers на каждом порядке FS/SQL commit и лимиты checkpoint/restore памяти. WAL commit не заменяет payload fsync.

Обычные mtime/size/inode/volume marker не доказывают отсутствие внешней same-size правки, восстановленных timestamps или клонирования тома. Следующее решение должно явно выбрать доверенную модель внешнего доступа, filesystem change journal/immutable ownership либо повторный hash. Sampling не доказывает целостность непрочитанных pieces. До этого решения никаких shortcuts в readiness нет.
